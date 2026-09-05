package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// CatalogEntry is one model a provider's upstream says it can serve, together
// with when we first saw it. The timestamp is the point of this file: a chip
// that reads "new" is the only way an operator learns a provider gained a model
// they have not published yet.
type CatalogEntry struct {
	ID        string `json:"id"`
	FirstSeen int64  `json:"first_seen"`
}

// ModelCatalog remembers each provider's discovered model list across restarts.
//
// Without persistence every model would look new after each start, which is the
// same as nothing looking new. Storing first_seen makes "new" mean "appeared
// upstream since you last looked", not "this process has not run long".
var ModelCatalog *modelCatalog

func InitModelCatalog(dir string) {
	ModelCatalog = &modelCatalog{data: make(map[string][]CatalogEntry), dir: dir}
	ModelCatalog.load()
}

type modelCatalog struct {
	mu   sync.RWMutex
	data map[string][]CatalogEntry // key: provider name
	dir  string
}

// Set records what a provider currently advertises. Models already known keep
// their original first_seen — a re-sync must not reset the age of every model
// and blank out the new ones.
func (c *modelCatalog) Set(provider string, models []string) []CatalogEntry {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := make(map[string]int64, len(c.data[provider]))
	for _, e := range c.data[provider] {
		seen[e.ID] = e.FirstSeen
	}
	now := time.Now().Unix()
	entries := make([]CatalogEntry, 0, len(models))
	for _, id := range models {
		first, known := seen[id]
		if !known {
			first = now
		}
		entries = append(entries, CatalogEntry{ID: id, FirstSeen: first})
	}
	c.data[provider] = entries
	c.persist()
	return append([]CatalogEntry(nil), entries...)
}

func (c *modelCatalog) Get(provider string) []CatalogEntry {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]CatalogEntry(nil), c.data[provider]...)
}

func (c *modelCatalog) persist() {
	if c.dir == "" {
		return
	}
	keys := make([]string, 0, len(c.data))
	for k := range c.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	entries := make(map[string][]CatalogEntry, len(keys))
	for _, k := range keys {
		entries[k] = c.data[k]
	}
	raw, _ := json.MarshalIndent(entries, "", "  ")
	os.WriteFile(filepath.Join(c.dir, "model_catalog.json"), raw, 0600)
}

func (c *modelCatalog) load() {
	if c.dir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(c.dir, "model_catalog.json"))
	if err != nil {
		return
	}
	var entries map[string][]CatalogEntry
	if json.Unmarshal(raw, &entries) != nil {
		return
	}
	for provider, list := range entries {
		c.data[provider] = list
	}
}
