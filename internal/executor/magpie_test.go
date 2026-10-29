package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/compaction"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
)

const magpieTestKey = "magpie-test-secret"

func magpieForTest(t *testing.T, base string) *MagpieExecutor {
	t.Helper()
	t.Setenv("TEST_MAGPIE_KEY", magpieTestKey)
	e := NewMagpieExecutor(config.MagpieConfig{BaseURL: base, APIKeyEnv: "TEST_MAGPIE_KEY"})
	e.SetModels([]config.ModelConfig{{Name: "claude-opus-5-5", Model: "claude/claude-opus-5-5"}})
	return e
}

func TestMagpieNativeResponsesPreservesToolsAndHistory(t *testing.T) {
	var got map[string]json.RawMessage
	wire := "data: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"ping\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"custom_tool_call\",\"call_id\":\"call_2\",\"name\":\"apply_patch\",\"input\":\"ping\"}]}}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+magpieTestKey ||
			r.Header.Get("User-Agent") != "llm-proxy/1.0" {
			t.Error("wrong endpoint, authentication or user agent")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, wire)
	}))
	defer up.Close()
	body := []byte(`{"model":"claude-opus-5-5","stream":false,
		"input":[{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"patch"},{"type":"custom_tool_call_output","call_id":"call_1","output":"success"}],
		"tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}},{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}],
		"tool_choice":"auto","additional_tools":[{"type":"function","name":"more"}],"metadata":{"sequence":9007199254740993}}`)
	e := magpieForTest(t, up.URL+"/v1/")
	s, err := e.OpenResponsesStream(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gotWire, err := io.ReadAll(s)
	if err != nil || string(gotWire) != wire {
		t.Fatalf("stream changed: %s, %v", gotWire, err)
	}
	var want map[string]json.RawMessage
	json.Unmarshal(body, &want)
	for _, key := range []string{"input", "tools", "additional_tools", "tool_choice", "metadata"} {
		var g, w interface{}
		json.Unmarshal(got[key], &g)
		json.Unmarshal(want[key], &w)
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s was changed", key)
		}
	}
	if string(got["model"]) != `"claude/claude-opus-5-5"` || string(got["stream"]) != "true" ||
		!strings.Contains(string(got["metadata"]), "9007199254740993") {
		t.Fatal("model alias, streaming or exact JSON numbers lost")
	}
}

func TestMagpieResponsesFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body             string
		httpStatus, wantStatus int
	}{
		{"auth", "invalid " + magpieTestKey, 401, 401},
		{"event_secret", "data: {\"type\":\"error\",\"message\":\"invalid " + magpieTestKey + "\"}\n\n", 200, 502},
		{"rate_limit", "busy", 429, 429},
		{"failed_event", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"model_at_capacity\",\"message\":\"busy\"}}}\n\n", 200, 429},
		{"empty_stream", "", 200, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.httpStatus)
				io.WriteString(w, tc.body)
			}))
			defer up.Close()
			_, err := magpieForTest(t, up.URL).OpenResponsesStream(context.Background(), []byte(`{"model":"claude-opus-5-5","input":"hi"}`))
			var he *HTTPError
			if !errors.As(err, &he) || he.Backend != "magpie" || he.Status != tc.wantStatus {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), magpieTestKey) {
				t.Fatal("credential exposed in error")
			}
		})
	}
}

func TestMagpiePortableCompaction(t *testing.T) {
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer up.Close()
	body, _ := json.Marshal(map[string]interface{}{
		"model": "claude-opus-5-5",
		"input": []interface{}{map[string]string{"type": "compaction", "encrypted_content": compaction.Encode("portable summary")}},
	})
	s, err := magpieForTest(t, up.URL).OpenResponsesStream(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if !strings.Contains(got, "portable summary") || strings.Contains(got, "encrypted_content") {
		t.Fatal("portable compaction was not expanded")
	}
}

func TestMagpieModelsAndAnthropicAuthentication(t *testing.T) {
	for _, suffix := range []string{"", "/v1", "/v1/"} {
		t.Run(suffix, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") != "llm-proxy/1.0" || r.Header.Get("x-api-key") != magpieTestKey {
					t.Error("missing gateway headers")
				}
				switch r.URL.Path {
				case "/v1/models":
					io.WriteString(w, `{"data":[{"id":"claude/claude-opus-5-5"}]}`)
				case "/v1/messages":
					var body map[string]interface{}
					json.NewDecoder(r.Body).Decode(&body)
					if body["model"] != "claude/claude-opus-5-5" || r.Header.Get("anthropic-version") != "2023-06-01" {
						t.Error("Anthropic model or version missing")
					}
					io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer up.Close()
			e := magpieForTest(t, up.URL+suffix)
			if models, err := e.SyncModels(context.Background()); err != nil || len(models) != 1 {
				t.Fatalf("catalog: %v %v", models, err)
			}
			s, status, err := e.OpenAnthropicStream(context.Background(), []byte(`{"model":"claude-opus-5-5","messages":[],"max_tokens":16}`), nil)
			if err != nil || status != 200 {
				t.Fatalf("Messages: %d %v", status, err)
			}
			s.Close()
		})
	}
}

func TestMagpieDoesNotFollowRedirectsOrSendCanceledRequests(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	e := magpieForTest(t, up.URL)
	body := []byte(`{"model":"claude-opus-5-5","input":"hi"}`)
	if _, err := e.OpenResponsesStream(context.Background(), body); StatusFromError(err) != 307 || calls.Load() != 0 {
		t.Fatalf("redirect leaked a request: %v, calls=%d", err, calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.OpenResponsesStream(ctx, body); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
}
