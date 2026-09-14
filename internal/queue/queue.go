package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// jobColumns is listed explicitly (never SELECT *) so adding a column later cannot
// silently break the scan order below.
const jobColumns = `id, type, payload, status, attempts, max_attempts,
	last_error, run_at, locked_at, locked_by, created_at, updated_at`

// Same column list, qualified with the table alias used by the UPDATE ... FROM in
// Dequeue. Keep the two lists in the same order — scanJob depends on it.
const jobColumnsAliased = `j.id, j.type, j.payload, j.status, j.attempts, j.max_attempts,
	j.last_error, j.run_at, j.locked_at, j.locked_by, j.created_at, j.updated_at`

// Queue is the data-access layer for the jobs table. It is safe for concurrent use
// because pgxpool.Pool is.
type Queue struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool}
}

// EnqueueParams describes a job to insert.
type EnqueueParams struct {
	Type        string
	Payload     json.RawMessage
	MaxAttempts int
	// Delay postpones the first attempt. Zero means "runnable immediately".
	Delay time.Duration
}

// Enqueue inserts a new pending job and returns it.
func (q *Queue) Enqueue(ctx context.Context, p EnqueueParams) (*Job, error) {
	if p.Type == "" {
		return nil, errors.New("job type is required")
	}
	if len(p.Payload) == 0 {
		p.Payload = json.RawMessage(`{}`)
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 5
	}

	const query = `
		INSERT INTO jobs (type, payload, max_attempts, run_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		RETURNING ` + jobColumns

	row := q.pool.QueryRow(ctx, query, p.Type, []byte(p.Payload), p.MaxAttempts, p.Delay.Seconds())
	job, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("enqueue: %w", err)
	}
	return job, nil
}

