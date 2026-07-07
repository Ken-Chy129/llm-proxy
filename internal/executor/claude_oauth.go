package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	internaltls "github.com/Ken-Chy129/llm-proxy/internal/tls"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

type ClaudeOAuthExecutor struct {
	oauth         *auth.ClaudeOAuth
	httpClient    *http.Client
	models        []string
	modelsMu      sync.RWMutex
	lastBetaFlags string
	betaMu        sync.RWMutex
}

func NewClaudeOAuthExecutor(oauth *auth.ClaudeOAuth, models []string) *ClaudeOAuthExecutor {
	return &ClaudeOAuthExecutor{
		oauth:      oauth,
		httpClient: internaltls.NewAnthropicHTTPClient(),
		models:     models,
	}
}

func (e *ClaudeOAuthExecutor) Models() []string {
	e.modelsMu.RLock()
	defer e.modelsMu.RUnlock()
	return e.models
}

// SetModels replaces the served model list at runtime. Callers must re-register
// the executor with the router afterwards so routing picks up the new list.
func (e *ClaudeOAuthExecutor) SetModels(models []string) {
	e.modelsMu.Lock()
	e.models = models
	e.modelsMu.Unlock()
}

// maxClaudeReactiveCooldown bounds how long a single upstream 429 sidelines a
// Claude account. Anthropic's `anthropic-ratelimit-unified-reset` hint on a
// subscription 429 points to the weekly window boundary even when the cap that
// was actually hit is model-specific (e.g. the Fable or Opus weekly limit), so
// trusting it verbatim benches the whole account for days over one model's
// limit. We never let one 429 bench an account longer than this — real
// account-wide exhaustion is decided by quota (session + all-models weekly),
// refreshed every 5 min and on each 429.
const maxClaudeReactiveCooldown = 5 * time.Minute

// capReactiveCooldown clamps a 429-derived cooldown to maxClaudeReactiveCooldown.
// When it clamps, the original reset time is no longer trustworthy as the true
// reset, so known is forced to false.
func capReactiveCooldown(until time.Time, known bool, now time.Time) (time.Time, bool) {
	if capAt := now.Add(maxClaudeReactiveCooldown); until.After(capAt) {
		return capAt, false
	}
	return until, known
}

// modelFromBody extracts the "model" field from an Anthropic request body so a
// 429 on the passthrough path can be attributed to a single model. Returns ""
// when absent, which scopes the cooldown account-wide.
func modelFromBody(body []byte) string {
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	return peek.Model
}

// doWithFailover acquires a Claude account, builds a request via makeReq, and
// sends it. On HTTP 429 it marks that account rate-limited for model (using the
// upstream reset time, clamped to maxClaudeReactiveCooldown, or a 60s default)
// and retries with the next account, up to one full pass over the account pool.
// The cooldown is scoped to model so hitting one model's cap (e.g. Fable/Opus
// weekly) doesn't sideline the account for other models. The 429 from the final
// attempt is returned so the client still sees the real upstream error when
// every account is exhausted.
func (e *ClaudeOAuthExecutor) doWithFailover(ctx context.Context, model string, makeReq func(token string) (*http.Request, error)) (*http.Response, error) {
	attempts := len(e.oauth.Store().AllForProvider("claude"))
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		token, accountID, err := e.oauth.GetTokenWithAccount(ctx, model)
		if err != nil {
			return nil, err
		}
		recordAccount(ctx, accountID)
		req, err := makeReq(token)
		if err != nil {
			return nil, err
		}
		resp, err := e.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			until, known := auth.RateLimitResetTime(resp.Header, 60*time.Second)
			// Don't let a model-specific weekly cap (whose 429 reports the weekly
			// boundary) bench the whole account for days; clamp short and let quota
			// be the authority on real account-wide exhaustion. Refetch that account's
			// quota now so a genuine session/all-models-weekly limit shows up right
			// away instead of at the next 5-min poll.
			until, known = capReactiveCooldown(until, known, time.Now())
			e.oauth.Store().MarkRateLimited("claude", accountID, model, until, !known)
			if auth.QuotaCache != nil && auth.QuotaCache.IsStale("claude:"+accountID, time.Minute) {
				go func(id string) { _ = e.oauth.FetchQuotaForAccountByID(context.Background(), id) }(accountID)
			}
			log.Printf("[failover] claude account %s rate-limited until %s (estimated=%t); %d/%d attempts used",
				accountID, until.Format(time.RFC3339), !known, i+1, attempts)
			if i < attempts-1 {
				resp.Body.Close()
				recordAccountFailover(ctx, accountID)
				lastErr = fmt.Errorf("claude account %s rate-limited (429)", accountID)
				continue
			}
			// Final attempt: let the real 429 flow back to the client.
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("claude oauth: no accounts available")
	}
	return nil, lastErr
}

