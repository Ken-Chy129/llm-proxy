package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type KeyData struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Key             string `json:"key"`
	TokenLimitDaily int    `json:"token_limit_daily,omitempty"`
	CreatedAt       string `json:"created_at"`
	Disabled        bool   `json:"disabled,omitempty"`
}

type KeyStore struct {
	mu   sync.RWMutex
	dir  string
	keys []*KeyData
}

func NewKeyStore(dir string) *KeyStore {
	ks := &KeyStore{dir: dir}
	ks.load()
	return ks
}

func (ks *KeyStore) path() string {
	return filepath.Join(ks.dir, "keys.json")
}

func (ks *KeyStore) load() {
	raw, err := os.ReadFile(ks.path())
	if err != nil {
		return
	}
	json.Unmarshal(raw, &ks.keys)
}

func (ks *KeyStore) save() error {
	raw, _ := json.MarshalIndent(ks.keys, "", "  ")
	return os.WriteFile(ks.path(), raw, 0600)
}

func (ks *KeyStore) Validate(key string) *KeyData {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	for _, k := range ks.keys {
		if k.Key == key {
			return k
		}
	}
	return nil
}

func (ks *KeyStore) All() []*KeyData {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	cp := make([]*KeyData, len(ks.keys))
	copy(cp, ks.keys)
	return cp
}

func (ks *KeyStore) Add(name string, tokenLimitDaily int) (*KeyData, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	buf := make([]byte, 32)
	rand.Read(buf)
	key := "sk-" + hex.EncodeToString(buf)

	id := fmt.Sprintf("key_%d", time.Now().UnixMilli())
	kd := &KeyData{
		ID:              id,
		Name:            name,
		Key:             key,
		TokenLimitDaily: tokenLimitDaily,
		CreatedAt:       time.Now().Format(time.RFC3339),
	}
	ks.keys = append(ks.keys, kd)
	return kd, ks.save()
}

func (ks *KeyStore) Update(id string, name string, tokenLimitDaily int) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for _, k := range ks.keys {
		if k.ID == id {
			k.Name = name
			k.TokenLimitDaily = tokenLimitDaily
			return ks.save()
		}
	}
	return fmt.Errorf("key %s not found", id)
}

// SetDisabled toggles a key's disabled flag. A disabled key is rejected by the
// auth middleware but kept (and re-enableable), unlike Delete.
func (ks *KeyStore) SetDisabled(id string, disabled bool) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for _, k := range ks.keys {
		if k.ID == id {
			k.Disabled = disabled
			return ks.save()
		}
	}
	return fmt.Errorf("key %s not found", id)
}

func (ks *KeyStore) Delete(id string) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for i, k := range ks.keys {
		if k.ID == id {
			ks.keys = append(ks.keys[:i], ks.keys[i+1:]...)
			return ks.save()
		}
	}
	return fmt.Errorf("key %s not found", id)
}

func (ks *KeyStore) Count() int {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return len(ks.keys)
}
