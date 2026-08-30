package router

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/executor"
)

var ErrAnthropicUnsupported = errors.New("model does not support Anthropic Messages API")

type BackendChecker interface {
	IsBackendDisabled(backend string) bool
}

// readinessChecker is implemented by providers whose ability to serve can change
// while the proxy runs — OAuth accounts get added from the dashboard, Vertex
// credentials get uploaded, an API key env var can be missing. A provider that
// says it is not configured is skipped, and the next one in the chain serves.
type readinessChecker interface {
	Configured() bool
}

// providerReady reports whether an executor can take a request right now.
// Providers that do not implement the interface are always ready.
func providerReady(e executor.Executor) bool {
	r, ok := e.(readinessChecker)
	return !ok || r.Configured()
}

// Router maps a published model name to the ordered providers that can serve
// it. The order is the whole routing policy: the first usable provider serves,
// and the ones after it are the failover chain.
//
// Providers register the executor they are (SetProvider); the routing table says
// which models each one may serve (SetRoutes). The two are deliberately
// separate — a provider re-registering its client must not disturb routing, and
// a routing edit must not need the providers rebuilt.
type Router struct {
	mu        sync.RWMutex
	providers map[string]executor.Executor // provider name -> live executor
	routes    map[string]Route             // model name -> ordered chain
	order     []string                     // model names, in configured order
	checker   BackendChecker
	pins      map[string]pin
}

// Route is one model's resolved provider chain.
type Route struct {
	Model     string
	Providers []string
}

// pin temporarily forces a model onto one provider, for trying a provider out
// without editing (and persisting) the routing table. It lives in memory only:
// an experiment must not outlive a restart.
type pin struct {
	provider string
	until    time.Time
}

func New() *Router {
	return &Router{
		providers: make(map[string]executor.Executor),
		routes:    make(map[string]Route),
		pins:      make(map[string]pin),
	}
}

func (r *Router) SetChecker(c BackendChecker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checker = c
}

// SetProvider installs (or replaces) a provider's live executor. Passing a nil
// executor removes it, which is how a provider that loses its credentials stops
// being routable without any change to the routing table.
func (r *Router) SetProvider(name string, exec executor.Executor) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if exec == nil {
		delete(r.providers, name)
		return
	}
	r.providers[name] = exec
}

// SetRoutes replaces the whole routing table.
func (r *Router) Provider(name string) (executor.Executor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	exec, ok := r.providers[strings.TrimSpace(name)]
	if !ok {
		return nil, false
	}
	if r.checker != nil && r.checker.IsBackendDisabled(name) {
		return nil, false
	}
	return exec, true
}

// SetRoutes replaces the whole routing table.
func (r *Router) SetRoutes(routes []Route) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = make(map[string]Route, len(routes))
	r.order = r.order[:0]
	for _, route := range routes {
		model := strings.TrimSpace(route.Model)
		if model == "" || len(route.Providers) == 0 {
			continue
		}
		r.routes[model] = Route{Model: model, Providers: append([]string(nil), route.Providers...)}
		r.order = append(r.order, model)
	}
}

// Routes returns the routing table in configured order.
func (r *Router) Routes() []Route {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Route, 0, len(r.order))
	for _, model := range r.order {
		route := r.routes[model]
		out = append(out, Route{Model: model, Providers: append([]string(nil), route.Providers...)})
	}
	return out
}

// Pin forces model onto provider until the TTL expires. A zero or negative TTL
// clears the pin.
func (r *Router) Pin(model, provider string, ttl time.Duration) error {
	model, provider = strings.TrimSpace(model), strings.TrimSpace(provider)
	r.mu.Lock()
	defer r.mu.Unlock()
	if ttl <= 0 {
		delete(r.pins, model)
		return nil
	}
	route, ok := r.routes[model]
	if !ok {
		return fmt.Errorf("model %q not found", model)
	}
	// Validated the same way a request would resolve it, so "pinned" cannot mean
	// something different from "serving": pinning to a paused or credential-less
	// provider would silently fall through to the chain on the next request.
	if _, err := r.pinnedExecutorLocked(route, provider, nil); err != nil {
		return err
	}
	r.pins[model] = pin{provider: provider, until: time.Now().Add(ttl)}
	return nil
}

