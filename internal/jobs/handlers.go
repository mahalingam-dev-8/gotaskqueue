// Package jobs holds the concrete job handlers. Keeping them out of internal/worker
// means the pool never imports business logic — the dependency arrow points one way.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/mahalingam-dev-8/gotaskqueue/internal/queue"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/worker"
)

// Job type names. Keeping them as constants means the API and the worker cannot
// drift apart over a typo.
const (
	TypeSendEmail      = "email:send"
	TypeGenerateReport = "report:generate"
	TypeDebugSleep     = "debug:sleep"
	TypeDebugFlaky     = "debug:flaky"
)

// Register wires every handler into the registry. cmd/worker calls this and nothing
// else, so adding a job type is a one-line change here.
func Register(r *worker.Registry, log *slog.Logger) {
	r.Register(TypeSendEmail, &SendEmail{Log: log})
	r.Register(TypeGenerateReport, &GenerateReport{Log: log})
	r.RegisterFunc(TypeDebugSleep, debugSleep)
	r.RegisterFunc(TypeDebugFlaky, debugFlaky)
}

// SendEmailPayload is the JSONB body for email:send.
type SendEmailPayload struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// SendEmail is a struct-based handler: this is where you hang real dependencies
// (an SES client, a template renderer) instead of reaching for globals.
type SendEmail struct {
	Log *slog.Logger
}

func (h *SendEmail) Handle(ctx context.Context, job queue.Job) error {
	var p SendEmailPayload
	if err := job.DecodePayload(&p); err != nil {
		// Malformed JSON will be just as malformed on attempt 5.
		return worker.Permanent(fmt.Errorf("decode payload: %w", err))
	}
	if p.To == "" {
		return worker.Permanent(fmt.Errorf("payload field %q is required", "to"))
	}

	// Pretend to call SES. Note ctx is honoured: that is what makes the per-job
	// timeout real rather than decorative.
	select {
	case <-time.After(250 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}

	h.Log.Info("email sent", slog.String("to", p.To), slog.String("subject", p.Subject))
	return nil
}

// GenerateReportPayload is the JSONB body for report:generate.
type GenerateReportPayload struct {
	ReportID string `json:"report_id"`
	Format   string `json:"format"`
}

type GenerateReport struct {
	Log *slog.Logger
}

func (h *GenerateReport) Handle(ctx context.Context, job queue.Job) error {
	var p GenerateReportPayload
	if err := job.DecodePayload(&p); err != nil {
		return worker.Permanent(fmt.Errorf("decode payload: %w", err))
	}
	if p.ReportID == "" {
		return worker.Permanent(fmt.Errorf("payload field %q is required", "report_id"))
	}

	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}

	h.Log.Info("report generated",
		slog.String("report_id", p.ReportID),
		slog.String("format", cmpOr(p.Format, "pdf")))
	return nil
}

// debugSleep sleeps for payload.seconds — useful for watching graceful shutdown and
// the per-job timeout actually work.
func debugSleep(ctx context.Context, job queue.Job) error {
	var p struct {
		Seconds int `json:"seconds"`
	}
	if err := job.DecodePayload(&p); err != nil {
		return worker.Permanent(err)
	}
	if p.Seconds <= 0 {
		p.Seconds = 2
	}

	select {
	case <-time.After(time.Duration(p.Seconds) * time.Second):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// debugFlaky fails with the given probability — useful for watching retries and
// exponential backoff in the logs.
func debugFlaky(_ context.Context, job queue.Job) error {
	var p struct {
		FailureRate float64 `json:"failure_rate"`
	}
	if err := job.DecodePayload(&p); err != nil {
		return worker.Permanent(err)
	}
	if p.FailureRate == 0 {
		p.FailureRate = 0.7
	}

	if rand.Float64() < p.FailureRate {
		return fmt.Errorf("flaky job failed on attempt %d", job.Attempts)
	}
	return nil
}

func cmpOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
