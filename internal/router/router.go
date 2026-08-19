package router

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Ken-Chy129/llm-proxy/internal/executor"
)

var ErrAnthropicUnsupported = errors.New("model does not support Anthropic Messages API")

type BackendChecker interface {
	IsBackendDisabled(backend string) bool
}

type Router struct {
	mu sync.RWMutex
	// candidates keeps every backend that can serve a model. A backend refresh
	// only changes its own candidate; it can never overwrite another backend.
	candidates map[string]map[string]executor.Executor // model -> backend -> exec
	priority   []string
	checker    BackendChecker
}

func New() *Router {
	return &Router{
		candidates: make(map[string]map[string]executor.Executor),
	}
}

func (r *Router) SetChecker(c BackendChecker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checker = c
}

func (r *Router) Register(exec executor.Executor, backend string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, model := range exec.Models() {
		r.registerModelLocked(model, exec, backend)
	}
}

func (r *Router) RegisterModel(model string, exec executor.Executor, backend string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerModelLocked(model, exec, backend)
}

func (r *Router) registerModelLocked(model string, exec executor.Executor, backend string) {
	model = strings.TrimSpace(model)
	backend = strings.TrimSpace(backend)
	if model == "" || backend == "" || exec == nil {
		return
	}
	if r.candidates[model] == nil {
		r.candidates[model] = make(map[string]executor.Executor)
	}
	r.candidates[model][backend] = exec
}

func (r *Router) UnregisterBackend(backend string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for model, choices := range r.candidates {
		delete(choices, backend)
		if len(choices) == 0 {
			delete(r.candidates, model)
		}
	}
}

func (r *Router) BackendName(model string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, backend, _ := r.resolveLocked(strings.TrimSpace(model))
	return backend
}

func (r *Router) Resolve(model string) (executor.Executor, error) {
	model = strings.TrimSpace(model)
	r.mu.RLock()
	defer r.mu.RUnlock()
	exec, backend, ok := r.resolveLocked(model)
	if !ok {
		// List only usable models: naming a paused backend's model as "available"
		// in a not-found error just sends the caller to the next error.
		return nil, fmt.Errorf("model %q not found, available: %v", model, r.usableModelsLocked())
	}
	if exec == nil {
		return nil, fmt.Errorf("all backends for model %q are disabled (highest priority: %q)", model, backend)
	}
	return exec, nil
}

// ResolveAnthropic applies the same backend priority while ignoring candidates
// that cannot speak Anthropic's native Messages protocol. This lets a broad
// OpenAI-only catalog rank above a relay for Chat Completions without breaking
// Claude Code: /v1/messages simply moves to the next compatible candidate.
func (r *Router) ResolveAnthropic(model string) (executor.AnthropicExecutor, string, error) {
	model = strings.TrimSpace(model)
	r.mu.RLock()
	defer r.mu.RUnlock()

	choices := r.candidates[model]
	if len(choices) == 0 {
		return nil, "", fmt.Errorf("model %q not found, available: %v", model, r.usableModelsLocked())
	}
	hasCompatible := false
	for _, backend := range r.orderedBackendsLocked(choices) {
		ae, ok := choices[backend].(executor.AnthropicExecutor)
		if !ok {
			continue
		}
		hasCompatible = true
		if r.checker != nil && r.checker.IsBackendDisabled(backend) {
			continue
		}
		return ae, backend, nil
	}
	if !hasCompatible {
		return nil, "", fmt.Errorf("%w: %s", ErrAnthropicUnsupported, model)
	}
	return nil, "", fmt.Errorf("all Anthropic-compatible backends for model %q are disabled", model)
}

// SetBackendPriority atomically changes collision resolution. Backends omitted
// from the list remain available after the configured entries, ordered by name,
// so a partial setting cannot accidentally make a backend unreachable.
func (r *Router) SetBackendPriority(priority []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]bool, len(priority))
	r.priority = r.priority[:0]
	for _, backend := range priority {
		backend = strings.TrimSpace(backend)
		if backend == "" || seen[backend] {
			continue
		}
		seen[backend] = true
		r.priority = append(r.priority, backend)
	}
}

func (r *Router) BackendPriority() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.priority...)
}

// resolveLocked picks the first enabled candidate according to priority.
// If candidates exist but all are disabled, exec is nil, backend names the
// highest-priority candidate, and ok remains true.
func (r *Router) resolveLocked(model string) (exec executor.Executor, backend string, ok bool) {
	choices := r.candidates[model]
	if len(choices) == 0 {
		return nil, "", false
	}
	for _, b := range r.orderedBackendsLocked(choices) {
		if backend == "" {
			backend = b
		}
		if r.checker != nil && r.checker.IsBackendDisabled(b) {
			continue
		}
		return choices[b], b, true
	}
	return nil, backend, true
}

func (r *Router) orderedBackendsLocked(choices map[string]executor.Executor) []string {
	out := make([]string, 0, len(choices))
	seen := make(map[string]bool, len(choices))
	for _, backend := range r.priority {
		if _, ok := choices[backend]; ok {
			out = append(out, backend)
			seen[backend] = true
		}
	}
	var rest []string
	for backend := range choices {
		if !seen[backend] {
			rest = append(rest, backend)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
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
	models := make([]string, 0, len(r.candidates))
	for m := range r.candidates {
		models = append(models, m)
	}
	sort.Strings(models)
	return models
}

// usableModelsLocked must be called with r.mu held. Calling the checker under
// the lock is safe and already done by Resolve: it reads the token store's own
// mutex and never calls back into the router.
func (r *Router) usableModelsLocked() []string {
	models := make([]string, 0, len(r.candidates))
	for m := range r.candidates {
		if exec, _, ok := r.resolveLocked(m); ok && exec != nil {
			models = append(models, m)
		}
	}
	sort.Strings(models)
	return models
}

// ModelsByBackend lists one backend's models, paused or not — the dashboard
// draws a backend's chips from this and must still show them while it is paused.
//
// Sorted for the same reason as the other lists: /api/status is re-fetched every
// time the tab regains focus, and unsorted map iteration made the chips visibly
// shuffle on each refresh.
func (r *Router) ModelsByBackend(backend string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var models []string
	for m, choices := range r.candidates {
		if _, ok := choices[backend]; ok {
			models = append(models, m)
		}
	}
	sort.Strings(models)
	return models
}