// PinnedProvider reports an active pin for a model, if any.
func (r *Router) PinnedProvider(model string) (string, time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.pins[strings.TrimSpace(model)]
	if !ok || !p.until.After(time.Now()) {
		return "", time.Time{}, false
	}
	return p.provider, p.until, true
}

// SplitModelProvider understands the "model@provider" escape hatch, which sends
// one request to a named provider without touching configuration. It is the
// per-request equivalent of a pin.
func SplitModelProvider(model string) (string, string) {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "@"); i > 0 {
		return model[:i], model[i+1:]
	}
	return model, ""
}

// BackendName reports the provider that would serve the model right now, which
// is also what gets logged when nothing fails over. An active pin wins, but
// only when it can actually serve: a pin whose provider has since been paused
// or lost its credentials falls through to the chain, and naming it here would
// tell the dashboard something that is not happening.
func (r *Router) BackendName(model string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	name, _ := SplitModelProvider(model)
	if p, ok := r.pins[name]; ok && p.until.After(time.Now()) {
		if route, ok := r.routes[name]; ok {
			if _, err := r.pinnedExecutorLocked(route, p.provider, nil); err == nil {
				return p.provider
			}
		}
	}
	serving, _ := r.chainLocked(name, nil)
	return serving
}

// Resolve returns an executor for the model. When more than one provider can
// serve it, the result is a chain that tries them in order.
func (r *Router) Resolve(model string) (executor.Executor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveLocked(model, nil)
}

// resolveLocked builds the executor for a model, keeping only providers that
// satisfy `accept` (nil accepts every provider).
func (r *Router) resolveLocked(model string, accept func(executor.Executor) bool) (executor.Executor, error) {
	name, provider := SplitModelProvider(model)
	route, ok := r.routes[name]
	if !ok {
		// List only usable models: naming a paused provider's model as
		// "available" in a not-found error just sends the caller to the next one.
		return nil, fmt.Errorf("model %q not found, available: %v", name, r.usableModelsLocked())
	}
	if provider != "" {
		return r.pinnedExecutorLocked(route, provider, accept)
	}
	if p, ok := r.pins[name]; ok && p.until.After(time.Now()) {
		if exec, err := r.pinnedExecutorLocked(route, p.provider, accept); err == nil {
			return exec, nil
		}
		// A pin that cannot serve this request (paused provider, wrong protocol)
		// falls through to the configured chain rather than failing the request:
		// a temporary experiment must not be able to take a model offline.
	}
	_, chain := r.chainLocked(name, accept)
	if len(chain) == 0 {
		return nil, r.unusableErrorLocked(route, accept)
	}
	return executor.NewChain(chain), nil
}

func (r *Router) pinnedExecutorLocked(route Route, provider string, accept func(executor.Executor) bool) (executor.Executor, error) {
	for _, p := range route.Providers {
		if p != provider {
			continue
		}
		exec, ok := r.providers[p]
		if !ok {
			return nil, fmt.Errorf("provider %q is not available for model %q", provider, route.Model)
		}
		if r.checker != nil && r.checker.IsBackendDisabled(p) {
			return nil, fmt.Errorf("provider %q is paused", provider)
		}
		if !providerReady(exec) {
			return nil, fmt.Errorf("provider %q has no usable credentials", provider)
		}
		if accept != nil && !accept(exec) {
			return nil, fmt.Errorf("%w: %s via %s", ErrAnthropicUnsupported, route.Model, provider)
		}
		return executor.NewChain([]executor.Link{{Provider: p, Exec: exec}}), nil
	}
	return nil, fmt.Errorf("provider %q does not serve model %q", provider, route.Model)
}

