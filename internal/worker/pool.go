package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ekkomd/gotaskqueue/internal/config"
	"github.com/ekkomd/gotaskqueue/internal/queue"
)

// dbOpTimeout bounds the status-update writes that follow a job. These run on a
// context detached from shutdown, so they need a deadline of their own.
const dbOpTimeout = 10 * time.Second

// Pool is the consumer side of the system:
//
//	dispatcher goroutine ──> jobs channel ──> N worker goroutines
//	        │                                        │
//	        └── claims from Postgres                 └── runs handler, writes result
//
// Concurrency is bounded two ways: there are exactly PoolSize worker goroutines,
// and the dispatcher never claims more jobs than there are idle workers.
type Pool struct {
	queue    *queue.Queue
	registry *Registry
	cfg      config.WorkerConfig
	log      *slog.Logger

	// inFlight counts jobs claimed from Postgres but not yet finished. Written by
	// the dispatcher and by every worker, hence atomic rather than a plain int.
	inFlight atomic.Int64
}

func New(q *queue.Queue, registry *Registry, cfg config.WorkerConfig, log *slog.Logger) *Pool {
	return &Pool{queue: q, registry: registry, cfg: cfg, log: log}
}

// Run starts the pool and blocks until ctx is cancelled and shutdown completes.
//
// Shutdown sequence on SIGINT/SIGTERM:
//  1. the dispatcher stops claiming new jobs and returns any it holds undelivered;
//  2. the jobs channel is closed, so each worker exits after its current job;
//  3. we wait up to ShutdownGrace for in-flight jobs to finish.
//
// In-flight jobs deliberately do NOT see the cancellation — see process().
func (p *Pool) Run(ctx context.Context) error {
	p.log.Info("worker pool starting",
		slog.Int("size", p.cfg.PoolSize),
		slog.String("worker_id", p.cfg.ID),
		slog.Any("job_types", p.registry.Types()),
		slog.Duration("poll_interval", p.cfg.PollInterval),
		slog.Duration("job_timeout", p.cfg.JobTimeout),
	)

	// Unbuffered: a send only completes when a worker is actually ready to receive.
	jobs := make(chan queue.Job)

	var workers sync.WaitGroup
	for i := 1; i <= p.cfg.PoolSize; i++ {
		workers.Add(1)
		go func(id int) {
			defer workers.Done()
			p.workerLoop(ctx, id, jobs)
		}(i)
	}

	var reaper sync.WaitGroup
	reaper.Add(1)
	go func() {
		defer reaper.Done()
		p.reapStuckJobs(ctx)
	}()

	// The dispatcher runs on this goroutine and returns when ctx is cancelled.
	p.dispatch(ctx, jobs)

	// Only the dispatcher sends on jobs, so it is the one that closes it. Closing
	// makes every worker's `range` loop end once the channel drains.
	close(jobs)
	reaper.Wait()

	p.log.Info("draining in-flight jobs",
		slog.Int64("in_flight", p.inFlight.Load()),
		slog.Duration("grace", p.cfg.ShutdownGrace))

	drained := make(chan struct{})
	go func() {
		workers.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		p.log.Info("worker pool stopped cleanly")
		return nil
	case <-time.After(p.cfg.ShutdownGrace):
		// Abandoned jobs stay 'processing' and are rescued by RecoverStuck later.
		p.log.Warn("shutdown grace expired; abandoning in-flight jobs",
			slog.Int64("in_flight", p.inFlight.Load()))
		return fmt.Errorf("shutdown grace period (%s) expired with %d jobs in flight",
			p.cfg.ShutdownGrace, p.inFlight.Load())
	}
}

// dispatch polls Postgres and feeds claimed jobs to the workers.
func (p *Pool) dispatch(ctx context.Context, out chan<- queue.Job) {
	for {
		if ctx.Err() != nil {
			p.log.Info("dispatcher stopping")
			return
		}

		capacity := p.cfg.PoolSize - int(p.inFlight.Load())
		if capacity <= 0 {
			// Everyone is busy; don't claim rows we cannot start.
			if !sleep(ctx, p.cfg.PollInterval) {
				return
			}
			continue
		}

		claimed, err := p.queue.Dequeue(ctx, p.cfg.ID, capacity)
		if err != nil {
			if ctx.Err() != nil {
				return // the error is just the cancelled context unwinding
			}
			p.log.Error("dequeue failed", slog.Any("error", err))
			if !sleep(ctx, p.cfg.PollInterval) {
				return
			}
			continue
		}

		if len(claimed) == 0 {
			if !sleep(ctx, p.cfg.PollInterval) {
				return
			}
			continue
		}

		p.inFlight.Add(int64(len(claimed)))

		for i, job := range claimed {
			select {
			case out <- job:
			case <-ctx.Done():
				// Shutdown landed mid-handoff: give back what nobody has started.
				undelivered := claimed[i:]
				p.inFlight.Add(-int64(len(undelivered)))
				p.releaseUndelivered(ctx, undelivered)
				return
			}
		}
	}
}

func (p *Pool) workerLoop(ctx context.Context, id int, in <-chan queue.Job) {
	log := p.log.With(slog.Int("worker", id))
	log.Debug("worker started")

	// Ranging over a channel receives until it is closed and drained — this is the
	// whole shutdown mechanism for workers. No stop flag, no extra channel.
	for job := range in {
		p.process(ctx, log, job)
	}
	log.Debug("worker stopped")
}

