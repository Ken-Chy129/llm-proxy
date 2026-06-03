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

type CodexOAuth struct {
	store *TokenStore
	mu    sync.Mutex
}

func NewCodexOAuth(store *TokenStore) *CodexOAuth {
	return &CodexOAuth{store: store}
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

	resp, err := http.DefaultClient.Do(req)
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

	resp, err := http.DefaultClient.Do(req)
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

	return &TokenData{
		Provider:     "codex",
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}
