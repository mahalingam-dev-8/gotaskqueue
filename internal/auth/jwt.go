// Package auth issues and verifies the HS256 JWTs that protect the job endpoints.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/mahalingam-dev-8/gotaskqueue/internal/config"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("token expired")
)

// Scopes recognised by the API.
const (
	ScopeJobsWrite = "jobs:write"
	ScopeJobsRead  = "jobs:read"
)

// Claims is our JWT body. Embedding jwt.RegisteredClaims gives us the standard
// fields (sub, iss, aud, exp, iat, jti) plus the validation methods the library
// calls, and we add our own scopes on top.
type Claims struct {
	jwt.RegisteredClaims
	Scopes []string `json:"scopes,omitempty"`
}

// HasScope reports whether the token carries scope.
func (c *Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Manager signs and verifies tokens. One instance is shared by the whole process.
type Manager struct {
	secret   []byte
	issuer   string
	audience string
	ttl      time.Duration
}

func NewManager(cfg config.JWTConfig) *Manager {
	return &Manager{
		secret:   []byte(cfg.Secret),
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		ttl:      cfg.TTL,
	}
}

// Issue mints a token for subject with the given scopes.
//
// In a real deployment your IdP (Cognito, Auth0, an internal auth service) issues
// these and the API only verifies. Issue exists so you can test locally — see
// cmd/token.
func (m *Manager) Issue(subject string, scopes []string) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			Issuer:    m.issuer,
			Audience:  jwt.ClaimStrings{m.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
			ID:        uuid.NewString(),
		},
		Scopes: scopes,
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// Verify parses and validates a token string.
//
// jwt.WithValidMethods is not optional: without it an attacker can hand you a token
// with alg "none" (or swap HS256/RS256) and the library will happily accept it.
// This is the single most commonly exploited JWT bug.
func (m *Manager) Verify(tokenString string) (*Claims, error) {
	claims := &Claims{}

	_, err := jwt.ParseWithClaims(tokenString, claims,
		func(t *jwt.Token) (any, error) { return m.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(m.issuer),
		jwt.WithAudience(m.audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}
	return claims, nil
}
