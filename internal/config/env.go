package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// loader reads environment variables and accumulates every problem it finds
// instead of bailing on the first one. Starting a service with five bad env vars
// and being told about one per restart is miserable.
//
// Go idiom: there is no exceptions mechanism, so "collect errors, return once" is
// a common pattern. errors.Join (Go 1.20+) wraps them into a single error value.
type loader struct {
	errs []error
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

// err returns nil when nothing went wrong — the zero value of a slice is nil,
// and len(nil) == 0, so no initialisation is needed anywhere.
func (l *loader) err() error {
	return errors.Join(l.errs...)
}

func (l *loader) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func (l *loader) required(key string) string {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		l.fail("%s is required", key)
	}
	return v
}

func (l *loader) requiredMinLen(key string, minLen int) string {
	v := l.required(key)
	if v != "" && len(v) < minLen {
		l.fail("%s must be at least %d characters", key, minLen)
	}
	return v
}

func (l *loader) int(key string, def int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		l.fail("%s: %q is not an integer", key, raw)
		return def
	}
	return v
}

func (l *loader) intRange(key string, def, minV, maxV int) int {
	v := l.int(key, def)
	if v < minV || v > maxV {
		l.fail("%s: %d is out of range [%d, %d]", key, v, minV, maxV)
		return def
	}
	return v
}

// dur parses Go duration strings: "5s", "1m30s", "10m", "2h".
func (l *loader) dur(key string, def time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		l.fail("%s: %q is not a duration (e.g. 30s, 5m, 1h)", key, raw)
		return def
	}
	if v <= 0 {
		l.fail("%s must be positive, got %s", key, v)
		return def
	}
	return v
}

func (l *loader) bool(key string, def bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		l.fail("%s: %q is not a boolean", key, raw)
		return def
	}
	return v
}
