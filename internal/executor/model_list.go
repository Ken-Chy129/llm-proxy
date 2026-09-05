package executor

import (
	"encoding/json"
	"strings"
)

// parseModelListIDs reads the two shapes a /models endpoint returns: the
// OpenAI-style {"data":[{"id":...}]} and the Anthropic-style {"data":[{"id":...}]}
// — identical on the wire for our purposes — plus a bare list, which some
// relays hand back. Ids are de-duplicated and kept in upstream order, since that
// order is usually newest-first and is the one worth reading.
func parseModelListIDs(body []byte) ([]string, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	items := payload.Data
	if len(items) == 0 {
		items = payload.Models
	}
	models := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	return models, nil
}
