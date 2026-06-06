package server

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

func min(a, b int) int { if a < b { return a }; return b }

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

// APIKeyAuth protects /v1/* endpoints with Bearer token OR session cookie.
func APIKeyAuth(apiKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if apiKey == "" {
			c.Next()
			return
		}
		// Check Bearer token first
		auth := c.GetHeader("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token != "" && token == apiKey {
			c.Next()
			return
		}
		// Check x-api-key (Anthropic API style)
		if xKey := c.GetHeader("x-api-key"); xKey == apiKey {
			c.Next()
			return
		}
		// Fall back to session cookie (for dashboard chat test)
		if cookie, err := c.Cookie("session"); err == nil && sessions.Valid(cookie) {
			c.Next()
			return
		}
		log.Printf("AUTH REJECTED: path=%s auth_header=%q token_len=%d expected_len=%d", c.Request.URL.Path, auth[:min(20, len(auth))], len(token), len(apiKey))
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{"message": "invalid api key", "type": "invalid_request_error"},
		})
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