func (e *ClaudeOAuthExecutor) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	ar := ToAnthropicRequest(req, req.Model)
	ar.Stream = false
	ar.AnthropicVersion = ""
	ar.Thinking = &types.ThinkingConfig{Type: "adaptive"}
	ar.MaxTokens = 64000

	body, _ := json.Marshal(ar)
	body = injectClaudeCodeSystemBlocks(body)

	resp, err := e.doWithFailover(ctx, req.Model, func(token string) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		e.applyHeaders(httpReq, token)
		return httpReq, nil
	})
	if err != nil {
		return nil, fmt.Errorf("claude oauth request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Backend: "claude oauth", Status: resp.StatusCode, Body: string(respBody)}
	}

	var anthropicResp types.AnthropicResponse
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return FromAnthropicResponse(&anthropicResp, req.Model), nil
}

func (e *ClaudeOAuthExecutor) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	ar := ToAnthropicRequest(req, req.Model)
	ar.Stream = true
	ar.AnthropicVersion = ""
	ar.Thinking = &types.ThinkingConfig{Type: "adaptive"}
	ar.MaxTokens = 64000

	body, _ := json.Marshal(ar)
	body = injectClaudeCodeSystemBlocks(body)
	log.Printf("[DEBUG-CHAT] URL=https://api.anthropic.com/v1/messages body=%s", string(body))

	resp, err := e.doWithFailover(ctx, req.Model, func(token string) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		e.applyHeaders(httpReq, token)
		log.Printf("[DEBUG-CHAT] headers: %v", redactHeaders(httpReq.Header))
		return httpReq, nil
	})
	if err != nil {
		return nil, fmt.Errorf("claude oauth stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("[DEBUG-CHAT] error response headers: %v", resp.Header)
		return nil, &HTTPError{Backend: "claude oauth", Status: resp.StatusCode, Body: string(respBody)}
	}

	chunkID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	created := time.Now().Unix()

	writeSSEChunk(w, types.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Delta: &types.ChatResult{Role: "assistant"}},
		},
	})

	var usage types.Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var hasToolCalls bool

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event types.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				usage.PromptTokens = event.Message.Usage.InputTokens
			}

		case "content_block_start":
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				hasToolCalls = true
				tc := types.ToolCall{
					ID:   event.ContentBlock.ID,
					Type: "function",
					Function: types.ToolCallFunction{Name: event.ContentBlock.Name},
				}
				writeSSEChunk(w, types.ChatCompletionChunk{
					ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []types.ChatCompletionChoice{
						{Index: 0, Delta: &types.ChatResult{ToolCalls: []types.ToolCall{tc}}},
					},
				})
			}

		case "content_block_delta":
			if len(event.Delta) > 0 {
				var delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
					JSON string `json:"partial_json"`
				}
				json.Unmarshal(event.Delta, &delta)

				switch delta.Type {
				case "text_delta":
					writeSSEChunk(w, types.ChatCompletionChunk{
						ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []types.ChatCompletionChoice{
							{Index: 0, Delta: &types.ChatResult{Content: delta.Text}},
						},
					})
				case "input_json_delta":
					tc := types.ToolCall{
						Function: types.ToolCallFunction{Arguments: delta.JSON},
					}
					writeSSEChunk(w, types.ChatCompletionChunk{
						ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []types.ChatCompletionChoice{
							{Index: 0, Delta: &types.ChatResult{ToolCalls: []types.ToolCall{tc}}},
						},
					})
				}
			}

		case "message_delta":
			if event.Usage != nil {
				usage.CompletionTokens = event.Usage.OutputTokens
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}
			finishReason := "stop"
			if hasToolCalls {
				finishReason = "tool_calls"
			}
			if len(event.Delta) > 0 {
				var d struct {
					StopReason string `json:"stop_reason"`
				}
				json.Unmarshal(event.Delta, &d)
				if d.StopReason != "" {
					finishReason = mapStopReason(d.StopReason)
				}
			}
			writeSSEChunk(w, types.ChatCompletionChunk{
				ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []types.ChatCompletionChoice{
					{Index: 0, Delta: &types.ChatResult{}, FinishReason: &finishReason},
				},
			})
		}
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	return &usage, nil
}

