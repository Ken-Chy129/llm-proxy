package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/executor"
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

// unreadyExecutor stands in for a provider that is registered but cannot serve
// yet — an OAuth backend with no account signed in, or a missing API key.
type unreadyExecutor struct{ stubAnthropicExecutor }

func (unreadyExecutor) Configured() bool { return false }

// pausedProviders reports the named providers as disabled.
type pausedProviders map[string]bool

func (p pausedProviders) IsBackendDisabled(provider string) bool { return p[provider] }

// newTestRouter wires three providers and routes two models across them, which
// is the shape every interesting routing question takes: a model with a chain,
// and a model with a single provider.
func newTestRouter(paused ...string) *Router {
	r := New()
	r.SetProvider("claude_oauth", &stubAnthropicExecutor{})
	r.SetProvider("relay", &stubAnthropicExecutor{})
	r.SetProvider("anygen", &stubExecutor{})
	r.SetRoutes([]Route{
		{Model: "claude-opus-5", Providers: []string{"claude_oauth", "relay"}},
		{Model: "gpt-5.4", Providers: []string{"anygen"}},
	})
	p := pausedProviders{}
	for _, b := range paused {
		p[b] = true
	}
	r.SetChecker(p)
	return r
}

func chainProviders(t *testing.T, exec executor.Executor) []string {
	t.Helper()
	c, ok := exec.(*executor.Chain)
	if !ok {
		t.Fatalf("Resolve returned %T, want a chain", exec)
	}
	return c.Providers()
}

// The chain is the routing policy: every usable provider, in configured order.
func TestResolveReturnsTheWholeChainInOrder(t *testing.T) {
	r := newTestRouter()
	exec, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	want := []string{"claude_oauth", "relay"}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

// A paused provider drops out of the chain rather than failing the model: that
// is the whole point of having a chain.
func TestPausedProviderIsSkipped(t *testing.T) {
	r := newTestRouter("claude_oauth")
	exec, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, []string{"relay"}) {
		t.Errorf("chain = %v, want [relay]", got)
	}
	if got := r.BackendName("claude-opus-5"); got != "relay" {
		t.Errorf("BackendName() = %q, want relay", got)
	}
}

func TestModelWithEveryProviderPausedFails(t *testing.T) {
	r := newTestRouter("claude_oauth", "relay")
	if _, err := r.Resolve("claude-opus-5"); err == nil {
		t.Fatal("Resolve() succeeded with every provider paused")
	}
}

// Regression guard: /v1/models used to advertise a paused provider's models,
// which Resolve then rejected — clients built pickers out of entries that
// always failed.
func TestUsableModelsExcludesFullyPausedModels(t *testing.T) {
	r := newTestRouter("anygen")
	if got := r.UsableModels(); !reflect.DeepEqual(got, []string{"claude-opus-5"}) {
		t.Errorf("UsableModels() = %v, want [claude-opus-5]", got)
	}
	// AllModels still lists it: the dashboard has to show what is configured,
	// paused or not.
	if got := r.AllModels(); len(got) != 2 {
		t.Errorf("AllModels() = %v, want both models", got)
	}
}

// A provider that cannot speak the Messages protocol is skipped for /v1/messages
// but still serves the model on the OpenAI-shaped endpoints.
func TestResolveAnthropicSkipsIncompatibleProviders(t *testing.T) {
	r := New()
	r.SetProvider("anygen", &stubExecutor{})
	r.SetProvider("relay", &stubAnthropicExecutor{})
	r.SetRoutes([]Route{{Model: "claude-opus-5", Providers: []string{"anygen", "relay"}}})

	_, serving, err := r.ResolveAnthropic("claude-opus-5")
	if err != nil {
		t.Fatalf("ResolveAnthropic() error: %v", err)
	}
	if serving != "relay" {
		t.Errorf("serving = %q, want relay", serving)
	}
}

func TestResolveAnthropicReportsUnsupportedModel(t *testing.T) {
	r := New()
	r.SetProvider("anygen", &stubExecutor{})
	r.SetRoutes([]Route{{Model: "gpt-5.4", Providers: []string{"anygen"}}})

	if _, _, err := r.ResolveAnthropic("gpt-5.4"); !errors.Is(err, ErrAnthropicUnsupported) {
		t.Fatalf("error = %v, want ErrAnthropicUnsupported", err)
	}
}

func TestUnknownModelIsNotFound(t *testing.T) {
	r := newTestRouter()
	if _, err := r.Resolve("mystery"); err == nil {
		t.Fatal("Resolve() succeeded for an unrouted model")
	}
}

// An unregistered provider (no credentials, disabled in config) is invisible to
// routing, exactly like a paused one.
func TestUnregisteredProviderIsSkipped(t *testing.T) {
	r := newTestRouter()
	r.SetProvider("claude_oauth", nil)
	exec, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, []string{"relay"}) {
		t.Errorf("chain = %v, want [relay]", got)
	}
}

// Credentials arrive while the proxy runs (an account is added from the
// dashboard, a key is set), so readiness is asked per request rather than at
// registration. A provider that cannot serve yet is skipped instead of being
// handed a request that is certain to fail.
func TestUnreadyProviderIsSkipped(t *testing.T) {
	r := newTestRouter()
	r.SetProvider("claude_oauth", &unreadyExecutor{})
	exec, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, []string{"relay"}) {
		t.Errorf("chain = %v, want [relay]", got)
	}
	// The dashboard reads the same decision, so an unready provider must not be
	// reported as the one serving the model.
	if got := r.BackendName("claude-opus-5"); got != "relay" {
		t.Errorf("BackendName() = %q, want relay", got)
	}
}

