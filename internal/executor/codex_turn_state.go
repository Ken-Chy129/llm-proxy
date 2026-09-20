package executor

import (
	"strings"
	"sync"
	"time"
)

// Upstream returns x-codex-turn-state on every Codex response and expects the
// next turn of the same conversation to send it back. Measured on a fixed
// 19K-token prefix, six consecutive turns hit 95%+ when the state was echoed
// and alternated between ~95% and 0% when it was not: the token is what routes
// a turn back to the machine that holds the conversation's prompt cache.
//
// The Codex client echoes it itself, but only when it saw it on the previous
// response. A 429 retry, an account switch or any path that answered without
// the header breaks the chain and the client then keeps sending turns without
// state. The proxy therefore remembers the latest state per conversation and
// fills it in whenever the client did not, so one gap does not cost the
// session its cache locality.
//
// A state is bound to the account whose cache machine issued it; reusing it
// on another account would route to the wrong machine. That makes account
// choice part of cache locality: with round-robin selection a two-account
// pool alternates A/B/A/B and no turn ever follows the one that wrote its
// state. So the conversation's last serving account is preferred while it
// remains usable, and the state is dropped when the account really changes.
// Entries expire with the upstream cache lifetime (30 minutes for GPT-5.6+).
const (
	codexTurnStateTTL = 30 * time.Minute
	codexTurnStateMax = 10000
)

type codexTurnState struct {
	state     string
	accountID string
	seenAt    time.Time
}

type codexTurnStateStore struct {
	mu      sync.Mutex
	entries map[string]codexTurnState
}

func newCodexTurnStateStore() *codexTurnStateStore {
	return &codexTurnStateStore{entries: make(map[string]codexTurnState)}
}

// preferredAccount reports the account that last served key, or "" when none
// is remembered or the entry expired.
func (s *codexTurnStateStore) preferredAccount(key string) string {
	if s == nil || key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return ""
	}
	if time.Since(e.seenAt) > codexTurnStateTTL {
		delete(s.entries, key)
		return ""
	}
	return e.accountID
}

// lookup returns the remembered state for key when it was issued for
// accountID and has not expired. Empty otherwise.
func (s *codexTurnStateStore) lookup(key, accountID string) string {
	if s == nil || key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return ""
	}
	if time.Since(e.seenAt) > codexTurnStateTTL || e.accountID != accountID {
		delete(s.entries, key)
		return ""
	}
	return e.state
}

// Upstream only issues a new turn-state when the request carried none; a turn
// that replays a valid state gets a response without the header. remember
// therefore keeps the existing entry when state is empty, refreshing its age
// so an active conversation never expires mid-session.
func (s *codexTurnStateStore) remember(key, accountID, state string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if state == "" {
		if e, ok := s.entries[key]; ok && e.accountID == accountID {
			e.seenAt = now
			s.entries[key] = e
		}
		return
	}
	if len(s.entries) >= codexTurnStateMax {
		cutoff := now.Add(-codexTurnStateTTL)
		for k, e := range s.entries {
			if e.seenAt.Before(cutoff) {
				delete(s.entries, k)
			}
		}
		if len(s.entries) >= codexTurnStateMax {
			var oldestKey string
			var oldest time.Time
			for k, e := range s.entries {
				if oldestKey == "" || e.seenAt.Before(oldest) {
					oldestKey, oldest = k, e.seenAt
				}
			}
			delete(s.entries, oldestKey)
		}
	}
	s.entries[key] = codexTurnState{state: state, accountID: accountID, seenAt: now}
}

// codexConversationKey identifies a conversation for turn-state purposes.
// prompt_cache_key is what the Codex client sets per thread; the window id
// header is the fallback for clients that send one but no cache key.
func codexConversationKey(req map[string]interface{}, windowID string) string {
	if key, _ := req["prompt_cache_key"].(string); strings.TrimSpace(key) != "" {
		return "pck:" + strings.TrimSpace(key)
	}
	if windowID = strings.TrimSpace(windowID); windowID != "" {
		return "win:" + windowID
	}
	return ""
}