// Dequeue atomically claims up to limit runnable jobs for workerID.
//
// The CTE selects candidate rows with FOR UPDATE SKIP LOCKED: rows already locked
// by another worker's in-flight transaction are skipped rather than waited on, so
// two workers polling at the same millisecond get disjoint sets of jobs. The outer
// UPDATE flips them to 'processing' in the same transaction (a bare statement is
// its own transaction in Postgres), so the claim is atomic and durable.
//
// attempts is incremented here, at claim time, not at failure time. That way a job
// whose worker is SIGKILLed still burns an attempt when the reaper requeues it —
// otherwise a job that reliably crashes its worker would retry forever.
func (q *Queue) Dequeue(ctx context.Context, workerID string, limit int) ([]Job, error) {
	if limit <= 0 {
		return nil, nil
	}

	const query = `
		WITH claimed AS (
			SELECT id
			FROM jobs
			WHERE status = 'pending' AND run_at <= now()
			ORDER BY run_at, created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE jobs j
		SET status     = 'processing',
			attempts   = j.attempts + 1,
			locked_at  = now(),
			locked_by  = $2,
			updated_at = now()
		FROM claimed c
		WHERE j.id = c.id
		RETURNING ` + jobColumnsAliased

	rows, err := q.pool.Query(ctx, query, limit, workerID)
	if err != nil {
		return nil, fmt.Errorf("dequeue: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("dequeue scan: %w", err)
		}
		jobs = append(jobs, *job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dequeue rows: %w", err)
	}
	return jobs, nil
}

// Get returns a single job by id.
func (q *Queue) Get(ctx context.Context, id uuid.UUID) (*Job, error) {
	const query = `SELECT ` + jobColumns + ` FROM jobs WHERE id = $1::uuid`

	job, err := scanJob(q.pool.QueryRow(ctx, query, id.String()))
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

// Complete marks a claimed job as done.
//
// The `AND status = 'processing'` guard matters: if the reaper decided this job was
// stuck and handed it to someone else, we must not overwrite that. A zero row count
// means we lost the claim, which the caller logs rather than treating as success.
func (q *Queue) Complete(ctx context.Context, id uuid.UUID) error {
	const query = `
		UPDATE jobs
		SET status = 'completed', last_error = NULL, locked_at = NULL, locked_by = NULL, updated_at = now()
		WHERE id = $1::uuid AND status = 'processing'`

	tag, err := q.pool.Exec(ctx, query, id.String())
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Retry schedules another attempt after delay.
func (q *Queue) Retry(ctx context.Context, id uuid.UUID, cause string, delay time.Duration) error {
	const query = `
		UPDATE jobs
		SET status     = 'pending',
			run_at     = now() + make_interval(secs => $2),
			last_error = $3,
			locked_at  = NULL,
			locked_by  = NULL,
			updated_at = now()
		WHERE id = $1::uuid AND status = 'processing'`

	tag, err := q.pool.Exec(ctx, query, id.String(), delay.Seconds(), truncate(cause, 4000))
	if err != nil {
		return fmt.Errorf("retry job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Fail marks a job dead — attempts exhausted, or a permanent error.
func (q *Queue) Fail(ctx context.Context, id uuid.UUID, cause string) error {
	const query = `
		UPDATE jobs
		SET status = 'failed', last_error = $2, locked_at = NULL, locked_by = NULL, updated_at = now()
		WHERE id = $1::uuid AND status = 'processing'`

	tag, err := q.pool.Exec(ctx, query, id.String(), truncate(cause, 4000))
	if err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Release hands claimed-but-never-started jobs straight back to the queue. Used on
// shutdown for jobs the dispatcher pulled but had no worker left to run, so they
// are picked up immediately instead of waiting for the stuck-job timeout.
// The attempt is refunded because the handler never ran.
func (q *Queue) Release(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}

	const query = `
		UPDATE jobs
		SET status     = 'pending',
			attempts   = GREATEST(attempts - 1, 0),
			run_at     = now(),
			locked_at  = NULL,
			locked_by  = NULL,
			updated_at = now()
		WHERE id = ANY($1::uuid[]) AND status = 'processing'`

	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	if _, err := q.pool.Exec(ctx, query, strs); err != nil {
		return fmt.Errorf("release jobs: %w", err)
	}
	return nil
}

// RecoveryResult reports what the reaper did in one pass.
type RecoveryResult struct {
	Requeued int
	Failed   int
}

// RecoverStuck rescues jobs left in 'processing' by a worker that died (OOM kill,
// ECS task replacement, network partition). Anything locked longer than timeout is
// requeued — or buried if it has already used all its attempts.
//
// This is the safety net that makes at-least-once delivery actually hold: nothing
// stays claimed forever just because a process vanished.
func (q *Queue) RecoverStuck(ctx context.Context, timeout time.Duration) (RecoveryResult, error) {
	const query = `
		UPDATE jobs
		SET status = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
			run_at     = now(),
			last_error = COALESCE(last_error, 'stuck in processing; worker presumed dead'),
			locked_at  = NULL,
			locked_by  = NULL,
			updated_at = now()
		WHERE status = 'processing'
		  AND locked_at < now() - make_interval(secs => $1)
		RETURNING status`

	rows, err := q.pool.Query(ctx, query, timeout.Seconds())
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("recover stuck jobs: %w", err)
	}
	defer rows.Close()

	var res RecoveryResult
	for rows.Next() {
		var status Status
		if err := rows.Scan(&status); err != nil {
			return res, fmt.Errorf("recover scan: %w", err)
		}
		if status == StatusFailed {
			res.Failed++
		} else {
			res.Requeued++
		}
	}
	return res, rows.Err()
}

// CountByStatus is a cheap snapshot for dashboards and smoke tests.
func (q *Queue) CountByStatus(ctx context.Context) (map[Status]int, error) {
	rows, err := q.pool.Query(ctx, `SELECT status, count(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count by status: %w", err)
	}
	defer rows.Close()

	counts := make(map[Status]int)
	for rows.Next() {
		var (
			s Status
			n int
		)
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		counts[s] = n
	}
	return counts, rows.Err()
}

// scanJob reads one row into a Job.
//
// pgx.Row and pgx.Rows both satisfy this tiny interface, so the same function serves
// QueryRow (single) and Query (loop) call sites — accepting the smallest interface
// you need is very idiomatic Go.
func scanJob(row interface{ Scan(dest ...any) error }) (*Job, error) {
	var (
		j     Job
		rawID string // scan UUIDs as text and parse: no extra pgx type registration needed
	)

	err := row.Scan(
		&rawID,
		&j.Type,
		&j.Payload,
		&j.Status,
		&j.Attempts,
		&j.MaxAttempts,
		&j.LastError,
		&j.RunAt,
		&j.LockedAt,
		&j.LockedBy,
		&j.CreatedAt,
		&j.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	id, err := uuid.Parse(rawID)
	if err != nil {
		return nil, fmt.Errorf("parse job id %q: %w", rawID, err)
	}
	j.ID = id
	return &j, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