func TestModelWithOnlyUnreadyProvidersFails(t *testing.T) {
	r := New()
	r.SetProvider("claude_oauth", &unreadyExecutor{})
	r.SetRoutes([]Route{{Model: "claude-opus-5", Providers: []string{"claude_oauth"}}})
	_, err := r.Resolve("claude-opus-5")
	if err == nil {
		t.Fatal("Resolve() succeeded with no ready provider")
	}
	if !strings.Contains(err.Error(), "configured") {
		t.Errorf("error = %q, want it to say no provider is configured", err)
	}
	if got := r.UsableModels(); len(got) != 0 {
		t.Errorf("UsableModels() = %v, want none advertised", got)
	}
}

// A pin must mean the same thing as serving. Pinning to a provider that cannot
// take the request would resolve straight back to the chain, so the dashboard
// would show a pin that nothing honours.
func TestPinRejectsProviderThatCannotServe(t *testing.T) {
	r := newTestRouter("relay")
	if err := r.Pin("claude-opus-5", "relay", time.Minute); err == nil {
		t.Fatal("Pin() accepted a paused provider")
	}

	r = newTestRouter()
	r.SetProvider("relay", &unreadyExecutor{})
	if err := r.Pin("claude-opus-5", "relay", time.Minute); err == nil {
		t.Fatal("Pin() accepted a provider with no credentials")
	}
	if _, _, pinned := r.PinnedProvider("claude-opus-5"); pinned {
		t.Error("a rejected pin was still recorded")
	}
}

// A pin taken while a provider was healthy must stop being reported once that
// provider is paused: requests fall through to the chain at that point, and the
// dashboard's "serving" column has to agree with them.
func TestBackendNameIgnoresAPinThatCannotServe(t *testing.T) {
	r := newTestRouter()
	if err := r.Pin("claude-opus-5", "relay", time.Minute); err != nil {
		t.Fatalf("Pin() error: %v", err)
	}
	if got := r.BackendName("claude-opus-5"); got != "relay" {
		t.Fatalf("BackendName() = %q, want the pinned relay", got)
	}

	r.SetChecker(pausedProviders{"relay": true})
	if got := r.BackendName("claude-opus-5"); got != "claude_oauth" {
		t.Errorf("BackendName() = %q, want the chain head once the pinned provider is paused", got)
	}
}

func TestPinForcesOneProvider(t *testing.T) {
	r := newTestRouter()
	if err := r.Pin("claude-opus-5", "relay", time.Minute); err != nil {
		t.Fatalf("Pin() error: %v", err)
	}
	exec, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, []string{"relay"}) {
		t.Errorf("pinned chain = %v, want [relay]", got)
	}
}

func TestExpiredPinFallsBackToTheChain(t *testing.T) {
	r := newTestRouter()
	if err := r.Pin("claude-opus-5", "relay", time.Nanosecond); err != nil {
		t.Fatalf("Pin() error: %v", err)
	}
	time.Sleep(time.Millisecond)
	exec, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, []string{"claude_oauth", "relay"}) {
		t.Errorf("chain after expiry = %v, want the configured order", got)
	}
}

// A pin is an experiment, not a kill switch: if the pinned provider goes away,
// the model keeps serving from its chain instead of going dark.
func TestPinOnPausedProviderFallsBackToTheChain(t *testing.T) {
	r := newTestRouter()
	if err := r.Pin("claude-opus-5", "relay", time.Minute); err != nil {
		t.Fatalf("Pin() error: %v", err)
	}
	r.SetChecker(pausedProviders{"relay": true})

	exec, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, []string{"claude_oauth"}) {
		t.Errorf("chain = %v, want [claude_oauth]", got)
	}
}

func TestPinRejectsAProviderOutsideTheChain(t *testing.T) {
	r := newTestRouter()
	if err := r.Pin("claude-opus-5", "anygen", time.Minute); err == nil {
		t.Fatal("Pin() accepted a provider that does not serve the model")
	}
}

// The @provider suffix sends one request to a named provider without touching
// configuration.
func TestModelAtProviderSelectsOneProvider(t *testing.T) {
	r := newTestRouter()
	exec, err := r.Resolve("claude-opus-5@relay")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, []string{"relay"}) {
		t.Errorf("chain = %v, want [relay]", got)
	}
}

func TestModelAtUnknownProviderFails(t *testing.T) {
	r := newTestRouter()
	if _, err := r.Resolve("claude-opus-5@vertex"); err == nil {
		t.Fatal("Resolve() accepted a provider outside the model's chain")
	}
}

func TestSplitModelProvider(t *testing.T) {
	for in, want := range map[string][2]string{
		"claude-opus-5":       {"claude-opus-5", ""},
		"claude-opus-5@relay": {"claude-opus-5", "relay"},
		"@relay":              {"@relay", ""}, // no model name: not a provider suffix
	} {
		model, provider := SplitModelProvider(in)
		if model != want[0] || provider != want[1] {
			t.Errorf("SplitModelProvider(%q) = (%q, %q), want %v", in, model, provider, want)
		}
	}
}

func TestModelsByBackendListsPausedModelsToo(t *testing.T) {
	r := newTestRouter("relay")
	if got := r.ModelsByBackend("relay"); !reflect.DeepEqual(got, []string{"claude-opus-5"}) {
		t.Errorf("ModelsByBackend() = %v, want [claude-opus-5]", got)
	}
}
