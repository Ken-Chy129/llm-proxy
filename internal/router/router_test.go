package router

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// stubExecutor is a do-nothing executor; these tests only exercise routing.
type stubExecutor struct{ models []string }

func (s *stubExecutor) Execute(context.Context, *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	return nil, nil
}
func (s *stubExecutor) ExecuteStream(context.Context, *types.ChatCompletionRequest, io.Writer) (*types.Usage, error) {
	return nil, nil
}
func (s *stubExecutor) Models() []string { return s.models }

// pausedBackends reports the named backends as disabled.
type pausedBackends map[string]bool

func (p pausedBackends) IsBackendDisabled(backend string) bool { return p[backend] }

func newTestRouter(paused ...string) *Router {
	r := New()
	r.Register(&stubExecutor{models: []string{"opus", "sonnet"}}, "claude")
	r.Register(&stubExecutor{models: []string{"gpt-5.5", "gpt-5.4"}}, "codex")
	r.Register(&stubExecutor{models: []string{"vertex-opus"}}, "vertex")
	p := pausedBackends{}
	for _, b := range paused {
		p[b] = true
	}
	r.SetChecker(p)
	return r
}

// TestUsableModelsExcludesPausedBackends is the regression guard: /v1/models used
// to advertise a paused backend's models, which Resolve then rejected — clients
// built pickers out of entries that always failed.
func TestUsableModelsExcludesPausedBackends(t *testing.T) {
	r := newTestRouter("vertex", "codex")

	want := []string{"opus", "sonnet"}
	if got := r.UsableModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("UsableModels() = %v, want %v", got, want)
	}

	// Every model UsableModels advertises must actually resolve, and every model
	// it withholds must not. That is the invariant the bug broke.
	for _, m := range r.UsableModels() {
		if _, err := r.Resolve(m); err != nil {
			t.Errorf("Resolve(%q) = %v, but UsableModels advertised it", m, err)
		}
	}
	for _, m := range []string{"gpt-5.5", "gpt-5.4", "vertex-opus"} {
		if _, err := r.Resolve(m); err == nil {
			t.Errorf("Resolve(%q) succeeded, but its backend is paused", m)
		}
	}
}

// AllModels keeps listing everything: the dashboard draws a paused backend's
// model chips from the registry and must still see them.
func TestAllModelsKeepsPausedBackends(t *testing.T) {
	r := newTestRouter("vertex", "codex")
	want := []string{"gpt-5.4", "gpt-5.5", "opus", "sonnet", "vertex-opus"}
	if got := r.AllModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("AllModels() = %v, want %v", got, want)
	}
}

func TestModelListsAreSorted(t *testing.T) {
	// Map iteration is randomised, so an unsorted list would reorder itself
	// between calls and make /v1/models churn for no reason.
	r := newTestRouter()
	first := r.UsableModels()
	for i := 0; i < 20; i++ {
		if got := r.UsableModels(); !reflect.DeepEqual(got, first) {
			t.Fatalf("UsableModels() returned %v then %v — not deterministic", first, got)
		}
	}
	if !sortedAscending(first) {
		t.Errorf("UsableModels() = %v, want ascending order", first)
	}
	if !sortedAscending(r.AllModels()) {
		t.Errorf("AllModels() = %v, want ascending order", r.AllModels())
	}
}

// A not-found error should point at models that would actually work.
func TestResolveNotFoundListsOnlyUsableModels(t *testing.T) {
	r := newTestRouter("codex")
	_, err := r.Resolve("no-such-model")
	if err == nil {
		t.Fatal("expected an error for an unknown model")
	}
	if strings.Contains(err.Error(), "gpt-5.5") {
		t.Errorf("error names a paused backend's model as available: %v", err)
	}
	if !strings.Contains(err.Error(), "opus") {
		t.Errorf("error omits a usable model: %v", err)
	}
}

// With no checker wired up nothing is paused, so both lists agree.
func TestNoCheckerTreatsEverythingAsUsable(t *testing.T) {
	r := New()
	r.Register(&stubExecutor{models: []string{"a", "b"}}, "claude")
	if got, want := r.UsableModels(), r.AllModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("UsableModels() = %v, AllModels() = %v; want equal when no checker is set", got, want)
	}
}

func TestUnregisterBackendRemovesItsModels(t *testing.T) {
	r := newTestRouter()
	r.UnregisterBackend("codex")
	for _, m := range r.AllModels() {
		if strings.HasPrefix(m, "gpt-") {
			t.Errorf("AllModels() still contains %q after UnregisterBackend", m)
		}
	}
}

func sortedAscending(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}
