package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
)

var sessions = &sessionStore{tokens: make(map[string]bool)}

type sessionStore struct {
	mu     sync.RWMutex
	tokens map[string]bool
}

func (s *sessionStore) Create() string {
	buf := make([]byte, 32)
	rand.Read(buf)
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	s.tokens[token] = true
	s.mu.Unlock()
	return token
}

func (s *sessionStore) Valid(token string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tokens[token]
}

// DesktopCORS allows the Tauri desktop widget to call an endpoint cross-origin.
//
// A packaged Tauri app runs on tauri://localhost (macOS/Linux) or
// https://tauri.localhost (Windows), so every request to a remote proxy is
// cross-origin and the browser demands CORS headers — without these the widget
// fails at the preflight with no useful error.
//
// Origins are whitelisted rather than mirrored back, and credentials are never
// allowed: the widget authenticates with a Bearer token, so permitting cookies
// would let any origin listed here ride on a live dashboard session. Echoing
// arbitrary origins plus allow-credentials is the classic mistake that turns a
// convenience header into CSRF against every admin route.
func DesktopCORS() gin.HandlerFunc {
	allowed := map[string]bool{
		"tauri://localhost":       true, // macOS / Linux bundle
		"https://tauri.localhost": true, // Windows bundle
		"http://tauri.localhost":  true,
		"http://localhost:1420":   true, // `tauri dev` vite server
		"http://127.0.0.1:1420":   true,
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && allowed[origin] {
			h := c.Writer.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, x-api-key, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			// Vary matters because the value depends on the request's Origin;
			// without it a proxy could serve one origin's header to another.
			h.Add("Vary", "Origin")
		}
		if c.Request.Method == http.MethodOptions {
			// Answer the preflight before auth runs — a preflight carries no
			// Authorization header, so requiring auth here would always 401.
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// credentialFromHeaders pulls a bearer credential out of a request. Anthropic
// clients send x-api-key, OpenAI-style ones send Authorization: Bearer, and the
// desktop widget sends the former shape too — so every credential-checking
// middleware accepts both.
func credentialFromHeaders(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return strings.TrimSpace(c.GetHeader("x-api-key"))
}

// APIKeyAuth protects /v1/* endpoints with Bearer token OR session cookie.
// Validates against managed keys issued from the dashboard Keys page.
func APIKeyAuth(keyStore *auth.KeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if keyStore.Count() == 0 {
			c.Next()
			return
		}
		apiKey := credentialFromHeaders(c)

		if apiKey != "" {
			// Check managed keys first
			if kd := keyStore.Validate(apiKey); kd != nil {
				if kd.Disabled {
					c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
						"error": gin.H{"message": "api key disabled", "type": "invalid_request_error"},
					})
					return
				}
				c.Set("api_key_name", kd.Name)
				c.Set("api_key_id", kd.ID)
				c.Set("api_key_limit", kd.TokenLimitDaily)
				c.Set("api_key_request_limit", kd.RequestLimitDaily)
				c.Next()
				return
			}
		}
		// Fall back to session cookie (for dashboard chat test)
		if cookie, err := c.Cookie("session"); err == nil && sessions.Valid(cookie) {
			c.Set("api_key_name", "dashboard")
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{"message": "invalid api key", "type": "invalid_request_error"},
		})
	}
}

// TrayAuth protects /api/tray, the read-only snapshot the desktop widget polls.
//
// That endpoint is admin-plane data (account emails, quota headroom, per-key
// usage), so it deliberately does NOT accept the managed /v1 API keys: a
// credential that can spend quota and a credential that can read the console
// are different things, and letting one stand in for the other is exactly the
// confusion this middleware exists to undo. It accepts:
//
//   - server.tray_token, via Authorization: Bearer or x-api-key
//   - a live dashboard session cookie, so the endpoint stays debuggable in a
//     logged-in browser
//
// An empty tray_token locks the endpoint down instead of opening it — the
// opposite of APIKeyAuth's "no keys configured, let everyone through" rule,
// which on a fresh install would leave this data world-readable.
func TrayAuth(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Compared in constant time: the token is a fixed secret checked on a
		// public endpoint every 60s, which is the shape byte-by-byte timing
		// attacks like.
		if want := cfg.TrayToken(); want != "" {
			got := credentialFromHeaders(c)
			if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
				c.Next()
				return
			}
		}
		if cookie, err := c.Cookie("session"); err == nil && sessions.Valid(cookie) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "invalid tray token — generate one under Config → Admin → Tray token in the dashboard",
				"type":    "invalid_request_error",
			},
		})
	}
}

// TokenLimitCheck enforces daily token and request limits for managed API keys.
func TokenLimitCheck(statsDB *stats.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyName, _ := c.Get("api_key_name")
		name, _ := keyName.(string)
		if name == "" {
			c.Next()
			return
		}

		// Daily request-count limit
		if v, exists := c.Get("api_key_request_limit"); exists {
			if limit, ok := v.(int); ok && limit > 0 {
				used := statsDB.RequestsTodayForKey(name)
				if used >= limit {
					c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
						"error": gin.H{
							"message": fmt.Sprintf("daily request limit exceeded (%d/%d)", used, limit),
							"type":    "rate_limit_error",
						},
					})
					return
				}
			}
		}

		// Daily token limit
		if v, exists := c.Get("api_key_limit"); exists {
			if limit, ok := v.(int); ok && limit > 0 {
				used := statsDB.TokensTodayForKey(name)
				if used >= limit {
					c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
						"error": gin.H{
							"message": fmt.Sprintf("daily token limit exceeded (%d/%d)", used, limit),
							"type":    "rate_limit_error",
						},
					})
					return
				}
			}
		}

		c.Next()
	}
}

// SessionAuth protects dashboard and admin routes with cookie session.
func SessionAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Cookie("session")
		if err != nil || !sessions.Valid(cookie) {
			c.Redirect(http.StatusTemporaryRedirect, "/login")
			c.Abort()
			return
		}
		c.Next()
	}
}

// SessionAuthJSON protects admin API routes — returns 401 JSON instead of redirect.
func SessionAuthJSON() gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Cookie("session")
		if err != nil || !sessions.Valid(cookie) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		c.Next()
	}
}
