package router

import (
	"context"
	"io"
	"net/http"
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

type stubAnthropicExecutor struct{ stubExecutor }

func (s *stubAnthropicExecutor) ExecuteAnthropicRaw(context.Context, []byte, http.Header) ([]byte, int, error) {
	return nil, http.StatusOK, nil
}
func (s *stubAnthropicExecutor) OpenAnthropicStream(context.Context, []byte, http.Header) (io.ReadCloser, int, error) {
	return io.NopCloser(strings.NewReader("")), http.StatusOK, nil
}

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

// Every model list must be deterministic and sorted. Map iteration is
// randomised, so an unsorted list reorders itself between identical calls: that
// is what made /v1/models churn, and what made the dashboard's model chips
// visibly shuffle every time the tab regained focus and re-fetched /api/status.
func TestModelListsAreSortedAndStable(t *testing.T) {
	r := newTestRouter()
	lists := map[string]func() []string{
		"UsableModels":            r.UsableModels,
		"AllModels":               r.AllModels,
		"ModelsByBackend(claude)": func() []string { return r.ModelsByBackend("claude") },
		"ModelsByBackend(codex)":  func() []string { return r.ModelsByBackend("codex") },
	}
	for name, fn := range lists {
		first := fn()
		if len(first) == 0 {
			t.Fatalf("%s returned nothing; fixture is wrong", name)
		}
		// 30 calls is comfortably enough for Go's randomised map iteration to
		// produce a different order at least once if nothing sorts it.
		for i := 0; i < 30; i++ {
			if got := fn(); !reflect.DeepEqual(got, first) {
				t.Fatalf("%s returned %v then %v — not deterministic", name, first, got)
			}
		}
		if !sortedAscending(first) {
			t.Errorf("%s = %v, want ascending order", name, first)
		}
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

func TestBackendPriorityDoesNotDependOnRegistrationOrder(t *testing.T) {
	r := New()
	r.SetBackendPriority([]string{"claude", "anygen", "relay"})

	claude := &stubExecutor{models: []string{"claude-fable-5"}}
	relay := &stubExecutor{models: []string{"claude-fable-5"}}
	anygen := &stubExecutor{models: []string{"claude-fable-5"}}

	// This is the production failure sequence: OAuth and relay are installed at
	// startup, then a later AnyGen model sync registers the same model again.
	r.Register(claude, "claude")
	r.Register(relay, "relay")
	r.Register(anygen, "anygen")

	got, err := r.Resolve("claude-fable-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got != claude {
		t.Fatalf("Resolve() = %p, want OAuth executor %p", got, claude)
	}
	if backend := r.BackendName("claude-fable-5"); backend != "claude" {
		t.Fatalf("BackendName() = %q, want claude", backend)
	}
}

func TestBackendPriorityCanChangeLive(t *testing.T) {
	r := New()
	claude := &stubExecutor{models: []string{"shared"}}
	anygen := &stubExecutor{models: []string{"shared"}}
	r.Register(claude, "claude")
	r.Register(anygen, "anygen")

	r.SetBackendPriority([]string{"claude", "anygen"})
	if got, _ := r.Resolve("shared"); got != claude {
		t.Fatal("shared model did not initially resolve to claude")
	}

	r.SetBackendPriority([]string{"anygen", "claude"})
	if got, _ := r.Resolve("shared"); got != anygen {
		t.Fatal("shared model did not switch to anygen after priority update")
	}
}

func TestResolveSkipsDisabledHigherPriorityBackend(t *testing.T) {
	r := New()
	claude := &stubExecutor{models: []string{"shared"}}
	relay := &stubExecutor{models: []string{"shared"}}
	r.Register(claude, "claude")
	r.Register(relay, "relay")
	r.SetBackendPriority([]string{"claude", "relay"})
	r.SetChecker(pausedBackends{"claude": true})

	got, err := r.Resolve("shared")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got != relay {
		t.Fatalf("Resolve() = %p, want enabled relay %p", got, relay)
	}
	if backend := r.BackendName("shared"); backend != "relay" {
		t.Fatalf("BackendName() = %q, want relay", backend)
	}
}

func TestModelsByBackendIncludesCandidatesThatDidNotWin(t *testing.T) {
	r := New()
	r.SetBackendPriority([]string{"claude", "anygen", "relay"})
	r.Register(&stubExecutor{models: []string{"shared"}}, "claude")
	r.Register(&stubExecutor{models: []string{"shared"}}, "anygen")
	r.Register(&stubExecutor{models: []string{"shared"}}, "relay")

	for _, backend := range []string{"claude", "anygen", "relay"} {
		if got := r.ModelsByBackend(backend); !reflect.DeepEqual(got, []string{"shared"}) {
			t.Errorf("ModelsByBackend(%q) = %v, want [shared]", backend, got)
		}
	}
}

func TestResolveAnthropicSkipsHigherPriorityIncompatibleBackend(t *testing.T) {
	r := New()
	anygen := &stubExecutor{models: []string{"shared"}}
	relay := &stubAnthropicExecutor{stubExecutor{models: []string{"shared"}}}
	r.Register(anygen, "anygen")
	r.Register(relay, "relay")
	r.SetBackendPriority([]string{"anygen", "relay"})

	// Generic chat routing follows the configured order.
	if got, _ := r.Resolve("shared"); got != anygen {
		t.Fatal("generic Resolve() did not select higher-priority anygen")
	}

	// Anthropic Messages routing skips AnyGen because it cannot speak that
	// protocol, then uses the next compatible backend.
	got, backend, err := r.ResolveAnthropic("shared")
	if err != nil {
		t.Fatalf("ResolveAnthropic() error: %v", err)
	}
	if got != relay || backend != "relay" {
		t.Fatalf("ResolveAnthropic() = (%T, %q), want relay", got, backend)
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
