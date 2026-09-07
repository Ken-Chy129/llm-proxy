package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The prompt cache is prefix-based, so a client identity that changes every call
// tells upstream each turn came from a new machine and costs cache locality.
// Two consecutive requests must therefore carry the same installation id.
func TestCodexInstallationIDStableAcrossRequests(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("x-codex-installation-id"))
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	withCodexUpstream(t, upstream.URL, upstream.URL)

	exec, _ := newCodexTestExecutor(t, "acct-1")
	for i := 0; i < 2; i++ {
		if err := exec.ExecuteRawStream(context.Background(), []byte(`{"model":"gpt-5.5"}`), io.Discard); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(seen))
	}
	if seen[0] == "" {
		t.Fatal("installation id header was not sent")
	}
	if seen[0] != seen[1] {
		t.Errorf("installation id changed between requests: %q then %q", seen[0], seen[1])
	}
}

// The id is persisted beside the OAuth tokens, so an executor rebuilt against an
// existing token dir reuses the id rather than presenting a new identity.
func TestCodexInstallationIDReadFromDisk(t *testing.T) {
	dir := t.TempDir()
	const want = "11111111-2222-3333-4444-555555555555"
	if err := os.WriteFile(filepath.Join(dir, "codex_installation_id"), []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("seed id: %v", err)
	}

	// installationID memoises per process, so exercise the read path directly
	// rather than depending on which test ran first.
	got := readInstallationID(dir)
	if got != want {
		t.Errorf("installation id = %q, want %q (trailing newline must be trimmed)", got, want)
	}
}

// A read-only token dir must not break requests: the id only has to hold still
// for this process to be useful to the cache.
func TestCodexInstallationIDSurvivesUnwritableDir(t *testing.T) {
	if id := readInstallationID(filepath.Join(t.TempDir(), "does-not-exist")); id == "" {
		t.Error("expected a usable in-memory id when the dir cannot be used")
	}
}
