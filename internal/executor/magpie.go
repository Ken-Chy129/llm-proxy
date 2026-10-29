package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
)

// MagpieExecutor shares the API-key executor's Messages and Chat support, but
// sends Responses directly. Translating that protocol through Chat would lose
// native tool definitions, custom calls and provider-specific request fields.
type MagpieExecutor struct {
	*KimiExecutor
}

func NewMagpieExecutor(cfg config.MagpieConfig) *MagpieExecutor {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://axon.ponte.sh"
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	keyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = "MAGPIE_API_KEY"
	}
	return &MagpieExecutor{KimiExecutor: &KimiExecutor{
		backend: "magpie", baseURL: base, apiKeyEnv: keyEnv,
		apiFormat: "anthropic", promptCaching: true,
		httpClient: &http.Client{
			Transport: magpieTransport{base: http.DefaultTransport},
			// Neither bearer nor x-api-key may follow a gateway redirect.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}}
}

// The gateway rejects some default SDK user agents (HTTP 403 / 1010).
// Identify this client consistently for catalog, Messages and Responses calls.
type magpieTransport struct{ base http.RoundTripper }

func (t magpieTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", "llm-proxy/1.0")
	return t.base.RoundTrip(req)
}

func (e *MagpieExecutor) OpenResponsesStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	key, err := e.apiKey()
	if err != nil {
		return nil, err
	}
	// UseNumber preserves opaque metadata and future numeric protocol fields.
	// Only model/stream and our own portable compaction envelope are rewritten.
	var payload map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse magpie responses request: %w", err)
	}
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, &HTTPError{Backend: "magpie", Status: http.StatusBadRequest, Body: "model is required"}
	}
	payload["model"] = e.resolveModel(model)
	payload["stream"] = true
	expandPortableCompactions(payload)
	body, err = json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode magpie responses request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL()+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("magpie responses request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return nil, fmt.Errorf("read magpie error: %w", readErr)
		}
		return nil, &HTTPError{Backend: "magpie", Status: resp.StatusCode, Body: strings.ReplaceAll(string(raw), key, "[REDACTED]")}
	}
	stream, err := preflightResponsesStream(resp.Body, "magpie")
	if err != nil {
		resp.Body.Close()
		var upstreamErr *HTTPError
		if errors.As(err, &upstreamErr) {
			return nil, &HTTPError{Backend: upstreamErr.Backend, Status: upstreamErr.Status,
				Body: strings.ReplaceAll(upstreamErr.Body, key, "[REDACTED]")}
		}
		return nil, err
	}
	return stream, nil
}
