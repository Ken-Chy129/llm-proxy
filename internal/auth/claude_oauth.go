package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	internaltls "github.com/Ken-Chy129/llm-proxy/internal/tls"
)

const (
	ClaudeAuthURL      = "https://claude.com/cai/oauth/authorize"
	ClaudeTokenURL     = "https://platform.claude.com/v1/oauth/token"
	ClaudeClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	ClaudeRedirectURI  = "https://platform.claude.com/oauth/code/callback"
	ClaudeCallbackPort = 54545
)

type claudeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
	Account struct {
		UUID  string `json:"uuid"`
		Email string `json:"email_address"`
	} `json:"account"`
}

type pendingClaudeOAuth struct {
	pkce  *PKCECodes
	state string
}

type ClaudeOAuth struct {
	store      *TokenStore
	httpClient *http.Client
	mu         sync.Mutex
	ServerPort int
	pending    *pendingClaudeOAuth
	pendingMu  sync.Mutex
}

func NewClaudeOAuth(store *TokenStore) *ClaudeOAuth {
	return &ClaudeOAuth{
		store:      store,
		httpClient: internaltls.NewAnthropicHTTPClient(),
	}
}

// Store exposes the underlying token store (for rate-limit failover bookkeeping).
func (o *ClaudeOAuth) Store() *TokenStore { return o.store }

func (o *ClaudeOAuth) GetToken(ctx context.Context) (string, error) {
	token, _, err := o.GetTokenWithAccount(ctx, "")
	return token, err
}

// GetTokenWithAccount returns an access token together with the ID of the
// account it belongs to, so callers can attribute rate-limit failures back to a
// specific account. model scopes selection so an account cooling down only for
// that model is skipped for it but still used for others; pass "" if unknown.
func (o *ClaudeOAuth) GetTokenWithAccount(ctx context.Context, model string) (string, string, error) {
	token := o.store.Get("claude", model)
	if token == nil {
		return "", "", fmt.Errorf("claude not authenticated (%d accounts), visit /auth/claude to login", len(o.store.AllForProvider("claude")))
	}
	id := token.ID
	if !token.IsExpired() {
		return token.AccessToken, id, nil
	}
	access, err := o.refresh(ctx, token)
	return access, id, err
}

func (o *ClaudeOAuth) refresh(ctx context.Context, token *TokenData) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Double-check: maybe another goroutine refreshed this specific account
	current := o.store.GetByID("claude", token.ID)
	if current != nil && !current.IsExpired() {
		return current.AccessToken, nil
	}

	if token.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token for %s, re-login required", token.ID)
	}

	reqBody := map[string]interface{}{
		"client_id":     ClaudeClientID,
		"grant_type":    "refresh_token",
		"refresh_token": token.RefreshToken,
	}
	jsonBody, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", ClaudeTokenURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp claudeTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse refresh response: %w", err)
	}

	newToken := &TokenData{
		Provider:         "claude",
		AccessToken:      tokenResp.AccessToken,
		RefreshToken:     tokenResp.RefreshToken,
		Email:            tokenResp.Account.Email,
		OrganizationID:   tokenResp.Organization.UUID,
		OrganizationName: tokenResp.Organization.Name,
		ExpiresAt:        time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}
	o.store.Add(newToken)
	fmt.Printf("claude token refreshed for %s\n", newToken.Email)
	return newToken.AccessToken, nil
}

func (o *ClaudeOAuth) StartLogin() (authURL string, err error) {
	pkce, err := GeneratePKCE()
	if err != nil {
		return "", err
	}
	state, err := GenerateState()
	if err != nil {
		return "", err
	}

	params := url.Values{
		"code":                  {"true"},
		"client_id":             {ClaudeClientID},
		"response_type":         {"code"},
		"redirect_uri":          {ClaudeRedirectURI},
		"scope":                 {"org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"},
		"code_challenge":        {pkce.CodeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}

	authURL = fmt.Sprintf("%s?%s", ClaudeAuthURL, params.Encode())

	o.pendingMu.Lock()
	o.pending = &pendingClaudeOAuth{pkce: pkce, state: state}
	o.pendingMu.Unlock()

	go o.startCallbackServer(pkce, state)

	return authURL, nil
}

// ExchangeCallbackURL exchanges a pasted OAuth callback URL or raw code for tokens.
// Accepts either a full callback URL (https://...?code=...&state=...) or a raw
// authentication code in "code#state" format as shown on the platform success page.
func (o *ClaudeOAuth) ExchangeCallbackURL(ctx context.Context, callbackURL string) (*TokenData, error) {
	o.pendingMu.Lock()
	pending := o.pending
	o.pendingMu.Unlock()

	if pending == nil {
		return nil, fmt.Errorf("no pending OAuth flow, click 'Add Account' first to get the auth URL")
	}

	var code, state string

	input := strings.TrimSpace(callbackURL)
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		parsed, err := url.Parse(input)
		if err != nil {
			return nil, fmt.Errorf("invalid URL: %w", err)
		}
		code = parsed.Query().Get("code")
		state = parsed.Query().Get("state")
		errParam := parsed.Query().Get("error")
		if errParam != "" {
			return nil, fmt.Errorf("OAuth error: %s", errParam)
		}
		if idx := strings.Index(code, "#"); idx >= 0 {
			code = code[:idx]
		}
	} else {
		if idx := strings.Index(input, "#"); idx >= 0 {
			code = input[:idx]
			state = input[idx+1:]
		} else {
			code = input
			state = pending.state
		}
	}

	if code == "" {
		return nil, fmt.Errorf("no authorization code found")
	}

	if state != pending.state {
		return nil, fmt.Errorf("state mismatch: please use the code from the most recent login attempt")
	}

	token, err := o.exchangeCode(ctx, code, pending.pkce.CodeVerifier, state)
	if err != nil {
		return nil, err
	}

	o.store.Add(token)

	o.pendingMu.Lock()
	o.pending = nil
	o.pendingMu.Unlock()

	fmt.Printf("claude authenticated via callback URL: %s\n", token.Email)
	return token, nil
}

