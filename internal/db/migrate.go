package db

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is an arbitrary but fixed key for pg_advisory_lock. When the API
// and the worker boot at the same time (very normal on ECS), only one of them gets
// the lock and actually applies migrations; the other waits and then finds there is
// nothing to do.
const migrationLockID int64 = 4815162342

// Migrate applies every *.sql file in fsys, in filename order, exactly once.
//
// This is deliberately a ~60 line runner instead of a dependency: the semantics
// (ordered, once, in a transaction, behind a lock) are the whole feature.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	// defer runs when the function returns, no matter which return path — Go's
	// try/finally. Unlocking on the same connection is required for advisory locks.
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID); err != nil {
			log.Error("release advisory lock", slog.Any("error", err))
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT        PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	files, err := sqlFiles(fsys)
	if err != nil {
		return err
	}

	for _, name := range files {
		if _, ok := applied[name]; ok {
			continue
		}

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		// Each migration runs in its own transaction: a failure rolls back cleanly
		// and the version row is never written, so a fixed migration re-runs.
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("exec: %w", err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		log.Info("migration applied", slog.String("version", name))
	}
	return nil
}

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[string]struct{}, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	// map[string]struct{} is Go's set: struct{} occupies zero bytes.
	applied := make(map[string]struct{})
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = struct{}{}
	}
	return applied, rows.Err()
}

func sqlFiles(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // zero-padded prefixes (0001_, 0002_) sort correctly as strings
	return names, nil
}