// process runs one job and records the outcome.
func (p *Pool) process(shutdownCtx context.Context, log *slog.Logger, job queue.Job) {
	defer p.inFlight.Add(-1)

	log = log.With(
		slog.String("job_id", job.ID.String()),
		slog.String("job_type", job.Type),
		slog.Int("attempt", job.Attempts),
		slog.Int("max_attempts", job.MaxAttempts),
	)

	// context.WithoutCancel (Go 1.21+) keeps the values of the parent context but
	// drops its cancellation. That is exactly "graceful shutdown": SIGTERM stops us
	// claiming new work, but a job already running gets to finish on its own
	// deadline instead of being cancelled halfway through a payment call.
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(shutdownCtx), p.cfg.JobTimeout)
	defer cancel()

	start := time.Now()
	err := p.runHandler(jobCtx, job)
	elapsed := time.Since(start)

	// Fresh context for the bookkeeping write: jobCtx may already be expired.
	dbCtx, dbCancel := context.WithTimeout(context.WithoutCancel(shutdownCtx), dbOpTimeout)
	defer dbCancel()

	switch {
	case err == nil:
		if err := p.queue.Complete(dbCtx, job.ID); err != nil {
			p.logClaimLoss(log, "complete", err)
			return
		}
		log.Info("job completed", slog.Duration("duration", elapsed))

	case IsPermanent(err) || job.Attempts >= job.MaxAttempts:
		reason := "attempts exhausted"
		if IsPermanent(err) {
			reason = "permanent error"
		}
		if failErr := p.queue.Fail(dbCtx, job.ID, err.Error()); failErr != nil {
			p.logClaimLoss(log, "fail", failErr)
			return
		}
		log.Error("job failed permanently",
			slog.String("reason", reason),
			slog.Duration("duration", elapsed),
			slog.Any("error", err))

	default:
		delay := queue.Backoff(job.Attempts, p.cfg.BaseBackoff, p.cfg.MaxBackoff)
		if retryErr := p.queue.Retry(dbCtx, job.ID, err.Error(), delay); retryErr != nil {
			p.logClaimLoss(log, "retry", retryErr)
			return
		}
		log.Warn("job failed; scheduled for retry",
			slog.Duration("duration", elapsed),
			slog.Duration("retry_in", delay),
			slog.Any("error", err))
	}
}

// runHandler looks up the handler and executes it, converting a panic into an error.
//
// The named return value (err error) is what lets the deferred closure overwrite
// the returned error after a panic — you cannot do that with an anonymous result.
func (p *Pool) runHandler(ctx context.Context, job queue.Job) (err error) {
	handler, err := p.registry.get(job.Type)
	if err != nil {
		return err // ErrNoHandler is permanent
	}

	defer func() {
		if r := recover(); r != nil {
			p.log.Error("handler panicked",
				slog.String("job_id", job.ID.String()),
				slog.String("job_type", job.Type),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()

	if err := handler.Handle(ctx, job); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("job timed out after %s: %w", p.cfg.JobTimeout, err)
		}
		return err
	}
	return nil
}

// reapStuckJobs periodically requeues jobs whose worker died mid-flight.
func (p *Pool) reapStuckJobs(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.RecoveryInterval)
	defer ticker.Stop() // tickers leak their runtime timer if you forget this

	for {
		select {
		case <-ctx.Done():
			p.log.Debug("stuck-job reaper stopping")
			return
		case <-ticker.C:
			res, err := p.queue.RecoverStuck(ctx, p.cfg.StuckJobTimeout)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				p.log.Error("stuck-job recovery failed", slog.Any("error", err))
				continue
			}
			if res.Requeued > 0 || res.Failed > 0 {
				p.log.Warn("recovered stuck jobs",
					slog.Int("requeued", res.Requeued),
					slog.Int("failed", res.Failed),
					slog.Duration("stuck_timeout", p.cfg.StuckJobTimeout))
			}
		}
	}
}

func (p *Pool) releaseUndelivered(shutdownCtx context.Context, jobs []queue.Job) {
	if len(jobs) == 0 {
		return
	}

	ids := make([]uuid.UUID, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(shutdownCtx), dbOpTimeout)
	defer cancel()

	if err := p.queue.Release(ctx, ids); err != nil {
		// Not fatal: the reaper will pick these up after StuckJobTimeout.
		p.log.Error("release undelivered jobs failed", slog.Any("error", err), slog.Int("count", len(ids)))
		return
	}
	p.log.Info("released undelivered jobs back to queue", slog.Int("count", len(ids)))
}

func (p *Pool) logClaimLoss(log *slog.Logger, op string, err error) {
	if errors.Is(err, queue.ErrNotFound) {
		// Someone else owns this job now — almost always the reaper after a long
		// stall. Dropping it is correct; the new owner will finish it.
		log.Warn("lost job claim before "+op, slog.Any("error", err))
		return
	}
	log.Error("failed to record job result", slog.String("op", op), slog.Any("error", err))
}

// sleep waits for d, returning false if the context was cancelled first.
// This is the cancellable replacement for time.Sleep — never use time.Sleep in a
// loop you need to be able to shut down.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
