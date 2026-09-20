package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	// Revoked lists accounts the upstream rejected with a 401 ("provider/id").
	// Unlike Accounts (a manual pause) this is set by the proxy itself and can
	// only be cleared by a successful re-login or refresh.
	Revoked []RevokedAccount `json:"revoked,omitempty"`
}

// RevokedAccount records an account whose credentials the upstream rejected, so
// the dashboard can say which account needs a re-login and since when. It is
// persisted: a restart must not resurrect an account as "active" when the
// upstream has already invalidated its token.
type RevokedAccount struct {
	Account string `json:"account"` // "provider/id"
	At      string `json:"at"`      // RFC3339, when the 401 was observed
	Reason  string `json:"reason,omitempty"`
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

// rlKey builds the rateLimited map key. An empty model is an account-wide
// cooldown (every model unavailable); a non-empty model scopes the cooldown to
// that one model, e.g. a per-model weekly cap like Fable/Opus.
func rlKey(provider, id, model string) string {
	return provider + "/" + id + "/" + model
}

// MarkRateLimited records that an account is rate-limited until the given time,
// so selection skips it until then. Pass model="" for an account-wide limit;
// pass a model to scope the cooldown to that model only (so a per-model cap
// doesn't sideline the whole account). estimated indicates the Until time is a
// default guess (upstream gave no reset hint).
func (s *TokenStore) MarkRateLimited(provider, id, model string, until time.Time, estimated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited[rlKey(provider, id, model)] = rateLimitEntry{Until: until, Estimated: estimated}
}

// RateLimitInfo returns the active account-wide cooldown for an account: the
// time it becomes usable again, whether that time is an estimate, and whether a
// cooldown is currently active at all. Per-model cooldowns are excluded on
// purpose — they don't make the whole account unavailable, so they must not
// drive the account-level "limited" badge (which is otherwise quota-driven).
func (s *TokenStore) RateLimitInfo(provider, id string) (until time.Time, estimated, active bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.rateLimited[rlKey(provider, id, "")]
	if !ok || time.Now().After(e.Until) {
		return time.Time{}, false, false
	}
	return e.Until, e.Estimated, true
}

// isRateLimitedLocked reports whether the account is cooling down for the given
// model — either an account-wide cooldown (model "") or one scoped to this model.
func (s *TokenStore) isRateLimitedLocked(provider, id, model string) bool {
	if e, ok := s.rateLimited[rlKey(provider, id, "")]; ok && time.Now().Before(e.Until) {
		return true
	}
	if model != "" {
		if e, ok := s.rateLimited[rlKey(provider, id, model)]; ok && time.Now().Before(e.Until) {
			return true
		}
	}
	return false
}

// ClearRateLimit drops every cooldown recorded for an account, account-wide and
// per-model alike, and reports whether anything was actually removed.
//
// A cooldown and the quota snapshot are independent state: the 429 handler
// records "unavailable until T" from the upstream's reset hint, and nothing
// revisits that hint if the quota is topped up early. Refreshing an account
// upstream would otherwise leave it sidelined until the original T elapsed,
// with the dashboard showing a "limited" badge no button could clear.
func (s *TokenStore) ClearRateLimit(provider, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := provider + "/" + id + "/"
	cleared := false
	for key := range s.rateLimited {
		if strings.HasPrefix(key, prefix) {
			delete(s.rateLimited, key)
			cleared = true
		}
	}
	return cleared
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

// MarkRevoked records that the upstream rejected this account's credentials
// (HTTP 401). A revoked account is skipped by selection and surfaced in the
// dashboard, since only a re-login can bring it back. Re-marking an already
// revoked account keeps the original timestamp, so the UI shows how long it
// has been broken rather than the time of the latest failed attempt.
func (s *TokenStore) MarkRevoked(provider, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + "/" + id
	for _, r := range s.disabled.Revoked {
		if r.Account == key {
			return nil
		}
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	s.disabled.Revoked = append(s.disabled.Revoked, RevokedAccount{
		Account: key,
		At:      time.Now().Format(time.RFC3339),
		Reason:  reason,
	})
	return s.saveDisabled()
}

// ClearRevoked removes the revoked mark, called when a refresh or re-login
// produces working credentials for the account again.
func (s *TokenStore) ClearRevoked(provider, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clearRevokedLocked(provider, id)
}

// clearRevokedLocked drops the revoked mark for an account. Callers hold s.mu,
// which is why this is separate from ClearRevoked: Add and Remove need it while
// already holding the lock.
func (s *TokenStore) clearRevokedLocked(provider, id string) error {
	key := provider + "/" + id
	for i, r := range s.disabled.Revoked {
		if r.Account == key {
			s.disabled.Revoked = append(s.disabled.Revoked[:i], s.disabled.Revoked[i+1:]...)
			return s.saveDisabled()
		}
	}
	return nil
}

// RevokedInfo reports whether an account is marked revoked, and if so when it
// was first seen and what the upstream said.
func (s *TokenStore) RevokedInfo(provider, id string) (at time.Time, reason string, revoked bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.revokedLocked(provider, id)
	if !ok {
		return time.Time{}, "", false
	}
	at, _ = time.Parse(time.RFC3339, r.At)
	return at, r.Reason, true
}

// IsRevoked reports whether the upstream has rejected this account's credentials.
func (s *TokenStore) IsRevoked(provider, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.revokedLocked(provider, id)
	return ok
}

func (s *TokenStore) revokedLocked(provider, id string) (RevokedAccount, bool) {
	key := provider + "/" + id
	for _, r := range s.disabled.Revoked {
		if r.Account == key {
			return r, true
		}
	}
	return RevokedAccount{}, false
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
// strategy. model scopes rate-limit filtering: an account cooling down only for
// a specific model, or whose quota shows that model's weekly cap (Fable/Opus)
// spent, is still eligible for other models. Pass "" when the caller isn't
// model-specific. Under "weekly_expiry" it
// prefers the usable account whose weekly window resets soonest (burning
// perishable weekly budget first); it falls back to round-robin when no
// quota-backed account qualifies. Under "round_robin" it uses blind rotation.
// Both share the same fallbacks so a request is always attempted while a token
// still exists.
func (s *TokenStore) Get(provider, model string) *TokenData {
	return s.GetExcluding(provider, model, nil)
}

// GetExcluding applies the normal selection policy while omitting accounts
// already tried by the current request. Provider executors use it to walk the
// pool without defeating quota-aware selection or accidentally reusing a
// manually disabled/revoked/cooling-down account.
func (s *TokenStore) GetExcluding(provider, model string, excluded map[string]bool) *TokenData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.accounts[provider]
	if len(list) == 0 {
		return nil
	}
	n := len(list)
	start := int(s.counter.Add(1)) % n

	now := time.Now()
	notBlocked := func(t *TokenData) bool {
		if excluded[t.ID] {
			return false
		}
		if _, revoked := s.revokedLocked(provider, t.ID); revoked {
			return false
		}
		if s.isAccountDisabledLocked(provider, t.ID) || s.isRateLimitedLocked(provider, t.ID, model) {
			return false
		}
		if QuotaCache != nil {
			q := QuotaCache.Get(provider + ":" + t.ID)
			// Fresh quota showing the session or all-models weekly window spent
			// is the same signal the dashboard's "limited" badge uses. Skipping
			// here keeps selection consistent with it: an account shown as
			// limited is never attempted. Exhausted() honours the reset time, so
			// a stale snapshot stops blocking on its own once the window rolls.
			if q != nil && q.HasRealData && (q.Primary.Exhausted(now) || q.Secondary.Exhausted(now)) {
				return false
			}
			// A model-scoped weekly cap (e.g. Fable) that quota shows as spent
			// makes this account useless for that model until the reset, even
			// though it still serves everything else.
			if model != "" && q.ModelExhausted(model, now) != nil {
				return false
			}
		}
		return true
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
	// Every active account is blocked. An expired access token is still worth
	// handing back: the caller refreshes it and the request goes through.
	for _, t := range list {
		if notBlocked(t) {
			return t
		}
	}
	// Nothing usable remains. This used to fall back to "any non-disabled
	// account so something is always tried", which handed back accounts that
	// were cooling down after a 429 or whose quota showed the window spent.
	// With the whole pool limited, every request then walked all of them
	// collecting one guaranteed 429 each before failing over to the next
	// provider. Return nil instead so the chain moves on immediately; the
	// cooldown expiring or a quota refresh brings accounts back on their own.
	return nil
}

// GetByIDIfUsable returns one specific account only while ordinary selection
// would still consider it eligible: not excluded, paused, revoked, cooling
// down or out of quota. Cache-locality callers use it to keep a conversation
// on the account whose upstream cache machine holds its prefix, without the
// preference ever overriding availability. An expired access token is still
// returned because the caller refreshes that exact account before use.
func (s *TokenStore) GetByIDIfUsable(provider, id, model string, excluded map[string]bool) *TokenData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id == "" || excluded[id] {
		return nil
	}
	now := time.Now()
	for _, t := range s.accounts[provider] {
		if t.ID != id {
			continue
		}
		if s.isAccountDisabledLocked(provider, id) || s.isRateLimitedLocked(provider, id, model) {
			return nil
		}
		if _, revoked := s.revokedLocked(provider, id); revoked {
			return nil
		}
		if QuotaCache != nil {
			q := QuotaCache.Get(provider + ":" + id)
			if q != nil && q.HasRealData && (q.Primary.Exhausted(now) || q.Secondary.Exhausted(now)) {
				return nil
			}
			if model != "" && q.ModelExhausted(model, now) != nil {
				return nil
			}
		}
		return t
	}
	return nil
}

// ErrAllAccountsRateLimited is returned by token lookups when a provider has
// accounts but every one is cooling down or out of quota. Executors map it to a
// 429 so the provider chain fails over instead of reporting a login problem.
var ErrAllAccountsRateLimited = errors.New("all accounts are rate-limited or out of quota")

// AllRateLimited reports whether the provider has accounts but every one that
// could otherwise serve (not paused, not revoked) is currently held back by a
// 429 cooldown or by quota showing its window spent. Executors use this to
// turn a nil selection into a 429-class error instead of "not authenticated",
// so the chain fails over to the next provider the way a real 429 would.
func (s *TokenStore) AllRateLimited(provider, model string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	limited := 0
	for _, t := range s.accounts[provider] {
		if s.isAccountDisabledLocked(provider, t.ID) {
			continue
		}
		if _, revoked := s.revokedLocked(provider, t.ID); revoked {
			continue
		}
		if s.isRateLimitedLocked(provider, t.ID, model) {
			limited++
			continue
		}
		if QuotaCache != nil {
			q := QuotaCache.Get(provider + ":" + t.ID)
			if q != nil && q.HasRealData && (q.Primary.Exhausted(now) || q.Secondary.Exhausted(now)) {
				limited++
				continue
			}
			if model != "" && q.ModelExhausted(model, now) != nil {
				limited++
				continue
			}
		}
		// A servable account exists; Get would have returned it.
		return false
	}
	return limited > 0
}

// pickByWeeklyExpiry selects the usable account whose weekly window resets
// soonest, so perishable weekly budget is consumed before it rolls over.
// "Usable" = not expired/disabled/rate-limited and, per fresh quota, neither
// the session (primary) nor the all-models-weekly (secondary) window is
// exhausted. Model-specific weekly limits (Opus/Fable) live in Additional and
// are applied per request model by notBlocked, never account-wide.
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
	var cands []cand
	for _, t := range list {
		if t.IsExpired() || !notBlocked(t) {
			continue
		}
		q := QuotaCache.Get(provider + ":" + t.ID)
		if q == nil || !q.HasRealData {
			continue
		}
		// notBlocked has already skipped exhausted windows; this tier only adds
		// the reset-time ordering on top.
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
	// Storing credentials means someone just logged in (or a refresh produced a
	// new token), so whatever the upstream rejected before is no longer what we
	// hold. Leaving the mark would keep a freshly re-authenticated account
	// sidelined and showing "re-login needed" forever, since account ids are
	// emails and a re-login reuses the same id.
	if err := s.clearRevokedLocked(data.Provider, data.ID); err != nil {
		return err
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
			// Drop the revoked mark too, or re-adding the same account (ids are
			// emails, so it comes back with the same key) would inherit the
			// state of the credentials that were just deleted.
			return s.clearRevokedLocked(provider, id)
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
		if _, revoked := s.revokedLocked(provider, t.ID); revoked {
			continue
		}
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
