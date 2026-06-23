package router

import (
	"fmt"
	"strings"
	"sync"

	"github.com/user/cli-proxy/internal/executor"
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
		return nil, fmt.Errorf("model %q not found, available: %v", model, r.allModelsLocked())
	}
	if backend := r.modelToBackend[model]; r.checker != nil && r.checker.IsBackendDisabled(backend) {
		return nil, fmt.Errorf("backend %q is disabled", backend)
	}
	return exec, nil
}

func (r *Router) AllModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.allModelsLocked()
}

func (r *Router) allModelsLocked() []string {
	models := make([]string, 0, len(r.modelToExecutor))
	for m := range r.modelToExecutor {
		models = append(models, m)
	}
	return models
}

func (r *Router) ModelsByBackend(backend string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var models []string
	for m, b := range r.modelToBackend {
		if b == backend {
			models = append(models, m)
		}
	}
	return models
}
