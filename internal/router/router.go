package router

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Ken-Chy129/llm-proxy/internal/executor"
)

type BackendChecker interface {
	IsBackendDisabled(backend string) bool
}

type Router struct {
	mu              sync.RWMutex
	modelToExecutor map[string]executor.Executor
	modelToBackend  map[string]string
	checker         BackendChecker
}

func New() *Router {
	return &Router{
		modelToExecutor: make(map[string]executor.Executor),
		modelToBackend:  make(map[string]string),
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
		r.modelToExecutor[model] = exec
		r.modelToBackend[model] = backend
	}
}

func (r *Router) RegisterModel(model string, exec executor.Executor, backend string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modelToExecutor[model] = exec
	r.modelToBackend[model] = backend
}

func (r *Router) UnregisterBackend(backend string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for model, b := range r.modelToBackend {
		if b == backend {
			delete(r.modelToExecutor, model)
			delete(r.modelToBackend, model)
		}
	}
}

func (r *Router) BackendName(model string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.modelToBackend[model]
}

func (r *Router) Resolve(model string) (executor.Executor, error) {
	model = strings.TrimSpace(model)
	r.mu.RLock()
	defer r.mu.RUnlock()
	exec, ok := r.modelToExecutor[model]
	if !ok {
		// List only usable models: naming a paused backend's model as "available"
		// in a not-found error just sends the caller to the next error.
		return nil, fmt.Errorf("model %q not found, available: %v", model, r.usableModelsLocked())
	}
	if backend := r.modelToBackend[model]; r.checker != nil && r.checker.IsBackendDisabled(backend) {
		return nil, fmt.Errorf("backend %q is disabled", backend)
	}
	return exec, nil
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
	models := make([]string, 0, len(r.modelToExecutor))
	for m := range r.modelToExecutor {
		models = append(models, m)
	}
	sort.Strings(models)
	return models
}

// usableModelsLocked must be called with r.mu held. Calling the checker under
// the lock is safe and already done by Resolve: it reads the token store's own
// mutex and never calls back into the router.
func (r *Router) usableModelsLocked() []string {
	models := make([]string, 0, len(r.modelToExecutor))
	for m := range r.modelToExecutor {
		if r.checker != nil && r.checker.IsBackendDisabled(r.modelToBackend[m]) {
			continue
		}
		models = append(models, m)
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
	for m, b := range r.modelToBackend {
		if b == backend {
			models = append(models, m)
		}
	}
	sort.Strings(models)
	return models
}
