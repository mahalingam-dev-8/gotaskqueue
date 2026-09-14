// Package config loads all runtime configuration from environment variables.
//
// There are two entrypoints with different needs (the API needs JWT settings, the
// worker needs pool settings), so there are two Load functions returning two
// structs that embed a shared Base. Embedding gives you cfg.DB.URL directly on the
// outer struct without writing a forwarding method — Go's version of composition.
package config

import (
	"fmt"
	"time"
)

type Base struct {
	Env string // development | staging | production
	Log LogConfig
	DB  DBConfig
}

func (b Base) IsProduction() bool { return b.Env == "production" }

type LogConfig struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

type DBConfig struct {
	URL             string
	MaxConns        int
	MinConns        int
	ConnMaxLifetime time.Duration
	ConnectTimeout  time.Duration
	RunMigrations   bool
}

type HTTPConfig struct {
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

func (h HTTPConfig) Addr() string { return fmt.Sprintf(":%d", h.Port) }

type JWTConfig struct {
	Secret   string
	Issuer   string
	Audience string
	TTL      time.Duration
}

type WorkerConfig struct {
	// PoolSize is the number of goroutines processing jobs concurrently.
	PoolSize int
	// PollInterval is how long the dispatcher sleeps when the queue is empty.
	PollInterval time.Duration
	// JobTimeout bounds a single job execution.
	JobTimeout time.Duration
	// BaseBackoff / MaxBackoff drive exponential retry backoff.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// StuckJobTimeout: a job 'processing' for longer than this is assumed orphaned.
	StuckJobTimeout time.Duration
	// RecoveryInterval is how often the reaper runs.
	RecoveryInterval time.Duration
	// ShutdownGrace bounds how long we wait for in-flight jobs on SIGTERM.
	ShutdownGrace time.Duration
	// ID identifies this worker process in jobs.locked_by. Defaults to the hostname.
	ID string
}

type APIConfig struct {
	Base
	HTTP               HTTPConfig
	JWT                JWTConfig
	DefaultMaxAttempts int
}

type WorkerProcessConfig struct {
	Base
	Worker WorkerConfig
}

func (l *loader) base() Base {
	return Base{
		Env: l.str("APP_ENV", "development"),
		Log: LogConfig{
			Level:  l.str("LOG_LEVEL", "info"),
			Format: l.str("LOG_FORMAT", "json"),
		},
		DB: DBConfig{
			URL:             l.required("DATABASE_URL"),
			MaxConns:        l.intRange("DB_MAX_CONNS", 10, 1, 500),
			MinConns:        l.intRange("DB_MIN_CONNS", 2, 0, 500),
			ConnMaxLifetime: l.dur("DB_CONN_MAX_LIFETIME", 30*time.Minute),
			ConnectTimeout:  l.dur("DB_CONNECT_TIMEOUT", 10*time.Second),
			RunMigrations:   l.bool("RUN_MIGRATIONS", true),
		},
	}
}

// LoadAPI reads the configuration needed by cmd/api.
func LoadAPI() (*APIConfig, error) {
	l := &loader{}
	cfg := &APIConfig{
		Base: l.base(),
		HTTP: HTTPConfig{
			Port:            l.intRange("HTTP_PORT", 8080, 1, 65535),
			ReadTimeout:     l.dur("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    l.dur("HTTP_WRITE_TIMEOUT", 20*time.Second),
			IdleTimeout:     l.dur("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout: l.dur("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
		},
		JWT: JWTConfig{
			// 32 bytes minimum for HS256 — short secrets are the usual way JWT auth
			// gets broken in practice.
			Secret:   l.requiredMinLen("JWT_SECRET", 32),
			Issuer:   l.str("JWT_ISSUER", "gotaskqueue"),
			Audience: l.str("JWT_AUDIENCE", "gotaskqueue-api"),
			TTL:      l.dur("JWT_TTL", time.Hour),
		},
		DefaultMaxAttempts: l.intRange("DEFAULT_MAX_ATTEMPTS", 5, 1, 50),
	}
	if err := l.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadWorker reads the configuration needed by cmd/worker.
func LoadWorker() (*WorkerProcessConfig, error) {
	l := &loader{}
	cfg := &WorkerProcessConfig{
		Base: l.base(),
		Worker: WorkerConfig{
			PoolSize:         l.intRange("WORKER_POOL_SIZE", 5, 1, 1000),
			PollInterval:     l.dur("WORKER_POLL_INTERVAL", time.Second),
			JobTimeout:       l.dur("WORKER_JOB_TIMEOUT", 30*time.Second),
			BaseBackoff:      l.dur("RETRY_BASE_BACKOFF", 5*time.Second),
			MaxBackoff:       l.dur("RETRY_MAX_BACKOFF", 10*time.Minute),
			StuckJobTimeout:  l.dur("STUCK_JOB_TIMEOUT", 5*time.Minute),
			RecoveryInterval: l.dur("RECOVERY_INTERVAL", time.Minute),
			ShutdownGrace:    l.dur("WORKER_SHUTDOWN_GRACE", 30*time.Second),
			ID:               l.str("WORKER_ID", ""), // filled in from hostname if empty
		},
	}
	if err := l.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}
