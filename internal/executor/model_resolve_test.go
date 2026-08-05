package executor

import (
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
)

// A model entry carries an upstream id only when the served name has to be
// renamed, which is the exception. The empty case has to fall through to the
// name itself — returning the empty Model verbatim would send `"model": ""`
// upstream, which fails as a confusing 400 rather than anything diagnosable.
func TestResolveModelTreatsEmptyUpstreamAsPassthrough(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "kimi-k3", Model: "k3"}, // renamed
		{Name: "kimi-for-coding"},      // passthrough
		{Name: "claude-haiku-4-5", Model: "claude-haiku-4-5-20251001"},
	}
	cases := []struct{ in, want string }{
		{"kimi-k3", "k3"},
		{"kimi-for-coding", "kimi-for-coding"},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001"},
		{"not-configured-at-all", "not-configured-at-all"},
	}

	kimi := &KimiExecutor{models: models}
	vertex := &VertexExecutor{cfg: config.VertexConfig{Models: models}}
	for _, c := range cases {
		if got := kimi.resolveModel(c.in); got != c.want {
			t.Errorf("kimi.resolveModel(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := vertex.resolveModel(c.in); got != c.want {
			t.Errorf("vertex.resolveModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
