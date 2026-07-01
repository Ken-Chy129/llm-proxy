package auth

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type TokenData struct {
	ID               string `json:"id"`
	Provider         string `json:"provider"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	Email            string `json:"email,omitempty"`
	OrganizationID   string `json:"organization_id,omitempty"`
	OrganizationName string `json:"organization_name,omitempty"`
	ExpiresAt        string `json:"expires_at"`
	FileName         string `json:"-"` // actual filename on disk, tracked for correct deletion
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

// Account-selection strategies for TokenStore.Get.
const (
	StrategyWeeklyExpiry = "weekly_expiry" // quota-aware: soonest weekly reset first
	StrategyRoundRobin   = "round_robin"   // legacy blind rotation
)

type TokenStore struct {
	mu       sync.RWMutex
	dir      string
	accounts map[string][]*TokenData // provider -> accounts
	counter  atomic.Uint64
	strategy string // account-selection strategy (see Strategy* constants)
	disabled DisabledState
	// rateLimited tracks accounts cooling down after an upstream 429.
	// Keyed by "provider/id". In-memory only (not persisted): a restart clears
	// it, at worst costing one 429 to re-learn the cooldown.
	rateLimited map[string]rateLimitEntry
}

type rateLimitEntry struct {
	Until time.Time
	// Estimated is true when the upstream 429 carried no reset hint
	// (no Retry-After / ratelimit headers) and we applied a default cooldown,
	// so the Until time is a guess rather than an authoritative reset time.
	Estimated bool
}

func NewTokenStore(dir, strategy string) *TokenStore {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".llm-proxy")
	}
	if strategy != StrategyRoundRobin {
		strategy = StrategyWeeklyExpiry
	}
	os.MkdirAll(dir, 0700)
	store := &TokenStore{
		dir:         dir,
		accounts:    make(map[string][]*TokenData),
		strategy:    strategy,
		rateLimited: make(map[string]rateLimitEntry),
	}
	store.loadAll()
	store.loadDisabled()
	return store
}

// MarkRateLimited records that an account is rate-limited until the given time,
// so round-robin selection skips it until then. estimated indicates the Until
// time is a default guess (upstream gave no reset hint).
func (s *TokenStore) MarkRateLimited(provider, id string, until time.Time, estimated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited[provider+"/"+id] = rateLimitEntry{Until: until, Estimated: estimated}
}

// RateLimitInfo returns the active cooldown for an account: the time it becomes
// usable again, whether that time is an estimate, and whether a cooldown is
// currently active at all.
func (s *TokenStore) RateLimitInfo(provider, id string) (until time.Time, estimated, active bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.rateLimited[provider+"/"+id]
	if !ok || time.Now().After(e.Until) {
		return time.Time{}, false, false
	}
	return e.Until, e.Estimated, true
}

func (s *TokenStore) isRateLimitedLocked(provider, id string) bool {
	e, ok := s.rateLimited[provider+"/"+id]
	return ok && time.Now().Before(e.Until)
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

// Get returns the next token for a provider according to the configured
// strategy. Under "weekly_expiry" it prefers the usable account whose weekly
// window resets soonest (burning perishable weekly budget first); it falls
// back to round-robin when no quota-backed account qualifies. Under
// "round_robin" it uses blind rotation. Both share the same fallbacks so a
// request is always attempted while a token still exists.
func (s *TokenStore) Get(provider string) *TokenData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.accounts[provider]
	if len(list) == 0 {
		return nil
	}
	n := len(list)
	start := int(s.counter.Add(1)) % n

	notBlocked := func(t *TokenData) bool {
		return !s.isAccountDisabledLocked(provider, t.ID) && !s.isRateLimitedLocked(provider, t.ID)
	}

	// Preferred tier: quota-aware selection by soonest weekly reset.
	if s.strategy == StrategyWeeklyExpiry {
		if t := s.pickByWeeklyExpiry(provider, list, notBlocked); t != nil {
			return t
		}
	}

	// Round-robin over active, non-disabled, non-rate-limited accounts.
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if !list[idx].IsExpired() && notBlocked(list[idx]) {
			return list[idx]
		}
	}
	// All expired/disabled/rate-limited. Prefer a non-disabled account that
	// isn't rate-limited (so the caller can refresh an expired token); fall
	// back to any non-disabled account so something is always tried.
	for _, t := range list {
		if notBlocked(t) {
			return t
		}
	}
	for _, t := range list {
		if !s.isAccountDisabledLocked(provider, t.ID) {
			return t
		}
	}
	return nil
}

// pickByWeeklyExpiry selects the usable account whose weekly window resets
// soonest, so perishable weekly budget is consumed before it rolls over.
// "Usable" = not expired/disabled/rate-limited and, per fresh quota, neither
// the session (primary) nor the weekly (secondary) window is exhausted.
// Accounts without real quota data are skipped here — they fall through to the
// round-robin tier. Returns nil when no quota-backed account qualifies.
//
// Ordering: soonest known weekly reset first; unknown reset (0) sorts last;
// ties break on soonest session reset. Callers hold s.mu.
func (s *TokenStore) pickByWeeklyExpiry(provider string, list []*TokenData, notBlocked func(*TokenData) bool) *TokenData {
	type cand struct {
		t          *TokenData
		weeklyRst  int64
		sessionRst int64
	}
	now := time.Now()
	var cands []cand
	for _, t := range list {
		if t.IsExpired() || !notBlocked(t) {
			continue
		}
		q := QuotaCache.Get(provider + ":" + t.ID)
		if q == nil || !q.HasRealData {
			continue
		}
		// Proactively skip an account whose session or weekly is exhausted, so
		// we don't spend a request just to collect a 429. A window whose reset
		// has already passed counts as fresh (see RateWindow.Exhausted).
		if q.Primary.Exhausted(now) || q.Secondary.Exhausted(now) {
			continue
		}
		c := cand{t: t}
		if q.Secondary != nil {
			c.weeklyRst = q.Secondary.ResetUnix
		}
		if q.Primary != nil {
			c.sessionRst = q.Primary.ResetUnix
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		wi, wj := orFuture(cands[i].weeklyRst), orFuture(cands[j].weeklyRst)
		if wi != wj {
			return wi < wj
		}
		return orFuture(cands[i].sessionRst) < orFuture(cands[j].sessionRst)
	})
	return cands[0].t
}

// orFuture maps an unknown reset time (<=0) to the far future so it sorts after
// any known reset time.
func orFuture(unix int64) int64 {
	if unix <= 0 {
		return math.MaxInt64
	}
	return unix
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
