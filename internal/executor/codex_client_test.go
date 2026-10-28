package executor

import "testing"

func TestCodexExecutorReusesHTTPClient(t *testing.T) {
	exec := NewCodexExecutor(nil, nil)

	if first, second := exec.client(), exec.client(); first != second {
		t.Fatal("Codex executor created a new HTTP client instead of reusing its connection pool")
	}
}
