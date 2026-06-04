package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	internaltls "github.com/user/cli-proxy/internal/tls"
)

const (
	CodexAuthURL      = "https://auth.openai.com/oauth/authorize"
	CodexTokenURL     = "https://auth.openai.com/oauth/token"
	CodexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexRedirectURI  = "http://localhost:1455/auth/callback"
	CodexCallbackPort = 1455
)

type codexTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	IDToken      string `json:"id_token"`
}

// parseJWTPlanType extracts chatgpt_plan_type from a JWT id_token without signature verification.
func ParseJWTPlanType(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	data, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			PlanType string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(data, &claims) == nil && claims.Auth.PlanType != "" {
		return claims.Auth.PlanType
	}
	return ""
}

type CodexOAuth struct {
	store      *TokenStore
	mu         sync.Mutex
	httpClient *http.Client
}

func NewCodexOAuth(store *TokenStore) *CodexOAuth {
	return &CodexOAuth{store: store, httpClient: internaltls.NewAnthropicHTTPClient()}
}

func (o *CodexOAuth) GetTokenData(_ context.Context) *TokenData {
	return o.store.Get("codex")
}

func (o *CodexOAuth) GetToken(ctx context.Context) (string, error) {
	token := o.store.Get("codex")
	if token == nil {
		return "", fmt.Errorf("codex not authenticated (%d accounts), visit /auth/codex to login", len(o.store.AllForProvider("codex")))
	}
	if !token.IsExpired() {
		return token.AccessToken, nil
	}
	return o.refresh(ctx, token)
}

func (o *CodexOAuth) refresh(ctx context.Context, token *TokenData) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	current := o.store.GetByID("codex", token.ID)
	if current != nil && !current.IsExpired() {
		return current.AccessToken, nil
	}

	if token.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token for %s, re-login required", token.ID)
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {CodexClientID},
		"refresh_token": {token.RefreshToken},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", CodexTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("codex refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("codex refresh failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp codexTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse codex refresh response: %w", err)
	}

	newToken := &TokenData{
		Provider:     "codex",
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}
	o.store.Add(newToken)
	fmt.Println("codex token refreshed")
	return newToken.AccessToken, nil
}

func (o *CodexOAuth) StartLogin() (authURL string, err error) {
	pkce, err := GeneratePKCE()
	if err != nil {
		return "", err
	}
	state, err := GenerateState()
	if err != nil {
		return "", err
	}

	params := url.Values{
		"client_id":                  {CodexClientID},
		"response_type":              {"code"},
		"redirect_uri":               {CodexRedirectURI},
		"scope":                      {"openid email profile offline_access"},
		"state":                      {state},
		"code_challenge":             {pkce.CodeChallenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}

	authURL = fmt.Sprintf("%s?%s", CodexAuthURL, params.Encode())

	go o.startCallbackServer(pkce, state)

	return authURL, nil
}

func (o *CodexOAuth) startCallbackServer(pkce *PKCECodes, expectedState string) {
	mux := http.NewServeMux()
	var srv *http.Server

	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
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

		token, err := o.exchangeCode(r.Context(), code, pkce.CodeVerifier)
		if err != nil {
			fmt.Fprintf(w, "<h2>Login Failed</h2><p>%s</p>", err.Error())
			go func() { time.Sleep(time.Second); srv.Close() }()
			return
		}

		o.store.Add(token)
		fmt.Println("codex authenticated")
		fmt.Fprintf(w, "<h2>Codex Login Successful</h2><p>You can close this window.</p>")
		go func() { time.Sleep(2 * time.Second); srv.Close() }()
	})

	srv = &http.Server{
		Addr:    fmt.Sprintf(":%d", CodexCallbackPort),
		Handler: mux,
	}

	fmt.Printf("waiting for codex OAuth callback on port %d...\n", CodexCallbackPort)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Printf("codex callback server error: %v\n", err)
	}
}

func (o *CodexOAuth) exchangeCode(ctx context.Context, code, codeVerifier string) (*TokenData, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {CodexClientID},
		"code":          {code},
		"redirect_uri":  {CodexRedirectURI},
		"code_verifier": {codeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", CodexTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp codexTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}

	td := &TokenData{
		Provider:     "codex",
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}

	// Extract plan_type from id_token and seed quota cache
	if tokenResp.IDToken != "" {
		if pt := ParseJWTPlanType(tokenResp.IDToken); pt != "" {
			QuotaCache.Set("codex:"+td.ID, &QuotaInfo{
				AccountID: td.ID,
				PlanType:  pt,
			})
			fmt.Printf("codex plan: %s\n", pt)
		}
	}

	return td, nil
}

const codexBaseURL = "https://chatgpt.com/backend-api"

const codexUA = "codex-tui/0.135.0 (Mac OS 26.5.0; arm64)"

func applyCodexHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codexUA)
	req.Header.Set("Accept-Encoding", "identity")
}