func (o *ClaudeOAuth) startCallbackServer(pkce *PKCECodes, expectedState string) {
	mux := http.NewServeMux()
	var srv *http.Server

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		errParam := r.URL.Query().Get("error")

		if errParam != "" {
			fmt.Fprintf(w, "<h2>Login Failed</h2><p>%s</p>", errParam)
			go func() { time.Sleep(time.Second); srv.Close() }()
			return
		}

		if state != expectedState {
			fmt.Fprintf(w, "<h2>Login Failed</h2><p>State mismatch</p>")
			go func() { time.Sleep(time.Second); srv.Close() }()
			return
		}

		// Handle code#state format
		if idx := strings.Index(code, "#"); idx >= 0 {
			code = code[:idx]
		}

		token, err := o.exchangeCode(r.Context(), code, pkce.CodeVerifier, expectedState)
		if err != nil {
			fmt.Fprintf(w, "<h2>Login Failed</h2><p>%s</p>", err.Error())
			go func() { time.Sleep(time.Second); srv.Close() }()
			return
		}

		o.store.Add(token)
		fmt.Printf("claude authenticated: %s\n", token.Email)
		http.Redirect(w, r, fmt.Sprintf("http://localhost:%d/", o.ServerPort), http.StatusTemporaryRedirect)
		go func() { time.Sleep(2 * time.Second); srv.Close() }()
	})

	srv = &http.Server{
		Addr:    fmt.Sprintf(":%d", ClaudeCallbackPort),
		Handler: mux,
	}

	fmt.Printf("waiting for claude OAuth callback on port %d...\n", ClaudeCallbackPort)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Printf("callback server error: %v\n", err)
	}
}

