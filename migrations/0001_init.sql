-- 0001_init.sql
-- Core job table for GoTaskQueue.
--
-- Design notes:
--   * status is TEXT + CHECK rather than a PG ENUM: adding a new status later is a
--     one-line constraint change instead of an ALTER TYPE dance.
--   * run_at is what makes retries work. A failed job goes back to 'pending' with
--     run_at = now() + backoff, so the dequeue query simply ignores it until then.
--   * locked_at / locked_by are for stuck-job recovery and debugging ("which worker
--     had this job when the container died?").

CREATE EXTENSION IF NOT EXISTS pgcrypto; -- gen_random_uuid() on PG < 13

CREATE TABLE IF NOT EXISTS jobs (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    type         TEXT        NOT NULL,
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    attempts     INTEGER     NOT NULL DEFAULT 0,
    max_attempts INTEGER     NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    last_error   TEXT,
    run_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_at    TIMESTAMPTZ,
    locked_by    TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The hot path: "give me the oldest runnable pending job".
-- Partial index keeps it small — completed/failed rows are not in it at all.
CREATE INDEX IF NOT EXISTS idx_jobs_pending_run_at
    ON jobs (run_at, created_at)
    WHERE status = 'pending';

-- Used by the stuck-job reaper.
CREATE INDEX IF NOT EXISTS idx_jobs_processing_locked_at
    ON jobs (locked_at)
    WHERE status = 'processing';

-- Handy for dashboards / debugging.
CREATE INDEX IF NOT EXISTS idx_jobs_type_status ON jobs (type, status);
