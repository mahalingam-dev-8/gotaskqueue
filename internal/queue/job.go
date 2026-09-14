// Package queue implements a PostgreSQL-backed job queue.
//
// The whole design rests on one Postgres feature: SELECT ... FOR UPDATE SKIP LOCKED.
// It lets N workers each grab a *different* row without blocking each other and
// without a distributed lock service. Postgres is the broker.
package queue

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when a job id does not exist, or when an update
// targeted a job this worker no longer owns (e.g. the reaper took it back).
//
// Go idiom: exported sentinel errors are compared with errors.Is(err, ErrNotFound),
// which walks the %w wrapping chain — the rough equivalent of instanceof on a
// custom Error subclass in TS.
var ErrNotFound = errors.New("job not found")

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// Job mirrors one row of the jobs table.
type Job struct {
	ID          uuid.UUID
	Type        string
	Payload     json.RawMessage
	Status      Status
	Attempts    int
	MaxAttempts int
	LastError   *string // NULL-able column -> pointer; nil means "never failed"
	RunAt       time.Time
	LockedAt    *time.Time
	LockedBy    *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AttemptsRemaining reports how many tries are left after the current one.
func (j Job) AttemptsRemaining() int {
	if j.Attempts >= j.MaxAttempts {
		return 0
	}
	return j.MaxAttempts - j.Attempts
}

// DecodePayload unmarshals the JSONB payload into v (a pointer to your typed struct).
//
// Generics would also work here, but a plain method keeps the call site obvious:
//
//	var p EmailPayload
//	if err := job.DecodePayload(&p); err != nil { ... }
func (j Job) DecodePayload(v any) error {
	if len(j.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(j.Payload, v)
}

// Backoff returns the delay before the next attempt: base * 2^(attempt-1), capped
// at maxDelay, with ±20% jitter so a batch of simultaneous failures does not
// retry in lockstep (the "thundering herd" you get with pure exponential backoff).
//
// attempt is 1-based: the delay after the 1st failed attempt is ~base.
func Backoff(attempt int, base, maxDelay time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Cap the exponent before the shift so we never overflow the int64 nanoseconds.
	exp := math.Min(float64(attempt-1), 30)
	delay := time.Duration(float64(base) * math.Pow(2, exp))
	if delay <= 0 || delay > maxDelay {
		delay = maxDelay
	}

	jitter := 1 + (rand.Float64()*0.4 - 0.2) // [0.8, 1.2)
	delay = time.Duration(float64(delay) * jitter)
	if delay > maxDelay {
		delay = maxDelay
	}
	if delay < 0 {
		delay = base
	}
	return delay
}
