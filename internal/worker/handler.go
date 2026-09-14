// Package worker runs a bounded pool of goroutines that claim jobs from the queue
// and execute registered handlers.
package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/mahalingam-dev-8/gotaskqueue/internal/queue"
)

// Handler processes one job. Returning nil means success.
//
// The ctx passed in carries the per-job timeout — pass it down into every network
// call you make, the way you'd thread an AbortSignal through in Node.
type Handler interface {
	Handle(ctx context.Context, job queue.Job) error
}

// HandlerFunc lets a plain function satisfy Handler.
//
// Go idiom: a named func type with a method on it (cf. http.HandlerFunc). It means
// callers can pass either a closure or a struct with dependencies, with no wrapper.
type HandlerFunc func(ctx context.Context, job queue.Job) error

func (f HandlerFunc) Handle(ctx context.Context, job queue.Job) error { return f(ctx, job) }

// ErrNoHandler is returned when a job type has no registered handler. It is treated
// as permanent — retrying will not conjure a handler into existence.
var ErrNoHandler = errors.New("no handler registered for job type")

// Registry maps job type -> handler.
//
// The mutex makes it safe to register from one goroutine while workers read; in
// practice everything is registered before Run, but a zero-cost guard is cheaper
// than a data race at 3am. Reads take RLock so N workers look up concurrently.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]Handler)}
}

// Register binds a handler to a job type, panicking on a duplicate. Panicking at
// startup for a programmer error is idiomatic; panicking at request time is not.
func (r *Registry) Register(jobType string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.handlers[jobType]; exists {
		panic(fmt.Sprintf("worker: duplicate handler registered for job type %q", jobType))
	}
	r.handlers[jobType] = h
}

// RegisterFunc is the closure-flavoured Register.
func (r *Registry) RegisterFunc(jobType string, f HandlerFunc) { r.Register(jobType, f) }

func (r *Registry) get(jobType string) (Handler, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	h, ok := r.handlers[jobType]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoHandler, jobType)
	}
	return h, nil
}

// Types lists the registered job types (for startup logs).
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		types = append(types, t)
	}
	return types
}

// permanentError marks a failure that must not be retried: malformed payload,
// 404 from an upstream, business-rule rejection. The pool buries these immediately
// instead of burning all max_attempts on something that can never succeed.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }

// Unwrap is what makes errors.Is/errors.As see through the wrapper.
func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the pool fails the job without retrying.
//
//	return worker.Permanent(fmt.Errorf("invalid payload: %w", err))
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// IsPermanent reports whether err (or anything it wraps) is permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p) || errors.Is(err, ErrNoHandler)
}