func (o *ClaudeOAuth) exchangeCode(ctx context.Context, code, codeVerifier, state string) (*TokenData, error) {
	reqBody := map[string]interface{}{
		"code":          code,
		"grant_type":    "authorization_code",
		"client_id":     ClaudeClientID,
		"redirect_uri":  ClaudeRedirectURI,
		"code_verifier": codeVerifier,
		"state":         state,
	}
	jsonBody, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", ClaudeTokenURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp claudeTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}

	return &TokenData{
		Provider:         "claude",
		AccessToken:      tokenResp.AccessToken,
		RefreshToken:     tokenResp.RefreshToken,
		Email:            tokenResp.Account.Email,
		OrganizationID:   tokenResp.Organization.UUID,
		OrganizationName: tokenResp.Organization.Name,
		ExpiresAt:        time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

// --- Claude account usage / quota -------------------------------------------

// Claude exposes OAuth-scoped account usage at api.anthropic.com. These are the
// same endpoints the Claude Code CLI calls; they require the oauth beta header
// and accept the sk-ant-oat* access token as a bearer.
const (
	claudeAPIBaseURL     = "https://api.anthropic.com"
	claudeOAuthBetaValue = "oauth-2025-04-20"
)

func applyClaudeUsageHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", claudeOAuthBetaValue)
	req.Header.Set("User-Agent", "claude-cli/2.1.181 (external, cli)")
}

// FetchAllQuotas fetches Claude account usage limits for every active account.
func (o *ClaudeOAuth) FetchAllQuotas(ctx context.Context) {
	for _, acc := range o.store.AllForProvider("claude") {
		if acc.IsExpired() {
			if _, err := o.refresh(ctx, acc); err != nil {
				continue
			}
			acc = o.store.GetByID("claude", acc.ID)
			if acc == nil {
				continue
			}
		}
		o.fetchQuotaForAccount(ctx, acc)
	}
}

// FetchQuotaForAccountByID fetches Claude usage limits for a single account.
func (o *ClaudeOAuth) FetchQuotaForAccountByID(ctx context.Context, accountID string) error {
	acc := o.store.GetByID("claude", accountID)
	if acc == nil {
		return fmt.Errorf("account %s not found", accountID)
	}
	if acc.IsExpired() {
		if _, err := o.refresh(ctx, acc); err != nil {
			return err
		}
		acc = o.store.GetByID("claude", accountID)
		if acc == nil {
			return fmt.Errorf("account %s not found after refresh", accountID)
		}
	}
	return o.fetchQuotaForAccount(ctx, acc)
}

func (o *ClaudeOAuth) fetchQuotaForAccount(ctx context.Context, acc *TokenData) error {
	email := acc.Email
	planType := strings.TrimSpace(acc.OrganizationName)

	// Best-effort: enrich the plan label + organization from the profile endpoint.
	if orgName, plan, err := o.fetchClaudeProfile(ctx, acc); err == nil {
		if plan != "" {
			planType = plan
		} else if orgName != "" {
			planType = orgName
		}
	}
	if planType == "" {
		planType = "Claude"
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, claudeAPIBaseURL+"/api/oauth/usage", nil)
	applyClaudeUsageHeaders(req, acc.AccessToken)
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return o.cacheEmptyQuota(acc, email, planType, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return o.cacheEmptyQuota(acc, email, planType, fmt.Errorf("fetch claude usage: %d", resp.StatusCode))
	}
	info, err := ParseClaudeUsageLimits(body)
	if err != nil {
		return o.cacheEmptyQuota(acc, email, planType, err)
	}
	info.AccountID = acc.ID
	info.Email = email
	info.PlanType = planType
	info.HasRealData = true
	QuotaCache.Set("claude:"+acc.ID, info)
	return nil
}

// cacheEmptyQuota records a placeholder so the dashboard still lists the account
// when a usage fetch fails — but never clobbers previously-fetched real data.
func (o *ClaudeOAuth) cacheEmptyQuota(acc *TokenData, email, planType string, cause error) error {
	if existing := QuotaCache.Get("claude:" + acc.ID); existing != nil && existing.HasRealData {
		return cause
	}
	QuotaCache.Set("claude:"+acc.ID, &QuotaInfo{AccountID: acc.ID, Email: email, PlanType: planType, HasRealData: false})
	return cause
}

// fetchClaudeProfile returns (organizationName, planLabel) and backfills the
// organization id/name onto the token when they are missing or stale.
func (o *ClaudeOAuth) fetchClaudeProfile(ctx context.Context, acc *TokenData) (string, string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, claudeAPIBaseURL+"/api/oauth/profile", nil)
	applyClaudeUsageHeaders(req, acc.AccessToken)
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch claude profile: %d", resp.StatusCode)
	}
	var profile struct {
		Account struct {
			HasClaudeMax bool `json:"has_claude_max"`
			HasClaudePro bool `json:"has_claude_pro"`
		} `json:"account"`
		Organization struct {
			UUID          string `json:"uuid"`
			Name          string `json:"name"`
			RateLimitTier string `json:"rate_limit_tier"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		return "", "", err
	}
	org := profile.Organization
	if org.UUID != "" && (acc.OrganizationID != org.UUID || acc.OrganizationName != org.Name) {
		acc.OrganizationID = org.UUID
		acc.OrganizationName = org.Name
		o.store.Add(acc)
	}
	plan := claudePlanLabel(org.RateLimitTier, profile.Account.HasClaudeMax, profile.Account.HasClaudePro)
	return org.Name, plan, nil
}

func claudePlanLabel(tier string, hasMax, hasPro bool) string {
	t := strings.ToLower(tier)
	switch {
	case strings.Contains(t, "max_20x"):
		return "Max 20x"
	case strings.Contains(t, "max_5x"):
		return "Max 5x"
	case strings.Contains(t, "max"):
		return "Max"
	case strings.Contains(t, "pro"):
		return "Pro"
	case hasMax:
		return "Max"
	case hasPro:
		return "Pro"
	}
	return ""
}

// ParseClaudeUsageLimits converts the api.anthropic.com /api/oauth/usage payload
// into the shared quota shape. The normalized "limits" array is preferred; the
// named top-level windows (five_hour, seven_day, seven_day_opus, …) are the
// fallback when "limits" is absent.
func ParseClaudeUsageLimits(body []byte) (*QuotaInfo, error) {
	var usage struct {
		Limits []struct {
			Kind     string  `json:"kind"`
			Group    string  `json:"group"`
			Percent  float64 `json:"percent"`
			Severity string  `json:"severity"`
			ResetsAt string  `json:"resets_at"`
		} `json:"limits"`
		FiveHour       *claudeUsageWindow `json:"five_hour"`
		SevenDay       *claudeUsageWindow `json:"seven_day"`
		SevenDayOpus   *claudeUsageWindow `json:"seven_day_opus"`
		SevenDaySonnet *claudeUsageWindow `json:"seven_day_sonnet"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, err
	}

	var windows []*RateWindow
	if len(usage.Limits) > 0 {
		for _, l := range usage.Limits {
			display, unix := parseClaudeReset(l.ResetsAt)
			windows = append(windows, &RateWindow{
				Label:            claudeLimitLabel(l.Kind, l.Group),
				RemainingPercent: remainingPercent(l.Percent),
				// Only a full window (percent >= 100) counts as reached. Anthropic
				// flags severity "critical" while the account is still usable (it's
				// a near-limit warning, not a refusal), so critical must NOT bench
				// the account — otherwise the last ~10% of the window is wasted and
				// the "limited" badge fires early. See RateWindow.Exhausted.
				LimitReached: l.Percent >= 100,
				ResetAt:          display,
				ResetUnix:        unix,
			})
		}
	} else {
		for _, w := range []struct {
			key string
			win *claudeUsageWindow
		}{
			{"five_hour", usage.FiveHour},
			{"seven_day", usage.SevenDay},
			{"seven_day_opus", usage.SevenDayOpus},
			{"seven_day_sonnet", usage.SevenDaySonnet},
		} {
			if w.win == nil {
				continue
			}
			display, unix := parseClaudeReset(w.win.ResetsAt)
			windows = append(windows, &RateWindow{
				Label:            claudeLimitLabel(w.key, ""),
				RemainingPercent: remainingPercent(w.win.Utilization),
				LimitReached:     w.win.Utilization >= 100,
				ResetAt:          display,
				ResetUnix:        unix,
			})
		}
	}

	if len(windows) == 0 {
		return nil, fmt.Errorf("no usage windows in claude usage response")
	}

	// Assign windows by semantic role, not by the order the API returns them:
	// Primary = the 5h session window, Secondary = the all-models weekly window.
	// These two are the only account-wide binding limits, so the "limited" badge
	// (admin handler) and quota-aware selection (token_store) key off them alone.
	// Model-specific weekly limits — Opus/Sonnet weekly and the Fable "Weekly
	// limit" — are informational and go to Additional: hitting one must never
	// bench the whole account, only the all-models weekly does.
	info := &QuotaInfo{}
	for _, w := range windows {
		switch {
		case w.Label == labelClaudeSession && info.Primary == nil:
			info.Primary = w
		case w.Label == labelClaudeWeeklyAll && info.Secondary == nil:
			info.Secondary = w
		default:
			info.Additional = append(info.Additional, AdditionalRL{Name: w.Label, Primary: w})
		}
	}
	return info, nil
}

type claudeUsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

func remainingPercent(usedPercent float64) float64 {
	remaining := 100 - usedPercent
	if remaining < 0 {
		remaining = 0
	}
	return math.Round(remaining*100) / 100
}

// labelClaudeSession and labelClaudeWeeklyAll are the canonical labels for the
// two account-wide binding windows. ParseClaudeUsageLimits matches on these to
// pin them to Primary/Secondary regardless of API ordering, so keep them in sync
// with claudeLimitLabel.
const (
	labelClaudeSession   = "Current session (5h)"
	labelClaudeWeeklyAll = "Weekly (all models)"
)

func claudeLimitLabel(kind, group string) string {
	switch strings.ToLower(kind) {
	case "session", "five_hour":
		return labelClaudeSession
	case "weekly_all", "seven_day":
		return labelClaudeWeeklyAll
	case "weekly_opus", "seven_day_opus":
		return "Opus weekly"
	case "weekly_sonnet", "seven_day_sonnet":
		return "Sonnet weekly"
	}
	lower := strings.ToLower(kind)
	if strings.Contains(lower, "opus") {
		return "Opus weekly"
	}
	if strings.Contains(lower, "sonnet") {
		return "Sonnet weekly"
	}
	if group == "weekly" {
		return "Weekly limit"
	}
	if kind != "" {
		return kind
	}
	return "Usage limit"
}

// parseClaudeReset returns the display string ("01/02 15:04", local) and the
// machine-readable unix seconds for a Claude RFC3339 reset timestamp. unix is 0
// when the input is empty or unparseable.
func parseClaudeReset(s string) (string, int64) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("01/02 15:04"), t.Unix()
	}
	return s, 0
}
