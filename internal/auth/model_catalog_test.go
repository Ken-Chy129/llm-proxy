package auth

import (
	"testing"
	"time"
)

// The point of persisting a catalog is that "new" means "new upstream", not
// "new to this process". A re-sync must therefore leave known models' first_seen
// alone, and a restart must not reset it either.
func TestModelCatalogKeepsFirstSeenAcrossSyncsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	InitModelCatalog(dir)

	first := ModelCatalog.Set("anygen", []string{"gpt-5.5", "gpt-5.4"})
	if len(first) != 2 {
		t.Fatalf("entries = %d, want 2", len(first))
	}
	original := map[string]int64{}
	for _, e := range first {
		if e.FirstSeen == 0 {
			t.Fatalf("%s has no first_seen", e.ID)
		}
		original[e.ID] = e.FirstSeen
	}

	// Backdate one entry so a preserved timestamp is distinguishable from a
	// freshly stamped one.
	backdated := time.Now().Add(-30 * 24 * time.Hour).Unix()
	ModelCatalog.mu.Lock()
	ModelCatalog.data["anygen"][0].FirstSeen = backdated
	ModelCatalog.mu.Unlock()

	after := ModelCatalog.Set("anygen", []string{"gpt-5.5", "gpt-5.4", "gpt-5.6-luna"})
	byID := map[string]int64{}
	for _, e := range after {
		byID[e.ID] = e.FirstSeen
	}
	if byID["gpt-5.5"] != backdated {
		t.Errorf("re-sync reset first_seen for a known model: %d, want %d", byID["gpt-5.5"], backdated)
	}
	if byID["gpt-5.4"] != original["gpt-5.4"] {
		t.Errorf("re-sync reset first_seen for a known model: %d, want %d", byID["gpt-5.4"], original["gpt-5.4"])
	}
	if byID["gpt-5.6-luna"] == 0 {
		t.Error("a model seen for the first time got no first_seen")
	}

	// A restart reads the same directory; timestamps must survive it, otherwise
	// every model looks new on every boot and the badge means nothing.
	InitModelCatalog(dir)
	reloaded := ModelCatalog.Get("anygen")
	if len(reloaded) != 3 {
		t.Fatalf("reloaded entries = %d, want 3", len(reloaded))
	}
	for _, e := range reloaded {
		if e.ID == "gpt-5.5" && e.FirstSeen != backdated {
			t.Errorf("restart lost first_seen: %d, want %d", e.FirstSeen, backdated)
		}
	}
}

// Removing a model upstream drops it from the catalog: the card describes what
// the provider offers now, not everything it ever offered.
func TestModelCatalogForgetsModelsTheUpstreamDropped(t *testing.T) {
	InitModelCatalog(t.TempDir())
	ModelCatalog.Set("kimi", []string{"kimi-k3", "kimi-k2"})
	ModelCatalog.Set("kimi", []string{"kimi-k3"})

	got := ModelCatalog.Get("kimi")
	if len(got) != 1 || got[0].ID != "kimi-k3" {
		t.Fatalf("catalog = %+v, want only kimi-k3", got)
	}
}
