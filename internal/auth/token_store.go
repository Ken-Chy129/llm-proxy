package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type TokenData struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email,omitempty"`
	ExpiresAt    string `json:"expires_at"`
	FileName     string `json:"-"` // actual filename on disk, tracked for correct deletion
}

func (t *TokenData) IsExpired() bool {
	if t.ExpiresAt == "" {
		return true
	}
	exp, err := time.Parse(time.RFC3339, t.ExpiresAt)
	if err != nil {
		return true
	}
	return time.Now().After(exp.Add(-5 * time.Minute))
}

func (t *TokenData) StatusLabel() string {
	if t.IsExpired() {
		return "expired"
	}
	return "active"
}

type DisabledState struct {
	Backends []string `json:"backends,omitempty"`
	Accounts []string `json:"accounts,omitempty"` // "provider/id"
}

type TokenStore struct {
	mu       sync.RWMutex
	dir      string
	accounts map[string][]*TokenData // provider -> accounts
	counter  atomic.Uint64
	disabled DisabledState
}

func NewTokenStore(dir string) *TokenStore {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cli-proxy")
	}
	os.MkdirAll(dir, 0700)
	store := &TokenStore{dir: dir, accounts: make(map[string][]*TokenData)}
	store.loadAll()
	store.loadDisabled()
	return store
}

func (s *TokenStore) disabledPath() string {
	return filepath.Join(s.dir, "disabled.json")
}

func (s *TokenStore) loadDisabled() {
	raw, err := os.ReadFile(s.disabledPath())
	if err != nil {
		return
	}
	json.Unmarshal(raw, &s.disabled)
}

func (s *TokenStore) saveDisabled() error {
	raw, _ := json.MarshalIndent(s.disabled, "", "  ")
	return os.WriteFile(s.disabledPath(), raw, 0600)
}

func (s *TokenStore) DisableBackend(backend string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.disabled.Backends {
		if b == backend {
			return nil
		}
	}
	s.disabled.Backends = append(s.disabled.Backends, backend)
	return s.saveDisabled()
}

func (s *TokenStore) EnableBackend(backend string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, b := range s.disabled.Backends {
		if b == backend {
			s.disabled.Backends = append(s.disabled.Backends[:i], s.disabled.Backends[i+1:]...)
			return s.saveDisabled()
		}
	}
	return nil
}

func (s *TokenStore) IsBackendDisabled(backend string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.disabled.Backends {
		if b == backend {
			return true
		}
	}
	return false
}

func (s *TokenStore) DisableAccount(provider, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + "/" + id
	for _, a := range s.disabled.Accounts {
		if a == key {
			return nil
		}
	}
	s.disabled.Accounts = append(s.disabled.Accounts, key)
	return s.saveDisabled()
}

func (s *TokenStore) EnableAccount(provider, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + "/" + id
	for i, a := range s.disabled.Accounts {
		if a == key {
			s.disabled.Accounts = append(s.disabled.Accounts[:i], s.disabled.Accounts[i+1:]...)
			return s.saveDisabled()
		}
	}
	return nil
}

func (s *TokenStore) IsAccountDisabled(provider, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isAccountDisabledLocked(provider, id)
}

func (s *TokenStore) isAccountDisabledLocked(provider, id string) bool {
	key := provider + "/" + id
	for _, a := range s.disabled.Accounts {
		if a == key {
			return true
		}
	}
	return false
}

func (s *TokenStore) Dir() string { return s.dir }

// Get returns the next active token for a provider using round-robin.
func (s *TokenStore) Get(provider string) *TokenData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.accounts[provider]
	if len(list) == 0 {
		return nil
	}

	// Round-robin over active, non-disabled accounts
	n := len(list)
	start := int(s.counter.Add(1)) % n
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if !list[idx].IsExpired() && !s.isAccountDisabledLocked(provider, list[idx].ID) {
			return list[idx]
		}
	}
	// All expired/disabled, return first non-disabled so caller can try refresh
	for _, t := range list {
		if !s.isAccountDisabledLocked(provider, t.ID) {
			return t
		}
	}
	return nil
}

// GetByID returns a specific account by ID.
func (s *TokenStore) GetByID(provider, id string) *TokenData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.accounts[provider] {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// All returns all accounts grouped by provider (for status display).
func (s *TokenStore) All() map[string][]*TokenData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string][]*TokenData, len(s.accounts))
	for k, v := range s.accounts {
		cp := make([]*TokenData, len(v))
		copy(cp, v)
		result[k] = cp
	}
	return result
}

// AllForProvider returns all accounts for a specific provider.
func (s *TokenStore) AllForProvider(provider string) []*TokenData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.accounts[provider]
	cp := make([]*TokenData, len(list))
	copy(cp, list)
	return cp
}

// Add adds or updates an account. If an account with the same email/ID exists, update it.
func (s *TokenStore) Add(data *TokenData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if data.ID == "" {
		data.ID = data.Email
		if data.ID == "" {
			data.ID = fmt.Sprintf("%s-%d", data.Provider, time.Now().UnixMilli())
		}
	}

	list := s.accounts[data.Provider]
	found := false
	for i, t := range list {
		if t.ID == data.ID {
			list[i] = data
			found = true
			break
		}
	}
	if !found {
		s.accounts[data.Provider] = append(list, data)
	}
	return s.save(data)
}

// Remove removes an account by provider and ID.
func (s *TokenStore) Remove(provider, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	list := s.accounts[provider]
	for i, t := range list {
		if t.ID == id {
			s.accounts[provider] = append(list[:i], list[i+1:]...)
			// Delete using tracked filename if available, else try both patterns
			if t.FileName != "" {
				os.Remove(filepath.Join(s.dir, t.FileName))
			} else {
				os.Remove(filepath.Join(s.dir, s.filename(provider, id)))
				os.Remove(filepath.Join(s.dir, id)) // legacy format
			}
			return nil
		}
	}
	return fmt.Errorf("account %s/%s not found", provider, id)
}

// ActiveCount returns the number of non-expired accounts for a provider.
func (s *TokenStore) ActiveCount(provider string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, t := range s.accounts[provider] {
		if !t.IsExpired() {
			count++
		}
	}
	return count
}

func (s *TokenStore) filename(provider, id string) string {
	safe := provider + "_" + sanitizeFilename(id) + ".json"
	return safe
}

func sanitizeFilename(s string) string {
	result := make([]byte, 0, len(s))
	for _, b := range []byte(s) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '-' || b == '_' || b == '.' || b == '@' {
			result = append(result, b)
		} else {
			result = append(result, '_')
		}
	}
	return string(result)
}

func (s *TokenStore) save(data *TokenData) error {
	raw, _ := json.MarshalIndent(data, "", "  ")
	path := filepath.Join(s.dir, s.filename(data.Provider, data.ID))
	return os.WriteFile(path, raw, 0600)
}

func (s *TokenStore) loadAll() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var data TokenData
		if err := json.Unmarshal(raw, &data); err != nil {
			continue
		}
		if data.Provider == "" {
			continue
		}
		if data.ID == "" {
			data.ID = data.Email
			if data.ID == "" {
				data.ID = e.Name()
			}
		}
		data.FileName = e.Name()
		s.accounts[data.Provider] = append(s.accounts[data.Provider], &data)
		count++
	}
	if count > 0 {
		fmt.Printf("loaded %d account(s) from %s\n", count, s.dir)
	}
}
