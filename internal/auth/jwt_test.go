package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mahalingam-dev-8/gotaskqueue/internal/config"
)

func testConfig() config.JWTConfig {
	return config.JWTConfig{
		Secret:   "test-secret-that-is-at-least-32-chars",
		Issuer:   "gotaskqueue",
		Audience: "gotaskqueue-api",
		TTL:      time.Hour,
	}
}

func TestIssueAndVerify(t *testing.T) {
	m := NewManager(testConfig())

	token, err := m.Issue("alice", []string{ScopeJobsRead, ScopeJobsWrite})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	claims, err := m.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "alice" {
		t.Errorf("subject = %q, want alice", claims.Subject)
	}
	if !claims.HasScope(ScopeJobsWrite) || claims.HasScope("admin") {
		t.Errorf("scopes = %v", claims.Scopes)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	issuer := NewManager(testConfig())

	other := testConfig()
	other.Secret = "a-completely-different-secret-32chars"
	verifier := NewManager(other)

	token, err := issuer.Issue("alice", nil)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := verifier.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	cfg := testConfig()
	cfg.TTL = -time.Minute // already expired
	m := NewManager(cfg)

	token, err := m.Issue("alice", nil)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := m.Verify(token); !errors.Is(err, ErrExpiredToken) {
		t.Errorf("err = %v, want ErrExpiredToken", err)
	}
}

// The classic JWT attack: re-sign the payload with alg "none" and hope the server
// does not pin the algorithm. jwt.WithValidMethods in Verify is what stops it.
func TestVerifyRejectsAlgNone(t *testing.T) {
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "attacker",
			Issuer:    "gotaskqueue",
			Audience:  jwt.ClaimStrings{"gotaskqueue-api"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Scopes: []string{ScopeJobsWrite},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("craft token: %v", err)
	}

	if _, err := NewManager(testConfig()).Verify(token); err == nil {
		t.Fatal("alg=none token was accepted")
	}
}

func TestVerifyRejectsWrongAudience(t *testing.T) {
	cfg := testConfig()
	issuerCfg := cfg
	issuerCfg.Audience = "some-other-api"

	token, err := NewManager(issuerCfg).Issue("alice", nil)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := NewManager(cfg).Verify(token); err == nil {
		t.Fatal("token for another audience was accepted")
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header  string
		want    string
		wantErr bool
	}{
		{header: "Bearer abc.def.ghi", want: "abc.def.ghi"},
		{header: "bearer abc.def.ghi", want: "abc.def.ghi"}, // case-insensitive scheme
		{header: "", wantErr: true},
		{header: "Basic abc", wantErr: true},
		{header: "Bearer ", wantErr: true},
	}

	for _, tc := range cases {
		got, err := bearerToken(tc.header)
		if tc.wantErr {
			if err == nil {
				t.Errorf("bearerToken(%q) = %q, want error", tc.header, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("bearerToken(%q) = (%q, %v), want %q", tc.header, got, err, tc.want)
		}
	}
}