const defaultClaudeBeta = "claude-code-20250219,context-1m-2025-08-07,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,advisor-tool-2026-03-01,effort-2025-11-24"

func (e *ClaudeOAuthExecutor) getClaudeBeta() string {
	e.betaMu.RLock()
	defer e.betaMu.RUnlock()
	if e.lastBetaFlags != "" {
		return e.lastBetaFlags
	}
	return defaultClaudeBeta
}

func (e *ClaudeOAuthExecutor) applyHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", e.getClaudeBeta())
}

func (e *ClaudeOAuthExecutor) ExecuteAnthropicRaw(ctx context.Context, body []byte, clientHeaders http.Header) ([]byte, int, error) {
	// OAuth tokens require the Claude Code identity/billing system blocks and a
	// signed body, or Anthropic rejects the request (opaque 429). Idempotent:
	// requests that already carry the billing header are just re-signed.
	body = injectClaudeCodeSystemBlocks(body)
	resp, err := e.doWithFailover(ctx, modelFromBody(body), func(token string) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		applyAnthropicPassthroughHeaders(httpReq, token, clientHeaders)
		return httpReq, nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("claude oauth request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

func (e *ClaudeOAuthExecutor) OpenAnthropicStream(ctx context.Context, body []byte, clientHeaders http.Header) (io.ReadCloser, int, error) {
	if beta := clientHeaders.Get("anthropic-beta"); beta != "" {
		e.betaMu.Lock()
		e.lastBetaFlags = beta
		e.betaMu.Unlock()
	}
	var bodyPeek map[string]json.RawMessage
	json.Unmarshal(body, &bodyPeek)
	keys := make([]string, 0, len(bodyPeek))
	for k := range bodyPeek {
		keys = append(keys, k)
	}
	log.Printf("[DEBUG-PASSTHROUGH] body keys=%v model=%s max_tokens=%s thinking=%s", keys, bodyPeek["model"], bodyPeek["max_tokens"], bodyPeek["thinking"])

	// OAuth tokens require the Claude Code identity/billing system blocks and a
	// signed body, or Anthropic rejects the request (opaque 429). Idempotent.
	body = injectClaudeCodeSystemBlocks(body)
	resp, err := e.doWithFailover(ctx, modelFromBody(body), func(token string) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		applyAnthropicPassthroughHeaders(httpReq, token, clientHeaders)
		log.Printf("[DEBUG-PASSTHROUGH] headers: %v", redactHeaders(httpReq.Header))
		return httpReq, nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("claude oauth stream request: %w", err)
	}
	return resp.Body, resp.StatusCode, nil
}

// redactHeaders returns a shallow copy of h with credential-bearing headers
// masked, so debug logs never persist upstream tokens or client API keys.
func redactHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "cookie":
			out[k] = []string{"***REDACTED***"}
		default:
			out[k] = v
		}
	}
	return out
}

func applyAnthropicPassthroughHeaders(req *http.Request, token string, clientHeaders http.Header) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if v := clientHeaders.Get("anthropic-version"); v != "" {
		req.Header.Set("anthropic-version", v)
	} else {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if v := clientHeaders.Get("anthropic-beta"); v != "" {
		req.Header.Set("anthropic-beta", v)
	}
}
