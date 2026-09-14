package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// contextKey is the key under which the verified claims are stored in the Gin
// context. A private named type prevents key collisions with other packages.
const contextKey = "auth.claims"

// Middleware verifies the Bearer token and aborts with 401 if it is missing or bad.
//
// gin.HandlerFunc is just func(*gin.Context); c.Next() continues the chain and
// c.Abort* stops it. Unlike NestJS guards there is no DI container — you build the
// middleware with its dependencies as a closure, which is the whole pattern.
func Middleware(m *Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := bearerToken(c.GetHeader("Authorization"))
		if err != nil {
			unauthorized(c, "missing or malformed Authorization header")
			return
		}

		claims, err := m.Verify(token)
		if err != nil {
			switch {
			case errors.Is(err, ErrExpiredToken):
				unauthorized(c, "token expired")
			default:
				unauthorized(c, "invalid token")
			}
			return
		}

		c.Set(contextKey, claims)
		c.Next()
	}
}

// RequireScope gates a route on a scope. Chain it after Middleware:
//
//	jobs.POST("", auth.RequireScope(auth.ScopeJobsWrite), h.createJob)
func RequireScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := ClaimsFrom(c)
		if !ok {
			unauthorized(c, "authentication required")
			return
		}
		if !claims.HasScope(scope) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":          "forbidden",
				"message":        "token is missing the required scope",
				"required_scope": scope,
			})
			return
		}
		c.Next()
	}
}

// ClaimsFrom pulls the verified claims out of the request context.
//
// The comma-ok pattern (value, ok := ...) is Go's stand-in for a nullable return —
// callers must acknowledge the "not there" case.
func ClaimsFrom(c *gin.Context) (*Claims, bool) {
	v, exists := c.Get(contextKey)
	if !exists {
		return nil, false
	}
	claims, ok := v.(*Claims) // type assertion: v is `any`
	return claims, ok
}

func bearerToken(header string) (string, error) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", errors.New("expected 'Bearer <token>'")
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", errors.New("empty token")
	}
	return token, nil
}

func unauthorized(c *gin.Context, message string) {
	// WWW-Authenticate is what makes a 401 spec-compliant, and curl/clients read it.
	c.Header("WWW-Authenticate", `Bearer realm="gotaskqueue"`)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error":   "unauthorized",
		"message": message,
	})
}
