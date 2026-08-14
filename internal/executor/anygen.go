package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

const (
	defaultAnyGenBaseURL   = "https://www.anygen.io/v1/openapi/anyclaw/app/appg4oo4fl2ay7g2u7my4eaqzy/api/v1"
	defaultAnyGenAPIKeyEnv = "ANYGEN_LLM_KEY"
	defaultAnyGenVerifyURL = "https://www.anygen.io/v1/openapi/key/verify"
)

// AnyGenCredits is returned by AnyGen's platform-native key verification API.
// Credits is a string on the wire, so it stays a string here to avoid silently
// losing precision or rejecting a future non-integer representation.
type AnyGenCredits struct {
	Verified bool   `json:"verified"`
	UserID   string `json:"user_id"`
	Credits  string `json:"credits"`
}

// AnyGenExecutor calls the app-scoped OpenAI-compatible AnyGen API. Its app
// currently authorizes only chat/completions and models, and Chat Completions
// is deliberately non-streaming.
type AnyGenExecutor struct {
	mu         sync.RWMutex
	baseURL    string
	apiKeyEnv  string
	models     []string
	credits    AnyGenCredits
	hasCredits bool
	verifyURL  string
	httpClient *http.Client
}

func NewAnyGenExecutor(cfg config.AnyGenConfig) *AnyGenExecutor {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultAnyGenBaseURL
	}
	apiKeyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if apiKeyEnv == "" {
		apiKeyEnv = defaultAnyGenAPIKeyEnv
	}
	return &AnyGenExecutor{
		baseURL:    baseURL,
		apiKeyEnv:  apiKeyEnv,
		models:     append([]string(nil), cfg.Models...),
		verifyURL:  defaultAnyGenVerifyURL,
		httpClient: http.DefaultClient,
	}
}

func (e *AnyGenExecutor) Models() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]string(nil), e.models...)
}

func (e *AnyGenExecutor) setModels(models []string) {
	e.mu.Lock()
	e.models = append([]string(nil), models...)
	e.mu.Unlock()
}

func (e *AnyGenExecutor) Configured() bool {
	return strings.TrimSpace(os.Getenv(e.APIKeyEnv())) != ""
}

func (e *AnyGenExecutor) BaseURL() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.baseURL
}

func (e *AnyGenExecutor) APIKeyEnv() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.apiKeyEnv
}

func (e *AnyGenExecutor) Credits() (AnyGenCredits, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.credits, e.hasCredits
}

func (e *AnyGenExecutor) apiKey() (string, error) {
	key := strings.TrimSpace(os.Getenv(e.APIKeyEnv()))
	if key == "" {
		return "", fmt.Errorf("missing environment variable %s", e.APIKeyEnv())
	}
	return key, nil
}

func (e *AnyGenExecutor) newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	key, err := e.apiKey()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (e *AnyGenExecutor) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	upstream := *req
	upstream.Stream = false
	payload, err := json.Marshal(&upstream)
	if err != nil {
		return nil, fmt.Errorf("marshal anygen request: %w", err)
	}
	httpReq, err := e.newRequest(ctx, http.MethodPost, e.BaseURL()+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call anygen chat completions: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read anygen response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Backend: "anygen", Status: resp.StatusCode, Body: string(body)}
	}
	var result types.ChatCompletionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode anygen response: %w", err)
	}
	return &result, nil
}

func (e *AnyGenExecutor) SupportsStreaming() bool { return false }

func (e *AnyGenExecutor) ExecuteStream(context.Context, *types.ChatCompletionRequest, io.Writer) (*types.Usage, error) {
	return nil, &HTTPError{
		Backend: "anygen",
		Status:  http.StatusBadRequest,
		Body:    "Chat Completions supports non-streaming requests only",
	}
}

func (e *AnyGenExecutor) SyncModels(ctx context.Context) ([]string, error) {
	req, err := e.newRequest(ctx, http.MethodGet, e.BaseURL()+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch anygen models: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read anygen models: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Backend: "anygen models", Status: resp.StatusCode, Body: string(body)}
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode anygen models: %w", err)
	}
	models := make([]string, 0, len(result.Data))
	seen := make(map[string]bool, len(result.Data))
	for _, item := range result.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("anygen models response contained no model ids")
	}
	e.setModels(models)
	return append([]string(nil), models...), nil
}

func (e *AnyGenExecutor) RefreshCredits(ctx context.Context) (AnyGenCredits, error) {
	req, err := e.newRequest(ctx, http.MethodGet, e.verifyURL, nil)
	if err != nil {
		return AnyGenCredits{}, err
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return AnyGenCredits{}, fmt.Errorf("verify anygen key: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return AnyGenCredits{}, fmt.Errorf("read anygen key verification: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AnyGenCredits{}, &HTTPError{Backend: "anygen key verify", Status: resp.StatusCode, Body: string(body)}
	}
	var credits AnyGenCredits
	if err := json.Unmarshal(body, &credits); err != nil {
		return AnyGenCredits{}, fmt.Errorf("decode anygen key verification: %w", err)
	}
	e.mu.Lock()
	e.credits = credits
	e.hasCredits = true
	e.mu.Unlock()
	return credits, nil
}