// FetchModels fetches available models and returns a cookie-warmed client for subsequent calls.
func (o *CodexOAuth) FetchModels(ctx context.Context) ([]ModelInfo, *http.Client, error) {
	token, err := o.GetToken(ctx)
	if err != nil {
		return nil, nil, err
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Transport: o.httpClient.Transport}

	req, _ := http.NewRequestWithContext(ctx, "GET", codexBaseURL+"/codex/models?client_version=0.135.0", nil)
	applyCodexHeaders(req, token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	// Try to extract quota from model response headers
	if quota := ParseCodexRateLimitHeaders(resp.Header); quota != nil {
		QuotaCache.Set("codex", quota)
	}

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("fetch codex models: %d %s", resp.StatusCode, string(body))
	}

	var result struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		var catResult struct {
			Categories []struct {
				Models []struct {
					Slug        string `json:"slug"`
					DisplayName string `json:"display_name"`
					Description string `json:"description"`
				} `json:"models"`
			} `json:"categories"`
		}
		if err2 := json.Unmarshal(body, &catResult); err2 == nil {
			var models []ModelInfo
			for _, cat := range catResult.Categories {
				for _, m := range cat.Models {
					models = append(models, ModelInfo{Slug: m.Slug, DisplayName: m.DisplayName, Description: m.Description})
				}
			}
			return models, client, nil
		}
		return nil, nil, err
	}

	models := make([]ModelInfo, len(result.Models))
	for i, m := range result.Models {
		models[i] = ModelInfo{Slug: m.Slug, DisplayName: m.DisplayName, Description: m.Description}
	}
	return models, client, nil
}

// FetchQuotaWithClient fetches per-account quota using a provided http.Client with CF cookies.
func (o *CodexOAuth) FetchQuotaWithClient(ctx context.Context, client *http.Client, accountID string) (*QuotaInfo, error) {
	token, err := o.GetToken(ctx)
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", codexBaseURL+"/codex/usage", nil)
	applyCodexHeaders(req, token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch codex usage: %d", resp.StatusCode)
	}

	var raw struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Allowed      bool `json:"allowed"`
			LimitReached bool `json:"limit_reached"`
			PrimaryWindow *struct {
				UsedPercent       float64 `json:"used_percent"`
				LimitWindowSecs   float64 `json:"limit_window_seconds"`
				ResetAfterSecs    float64 `json:"reset_after_seconds"`
				ResetAt           float64 `json:"reset_at"`
			} `json:"primary_window"`
			SecondaryWindow *struct {
				UsedPercent       float64 `json:"used_percent"`
				LimitWindowSecs   float64 `json:"limit_window_seconds"`
				ResetAfterSecs    float64 `json:"reset_after_seconds"`
				ResetAt           float64 `json:"reset_at"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
		AdditionalRateLimits []struct {
			LimitName string `json:"limit_name"`
			RateLimit *struct {
				PrimaryWindow *struct {
					UsedPercent float64 `json:"used_percent"`
					ResetAt     float64 `json:"reset_at"`
					LimitWindowSecs float64 `json:"limit_window_seconds"`
				} `json:"primary_window"`
			} `json:"rate_limit"`
		} `json:"additional_rate_limits"`
		Credits *struct {
			HasCredits bool   `json:"has_credits"`
			Unlimited  bool   `json:"unlimited"`
			Balance    string `json:"balance"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	info := &QuotaInfo{
		AccountID: accountID,
		PlanType:  raw.PlanType,
		FetchedAt: time.Now().Format("01/02 15:04"),
	}

	if pw := raw.RateLimit.PrimaryWindow; pw != nil {
		info.Primary = &RateWindow{
			Label:        windowLabel(int(pw.LimitWindowSecs / 60)),
			UsedPercent:  math.Round(pw.UsedPercent*100) / 100,
			LimitReached: pw.UsedPercent >= 100 || raw.RateLimit.LimitReached,
			ResetAt:      formatResetAt(pw.ResetAt),
		}
	}

	if sw := raw.RateLimit.SecondaryWindow; sw != nil {
		info.Secondary = &RateWindow{
			Label:        windowLabel(int(sw.LimitWindowSecs / 60)),
			UsedPercent:  math.Round(sw.UsedPercent*100) / 100,
			LimitReached: sw.UsedPercent >= 100,
			ResetAt:      formatResetAt(sw.ResetAt),
		}
	}

	for _, arl := range raw.AdditionalRateLimits {
		if arl.RateLimit != nil && arl.RateLimit.PrimaryWindow != nil {
			pw := arl.RateLimit.PrimaryWindow
			info.Additional = append(info.Additional, AdditionalRL{
				Name: arl.LimitName,
				Primary: &RateWindow{
					Label:        arl.LimitName + " " + windowLabel(int(pw.LimitWindowSecs/60)),
					UsedPercent:  math.Round(pw.UsedPercent*100) / 100,
					LimitReached: pw.UsedPercent >= 100,
					ResetAt:      formatResetAt(pw.ResetAt),
				},
			})
		}
	}

	if raw.Credits != nil {
		info.Credits = &Credits{
			HasCredits: raw.Credits.HasCredits,
			Unlimited:  raw.Credits.Unlimited,
			Balance:    raw.Credits.Balance,
		}
	}

	return info, nil
}

// FetchAllQuotas fetches quota for all active Codex accounts.
func (o *CodexOAuth) FetchAllQuotas(ctx context.Context) {
	accounts := o.store.AllForProvider("codex")
	for _, acc := range accounts {
		if acc.IsExpired() {
			continue
		}

		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, Transport: o.httpClient.Transport}

		// Warmup with models endpoint
		warmupReq, _ := http.NewRequestWithContext(ctx, "GET", codexBaseURL+"/codex/models?client_version=0.135.0", nil)
		warmupReq.Header.Set("Authorization", "Bearer "+acc.AccessToken)
		warmupReq.Header.Set("Accept", "application/json")
		warmupReq.Header.Set("User-Agent", codexUA)
		if warmupResp, err := client.Do(warmupReq); err == nil {
			io.ReadAll(warmupResp.Body)
			warmupResp.Body.Close()
		}

		// Fetch usage
		usageReq, _ := http.NewRequestWithContext(ctx, "GET", codexBaseURL+"/codex/usage", nil)
		usageReq.Header.Set("Authorization", "Bearer "+acc.AccessToken)
		usageReq.Header.Set("Accept", "application/json")
		usageReq.Header.Set("User-Agent", codexUA)

		resp, err := client.Do(usageReq)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}

		// Parse plan_type from JWT
		planType := ParseJWTPlanType(acc.AccessToken)

		var raw struct {
			PlanType string `json:"plan_type"`
		}
		json.Unmarshal(body, &raw)
		if raw.PlanType != "" {
			planType = raw.PlanType
		}

		// Reparse full quota using FetchQuotaWithClient-style parsing
		info := &QuotaInfo{AccountID: acc.ID, PlanType: planType, FetchedAt: time.Now().Format("01/02 15:04")}
		parseUsageBody(body, info)
		QuotaCache.Set("codex:"+acc.ID, info)
	}
}

