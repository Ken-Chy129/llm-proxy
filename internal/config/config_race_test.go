package config

import (
	"path/filepath"
	"sync"
	"testing"
)

// The dashboard rewrites admin credentials, the tray token and the port while
// request handlers are reading them. Before the accessors existed this was a
// plain data race between UpdateConfig and loginHandler; run with -race.
func TestRuntimeMutableFieldsAreRaceFree(t *testing.T) {
	cfg := &Config{}
	cfg.Server.Port = 9090
	path := filepath.Join(t.TempDir(), "config.yaml")

	var wg sync.WaitGroup
	const rounds = 200

	writers := []func(i int){
		func(i int) { cfg.SetAdminCreds("admin", "pw") },
		func(i int) { cfg.SetTrayToken("tray-token") },
		func(i int) { cfg.SetPort(9090 + i%2) },
	}
	readers := []func(){
		func() { cfg.AdminCreds() },
		func() { cfg.TrayToken() },
		func() { cfg.Port() },
		func() {
			if err := Save(path, cfg); err != nil {
				t.Errorf("save: %v", err)
			}
		},
	}

	for _, w := range writers {
		wg.Add(1)
		go func(w func(int)) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				w(i)
			}
		}(w)
	}
	for _, r := range readers {
		wg.Add(1)
		go func(r func()) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				r()
			}
		}(r)
	}
	wg.Wait()
}

// SetTrayToken stores "" verbatim (revoke), while SetAdminCreds treats "" as
// "keep current" — the asymmetry is deliberate and easy to break later.
func TestSetterEmptyValueSemantics(t *testing.T) {
	cfg := &Config{}
	cfg.SetAdminCreds("admin", "secret")
	cfg.SetTrayToken("tray-abc")

	cfg.SetAdminCreds("", "")
	if user, pass := cfg.AdminCreds(); user != "admin" || pass != "secret" {
		t.Errorf("empty admin creds overwrote existing ones: got %q/%q", user, pass)
	}

	cfg.SetTrayToken("")
	if got := cfg.TrayToken(); got != "" {
		t.Errorf("clearing tray token did not revoke it: got %q", got)
	}
}