// chainLocked returns the serving provider and the usable links for a model.
func (r *Router) chainLocked(model string, accept func(executor.Executor) bool) (string, []executor.Link) {
	name, _ := SplitModelProvider(model)
	route, ok := r.routes[name]
	if !ok {
		return "", nil
	}
	var links []executor.Link
	for _, p := range route.Providers {
		exec, ok := r.providers[p]
		if !ok {
			continue
		}
		if r.checker != nil && r.checker.IsBackendDisabled(p) {
			continue
		}
		if !providerReady(exec) {
			continue
		}
		if accept != nil && !accept(exec) {
			continue
		}
		links = append(links, executor.Link{Provider: p, Exec: exec})
	}
	if len(links) == 0 {
		return "", nil
	}
	return links[0].Provider, links
}

// unusableErrorLocked explains why a configured model cannot be served, which is
// always one of three different problems for the operator to fix.
func (r *Router) unusableErrorLocked(route Route, accept func(executor.Executor) bool) error {
	registered, ready, compatible := 0, 0, 0
	for _, p := range route.Providers {
		exec, ok := r.providers[p]
		if !ok {
			continue
		}
		registered++
		if !providerReady(exec) {
			continue
		}
		ready++
		if accept == nil || accept(exec) {
			compatible++
		}
	}
	switch {
	case registered == 0 || ready == 0:
		return fmt.Errorf("no provider for model %q is configured (chain: %v)", route.Model, route.Providers)
	case compatible == 0:
		return fmt.Errorf("%w: %s", ErrAnthropicUnsupported, route.Model)
	default:
		return fmt.Errorf("all providers for model %q are paused (chain: %v)", route.Model, route.Providers)
	}
}

// ResolveAnthropic applies the same backend priority while ignoring candidates
// that cannot speak Anthropic's native Messages protocol. This lets a broad
// OpenAI-only catalog rank above a relay for Chat Completions without breaking
// Claude Code: /v1/messages simply moves to the next compatible candidate.
func (r *Router) ResolveAnthropic(model string) (executor.AnthropicExecutor, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	accept := func(e executor.Executor) bool {
		_, ok := e.(executor.AnthropicExecutor)
		return ok
	}
	exec, err := r.resolveLocked(model, accept)
	if err != nil {
		return nil, "", err
	}
	ae, ok := exec.(executor.AnthropicExecutor)
	if !ok {
		name, _ := SplitModelProvider(model)
		return nil, "", fmt.Errorf("%w: %s", ErrAnthropicUnsupported, name)
	}
	name, _ := SplitModelProvider(model)
	serving, _ := r.chainLocked(name, accept)
	return ae, serving, nil
}

// AllModels returns every registered model, including those on paused backends.
// For anything a client sees, use UsableModels — advertising a model that
// Resolve will reject is worse than not advertising it.
func (r *Router) AllModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.allModelsLocked()
}

// UsableModels returns the models a request could actually be served by right
// now: registered, and on a backend that is not paused. Sorted, so /v1/models
// doesn't reorder itself on every call (Go map iteration is randomised).
func (r *Router) UsableModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.usableModelsLocked()
}

func (r *Router) allModelsLocked() []string {
	models := make([]string, 0, len(r.routes))
	for m := range r.routes {
		models = append(models, m)
	}
	sort.Strings(models)
	return models
}

// usableModelsLocked must be called with r.mu held. Calling the checker under
// the lock is safe and already done by Resolve: it reads the token store's own
// mutex and never calls back into the router.
func (r *Router) usableModelsLocked() []string {
	models := make([]string, 0, len(r.routes))
	for m := range r.routes {
		if serving, _ := r.chainLocked(m, nil); serving != "" {
			models = append(models, m)
		}
	}
	sort.Strings(models)
	return models
}

// ModelsByBackend lists the models a provider may serve, paused or not — the
// dashboard draws a provider's chips from this and must still show them while it
// is paused.
//
// Sorted for the same reason as the other lists: /api/status is re-fetched every
// time the tab regains focus, and unsorted map iteration made the chips visibly
// shuffle on each refresh.
func (r *Router) ModelsByBackend(backend string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var models []string
	for m, route := range r.routes {
		for _, p := range route.Providers {
			if p == backend {
				models = append(models, m)
				break
			}
		}
	}
	sort.Strings(models)
	return models
}