func parseUsageBody(body []byte, info *QuotaInfo) {
	var raw struct {
		RateLimit struct {
			LimitReached bool `json:"limit_reached"`
			PrimaryWindow *struct {
				UsedPercent     float64 `json:"used_percent"`
				LimitWindowSecs float64 `json:"limit_window_seconds"`
				ResetAt         float64 `json:"reset_at"`
			} `json:"primary_window"`
			SecondaryWindow *struct {
				UsedPercent     float64 `json:"used_percent"`
				LimitWindowSecs float64 `json:"limit_window_seconds"`
				ResetAt         float64 `json:"reset_at"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
		AdditionalRateLimits []struct {
			LimitName string `json:"limit_name"`
			RateLimit *struct {
				PrimaryWindow *struct {
					UsedPercent     float64 `json:"used_percent"`
					ResetAt         float64 `json:"reset_at"`
					LimitWindowSecs float64 `json:"limit_window_seconds"`
				} `json:"primary_window"`
			} `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return
	}
	if pw := raw.RateLimit.PrimaryWindow; pw != nil {
		info.Primary = &RateWindow{
			Label:        windowLabel(int(pw.LimitWindowSecs / 60)),
			UsedPercent:  math.Round(pw.UsedPercent*100) / 100,
			LimitReached: pw.UsedPercent >= 100 || raw.RateLimit.LimitReached,
			ResetAt:      formatResetAt(pw.ResetAt),
		}
	}
	if sw := raw.RateLimit.SecondaryWindow; sw != nil {
		info.Secondary = &RateWindow{
			Label:        windowLabel(int(sw.LimitWindowSecs / 60)),
			UsedPercent:  math.Round(sw.UsedPercent*100) / 100,
			LimitReached: sw.UsedPercent >= 100,
			ResetAt:      formatResetAt(sw.ResetAt),
		}
	}
	for _, arl := range raw.AdditionalRateLimits {
		if arl.RateLimit != nil && arl.RateLimit.PrimaryWindow != nil {
			pw := arl.RateLimit.PrimaryWindow
			info.Additional = append(info.Additional, AdditionalRL{
				Name: arl.LimitName,
				Primary: &RateWindow{
					Label:        arl.LimitName,
					UsedPercent:  math.Round(pw.UsedPercent*100) / 100,
					LimitReached: pw.UsedPercent >= 100,
					ResetAt:      formatResetAt(pw.ResetAt),
				},
			})
		}
	}
}
