// Command token mints a local development JWT so you can curl the API.
//
//	go run ./cmd/token -sub dev-user
//	docker compose run --rm token -sub dev-user -ttl 24h
//
// In a real deployment tokens come from your IdP; this exists only so the local
// docker-compose stack is usable without one.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mahalingam-dev-8/gotaskqueue/internal/auth"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/config"
)

func main() {
	var (
		subject = flag.String("sub", "local-dev", "token subject (who the token is for)")
		scopes  = flag.String("scopes", "jobs:read,jobs:write", "comma-separated scopes")
		ttl     = flag.Duration("ttl", time.Hour, "token lifetime, e.g. 30m, 24h")
	)
	flag.Parse()

	secret := os.Getenv("JWT_SECRET")
	if len(secret) < 32 {
		fmt.Fprintln(os.Stderr, "JWT_SECRET must be set and at least 32 characters")
		os.Exit(1)
	}

	manager := auth.NewManager(config.JWTConfig{
		Secret:   secret,
		Issuer:   envOr("JWT_ISSUER", "gotaskqueue"),
		Audience: envOr("JWT_AUDIENCE", "gotaskqueue-api"),
		TTL:      *ttl,
	})

	token, err := manager.Issue(*subject, strings.Split(*scopes, ","))
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue token:", err)
		os.Exit(1)
	}
	fmt.Println(token)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
