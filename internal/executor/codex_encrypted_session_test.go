package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/compaction"
)

const encryptedRejected = `{"error":{"message":"The encrypted content for item cmp_old could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error"}}`

const completedResponsesStream = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

func encryptedSessionBody(encrypted string) []byte {
	body, _ := json.Marshal(map[string]interface{}{
		"model": "gpt-5.5", "stream": true,
		"input": []interface{}{
			map[string]interface{}{"type": "compaction", "id": "cmp_old", "encrypted_content": encrypted},
			map[string]interface{}{"role": "user", "content": "continue after compact"},
		},
	})
	return body
}

func TestCodexEncryptedSessionTriesAnotherAccountBeforeDegrading(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		bodies, tokens = append(bodies, string(body)), append(tokens, token)
		mu.Unlock()
		if token == "access-A" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, encryptedRejected)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, completedResponsesStream)
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "B", "A") // round-robin first selection is A

	stream, err := exec.OpenResponsesStream(context.Background(), encryptedSessionBody("opaque-account-A"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, stream)
	stream.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] != "access-A" || tokens[1] != "access-B" {
		t.Fatalf("tokens=%v", tokens)
	}
	if !strings.Contains(bodies[1], "opaque-account-A") {
		t.Fatalf("second account received degraded history: %s", bodies[1])
	}
}

func TestCodexEncryptedSessionDegradesOnlyAfterEveryAccountRejects(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		call := len(bodies)
		mu.Unlock()
		if call <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, encryptedRejected)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, completedResponsesStream)
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "B", "A")
	stream, err := exec.OpenResponsesStream(context.Background(), encryptedSessionBody("opaque-old"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, stream)
	stream.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("calls=%d bodies=%v", len(bodies), bodies)
	}
	if strings.Contains(bodies[2], "opaque-old") || !strings.Contains(bodies[2], "continue after compact") {
		t.Fatalf("degraded request did not drop only compaction: %s", bodies[2])
	}
}

func TestCodexEncryptedSessionStillSwitchesWhenOwningAccountRunsOut(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		calls = append(calls, token+" "+string(body))
		call := len(calls)
		mu.Unlock()
		switch call {
		case 1:
			// The account selected first has no quota, so account switching must
			// remain enabled even though the session carries encrypted state.
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`)
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, encryptedRejected)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, completedResponsesStream)
		}
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "B", "A")
	stream, err := exec.OpenResponsesStream(context.Background(), encryptedSessionBody("opaque-old"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, stream)
	stream.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("calls=%d, want quota failure + other account + degraded retry: %v", len(calls), calls)
	}
	if strings.Contains(calls[2], "opaque-old") {
		t.Fatalf("recovery retry still contains unreadable ciphertext: %s", calls[2])
	}
}

func TestCodexExpandsPortableCompactionBeforeUpstream(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, completedResponsesStream)
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "A")
	stream, err := exec.OpenResponsesStream(context.Background(), encryptedSessionBody(compaction.Encode("portable summary")))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, stream)
	stream.Close()
	if strings.Contains(body, "encrypted_content") || !strings.Contains(body, "portable summary") {
		t.Fatalf("portable compaction was not expanded: %s", body)
	}
}
