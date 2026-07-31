package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/gin-gonic/gin"
)

func newTrayRouter(token string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Server.TrayToken = token
	r := gin.New()
	r.GET("/api/tray", TrayAuth(cfg), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func trayStatus(t *testing.T, r *gin.Engine, setup func(*http.Request)) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/tray", nil)
	if setup != nil {
		setup(req)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestTrayAuthAcceptsConfiguredToken(t *testing.T) {
	r := newTrayRouter("tray-secret")

	if code := trayStatus(t, r, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer tray-secret")
	}); code != http.StatusOK {
		t.Errorf("Bearer with correct token = %d, want 200", code)
	}

	// Anthropic-style clients send the credential in x-api-key instead.
	if code := trayStatus(t, r, func(req *http.Request) {
		req.Header.Set("x-api-key", "tray-secret")
	}); code != http.StatusOK {
		t.Errorf("x-api-key with correct token = %d, want 200", code)
	}
}

func TestTrayAuthRejectsWrongOrMissingToken(t *testing.T) {
	r := newTrayRouter("tray-secret")

	for name, setup := range map[string]func(*http.Request){
		"no credential": nil,
		"wrong token": func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer tray-nope")
		},
		"prefix of the real token": func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer tray-secre")
		},
		"empty bearer": func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer ")
		},
	} {
		if code := trayStatus(t, r, setup); code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, code)
		}
	}
}

// The whole point of the dedicated token: a key that can spend quota on /v1/*
// must not also unlock the monitoring snapshot.
func TestTrayAuthRejectsManagedAPIKeys(t *testing.T) {
	r := newTrayRouter("tray-secret")
	if code := trayStatus(t, r, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer sk-some-managed-key")
	}); code != http.StatusUnauthorized {
		t.Errorf("managed API key = %d, want 401", code)
	}
}

// An unset token must lock the endpoint down, not open it — the opposite of
// APIKeyAuth's "no keys configured, allow everything" rule, which would leave
// account emails and usage world-readable on a fresh install.
func TestTrayAuthEmptyTokenDeniesEveryone(t *testing.T) {
	r := newTrayRouter("")

	for name, setup := range map[string]func(*http.Request){
		"no credential": nil,
		"empty bearer": func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer ")
		},
		"any token at all": func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer anything")
		},
	} {
		if code := trayStatus(t, r, setup); code != http.StatusUnauthorized {
			t.Errorf("empty tray_token, %s = %d, want 401", name, code)
		}
	}
}

func TestTrayAuthAcceptsDashboardSession(t *testing.T) {
	r := newTrayRouter("tray-secret")
	token := sessions.Create()

	if code := trayStatus(t, r, func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "session", Value: token})
	}); code != http.StatusOK {
		t.Errorf("live session cookie = %d, want 200", code)
	}
	if code := trayStatus(t, r, func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "session", Value: "not-a-session"})
	}); code != http.StatusUnauthorized {
		t.Errorf("bogus session cookie = %d, want 401", code)
	}
}
