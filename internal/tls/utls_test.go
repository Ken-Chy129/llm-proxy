package tls

import (
	"context"
	"net/http"
	"testing"
)

func TestCanceledRequestDoesNotEvictSharedConnection(t *testing.T) {
	active, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if !shouldEvictConnection(active) {
		t.Fatal("an active request error must evict the failed connection")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if shouldEvictConnection(canceled) {
		t.Fatal("canceling one HTTP/2 stream must not evict the shared connection")
	}
}
