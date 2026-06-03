package router

import (
	"fmt"

	"github.com/user/cli-proxy/internal/executor"
)

type Router struct {
	modelToExecutor map[string]executor.Executor
	modelToBackend  map[string]string
}

func New() *Router {
	return &Router{
		modelToExecutor: make(map[string]executor.Executor),
		modelToBackend:  make(map[string]string),
	}
}

func (r *Router) Register(exec executor.Executor, backend string) {
	for _, model := range exec.Models() {
		r.modelToExecutor[model] = exec
		r.modelToBackend[model] = backend
	}
}

func (r *Router) BackendName(model string) string {
	return r.modelToBackend[model]
}

func (r *Router) Resolve(model string) (executor.Executor, error) {
	exec, ok := r.modelToExecutor[model]
	if !ok {
		return nil, fmt.Errorf("model %q not found", model)
	}
	return exec, nil
}

func (r *Router) AllModels() []string {
	models := make([]string, 0, len(r.modelToExecutor))
	for m := range r.modelToExecutor {
		models = append(models, m)
	}
	return models
}
