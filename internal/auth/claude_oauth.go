package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	internaltls "github.com/user/cli-proxy/internal/tls"
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

func (o *ClaudeOAuth) GetToken(ctx context.Context) (string, error) {
	token := o.store.Get("claude")
	if token == nil {
		return "", fmt.Errorf("claude not authenticated (%d accounts), visit /auth/claude to login", len(o.store.AllForProvider("claude")))
	}
	if !token.IsExpired() {
		return token.AccessToken, nil
	}
	return o.refresh(ctx, token)
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
		Provider:     "claude",
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		Email:        tokenResp.Account.Email,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
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
		Provider:     "claude",
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		Email:        tokenResp.Account.Email,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}
