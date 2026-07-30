package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newCORSRouter mirrors how /api/tray is wired in Run: CORS first, then a
// handler standing in for the authenticated endpoint.
func newCORSRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tray", DesktopCORS(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.OPTIONS("/api/tray", DesktopCORS())
	return r
}

func TestDesktopCORSAllowsTauriOrigins(t *testing.T) {
	r := newCORSRouter()
	for _, origin := range []string{
		"tauri://localhost",
		"https://tauri.localhost",
		"http://tauri.localhost",
		"http://localhost:1420",
		"http://127.0.0.1:1420",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/tray", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("origin %q: Allow-Origin = %q, want %q", origin, got, origin)
		}
		if w.Code != http.StatusOK {
			t.Errorf("origin %q: status = %d, want 200", origin, w.Code)
		}
		// Vary must be present because the header value depends on Origin.
		if got := w.Header().Get("Vary"); got != "Origin" {
			t.Errorf("origin %q: Vary = %q, want Origin", origin, got)
		}
	}
}

// TestDesktopCORSRejectsUnknownOrigins is the important one: mirroring back an
// arbitrary Origin would let any website read the tray data of a logged-in user.
func TestDesktopCORSRejectsUnknownOrigins(t *testing.T) {
	r := newCORSRouter()
	for _, origin := range []string{
		"https://evil.example.com",
		"http://localhost:3000",        // a different local port is not ours
		"tauri://localhost.evil.com",   // suffix trick
		"https://tauri.localhost.evil", // prefix trick
		"null",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/tray", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q must NOT be allowed, got Allow-Origin = %q", origin, got)
		}
	}
}

// TestDesktopCORSNeverAllowsCredentials guards against the CSRF footgun: the
// widget authenticates with a Bearer key, so cookies must never be permitted.
// Allow-Credentials together with a reflected origin would let a listed origin
// ride on a live dashboard session.
func TestDesktopCORSNeverAllowsCredentials(t *testing.T) {
	r := newCORSRouter()
	req := httptest.NewRequest(http.MethodGet, "/api/tray", nil)
	req.Header.Set("Origin", "tauri://localhost")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials must never be set, got %q", got)
	}
}

// TestDesktopCORSPreflightSkipsAuth documents why CORS is ordered before
// APIKeyAuth: a preflight carries no Authorization header, so auth-first would
// answer 401 and the widget would never get to send the real request.
func TestDesktopCORSPreflightSkipsAuth(t *testing.T) {
	r := newCORSRouter()
	req := httptest.NewRequest(http.MethodOptions, "/api/tray", nil)
	req.Header.Set("Origin", "tauri://localhost")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "tauri://localhost" {
		t.Errorf("preflight Allow-Origin = %q, want tauri://localhost", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("preflight must advertise allowed headers, else Authorization is rejected")
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("preflight must advertise allowed methods")
	}
}

// A request with no Origin (curl, the dashboard itself) must pass through
// untouched — CORS headers only matter for browsers.
func TestDesktopCORSNoOriginPassesThrough(t *testing.T) {
	r := newCORSRouter()
	req := httptest.NewRequest(http.MethodGet, "/api/tray", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("no Origin should mean no CORS header, got %q", got)
	}
}
