package router

import (
	"log"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
)

// Providers holds every provider the proxy can route to. They are built once at
// startup and live for the process: enabling or disabling one changes whether it
// is registered with the router, not whether it exists.
type Providers struct {
	Claude *executor.ClaudeOAuthExecutor
	Codex  *executor.CodexExecutor
	Vertex *executor.VertexExecutor
	Kimi   *executor.KimiExecutor
	Relay  *executor.KimiExecutor
	AnyGen *executor.AnyGenExecutor
}

// Apply makes the config's routing table the live one.
//
// It is the single path by which routing changes, at startup and on every
// dashboard save alike, so the running state cannot drift from what the config
// says. It does two things: hand each provider the models it must serve, and
// register the providers that can actually take traffic.
//
// A provider is registered when it is enabled in the config. Whether it can
// actually send a request — credentials, a project, a signed-in account — is
// asked on every routing decision instead, because all three arrive while the
// proxy is running. A provider that cannot answer is skipped and the next one
// in the chain serves.
func Apply(r *Router, cfg *config.Config, p *Providers) {
	routes := cfg.Routes()
	byProvider := ProviderModels(routes)

	if p.Claude != nil {
		p.Claude.SetModels(byProvider["claude_oauth"])
	}
	if p.Codex != nil {
		p.Codex.SetModels(names(byProvider["codex"]))
	}
	if p.Vertex != nil {
		p.Vertex.SetModels(byProvider["vertex"])
	}
	if p.Kimi != nil {
		p.Kimi.SetModels(byProvider["kimi"])
	}
	if p.Relay != nil {
		p.Relay.SetModels(byProvider["relay"])
	}
	if p.AnyGen != nil {
		p.AnyGen.SetModels(byProvider["anygen"])
	}

	setProvider(r, "claude_oauth", p.Claude, cfg.ClaudeOAuth.Enabled && p.Claude != nil)
	setProvider(r, "codex", p.Codex, cfg.Codex.Enabled && p.Codex != nil)
	setProvider(r, "vertex", p.Vertex, cfg.Vertex.Enabled && p.Vertex != nil)
	setProvider(r, "kimi", p.Kimi, cfg.Kimi.Enabled && p.Kimi != nil)
	setProvider(r, "relay", p.Relay, cfg.Relay.Enabled && p.Relay != nil)
	setProvider(r, "anygen", p.AnyGen, cfg.AnyGen.Enabled && p.AnyGen != nil)

	r.SetRoutes(RoutesFrom(routes))
}

// setProvider installs or removes one provider. Taking the concrete executor and
// a usable flag avoids the typed-nil trap: a nil *KimiExecutor stored in an
// Executor interface is not nil and would panic on first use.
func setProvider(r *Router, name string, exec executor.Executor, usable bool) {
	if !usable {
		r.SetProvider(name, nil)
		return
	}
	r.SetProvider(name, exec)
}

func names(models []config.ModelConfig) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = m.Name
	}
	return out
}

// LogRoutes prints the live routing table, one line per model, so a misrouted
// model is visible at startup without opening the dashboard.
func LogRoutes(r *Router) {
	for _, route := range r.Routes() {
		log.Printf("model %s -> %v", route.Model, route.Providers)
	}
}

// ProviderModels groups a routing table by provider, giving each one the models
// it must be prepared to serve and the upstream id to use for each.
//
// Providers do not read the config file: the routing table is the single place
// that decides who serves what, and this is how that decision reaches them.
func ProviderModels(routes []config.ModelRoute) map[string][]config.ModelConfig {
	out := make(map[string][]config.ModelConfig, len(config.KnownProviders))
	for _, route := range routes {
		for _, ref := range route.Providers {
			out[ref.Provider] = append(out[ref.Provider], config.ModelConfig{
				Name:  route.Name,
				Model: ref.Upstream,
			})
		}
	}
	return out
}

// RoutesFrom converts the config's model table into the router's own form.
func RoutesFrom(models []config.ModelRoute) []Route {
	out := make([]Route, 0, len(models))
	for _, route := range models {
		providers := make([]string, len(route.Providers))
		for i, ref := range route.Providers {
			providers[i] = ref.Provider
		}
		out = append(out, Route{Model: route.Name, Providers: providers})
	}
	return out
}
