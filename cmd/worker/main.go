// Command worker is the consumer: a pool of goroutines that claim jobs from
// Postgres and run the registered handlers.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mahalingam-dev-8/gotaskqueue/internal/config"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/db"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/jobs"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/logging"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/queue"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/worker"
	"github.com/mahalingam-dev-8/gotaskqueue/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.Worker.ID == "" {
		// On ECS the hostname is the container id, which is exactly what you want in
		// jobs.locked_by when you are working out which task stalled.
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "worker"
		}
		cfg.Worker.ID = host
	}

	log := logging.NewWithService(cfg.Log.Level, cfg.Log.Format, "worker").
		With(slog.String("worker_id", cfg.Worker.ID))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DB)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	if cfg.DB.RunMigrations {
		if err := db.Migrate(ctx, pool, migrations.FS, log); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
	}

	// Each busy worker holds a connection while it writes its result, plus the
	// dispatcher and the reaper. Under-sizing the pool shows up as mysterious
	// latency, not as an error, so say it out loud at boot.
	if cfg.DB.MaxConns < cfg.Worker.PoolSize+2 {
		log.Warn("DB_MAX_CONNS is smaller than WORKER_POOL_SIZE+2; workers will queue for connections",
			slog.Int("db_max_conns", cfg.DB.MaxConns),
			slog.Int("pool_size", cfg.Worker.PoolSize))
	}

	registry := worker.NewRegistry()
	jobs.Register(registry, log)

	// Run blocks until ctx is cancelled and the pool has drained.
	if err := worker.New(queue.New(pool), registry, cfg.Worker, log).Run(ctx); err != nil {
		return fmt.Errorf("worker pool: %w", err)
	}

	log.Info("worker stopped")
	return nil
}
