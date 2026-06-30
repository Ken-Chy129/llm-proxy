package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/Ken-Chy129/llm-proxy/internal/auth"
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

// APIKeyAuth protects /v1/* endpoints with Bearer token OR session cookie.
// Validates against managed keys issued from the dashboard Keys page.
func APIKeyAuth(keyStore *auth.KeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if keyStore.Count() == 0 {
			c.Next()
			return
		}
		// Extract key from headers
		apiKey := ""
		authHeader := c.GetHeader("Authorization")
		if t := strings.TrimPrefix(authHeader, "Bearer "); t != authHeader {
			apiKey = t
		}
		if apiKey == "" {
			apiKey = c.GetHeader("x-api-key")
		}

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

// TokenLimitCheck enforces daily token limits for managed API keys.
func TokenLimitCheck(statsDB *stats.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		limitVal, exists := c.Get("api_key_limit")
		if !exists {
			c.Next()
			return
		}
		limit, ok := limitVal.(int)
		if !ok || limit <= 0 {
			c.Next()
			return
		}
		keyName, _ := c.Get("api_key_name")
		name, _ := keyName.(string)
		if name == "" {
			c.Next()
			return
		}
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
