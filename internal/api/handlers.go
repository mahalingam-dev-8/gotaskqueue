package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mahalingam-dev-8/gotaskqueue/internal/auth"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/queue"
)

// createJobRequest is the POST /jobs body.
//
// The `binding` tags are go-playground/validator rules that Gin applies in
// ShouldBindJSON — the rough equivalent of class-validator decorators in NestJS,
// minus the decorators.
type createJobRequest struct {
	Type         string          `json:"type" binding:"required,max=100"`
	Payload      json.RawMessage `json:"payload"`
	MaxAttempts  int             `json:"max_attempts" binding:"omitempty,min=1,max=50"`
	DelaySeconds int             `json:"delay_seconds" binding:"omitempty,min=0,max=604800"`
}

// jobResponse is the wire shape of a job. A dedicated struct (rather than returning
// queue.Job) keeps internal columns like locked_by out of the public contract.
type jobResponse struct {
	ID          uuid.UUID       `json:"id"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	Status      queue.Status    `json:"status"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	LastError   *string         `json:"last_error,omitempty"`
	RunAt       time.Time       `json:"run_at"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func toJobResponse(j *queue.Job) jobResponse {
	return jobResponse{
		ID:          j.ID,
		Type:        j.Type,
		Payload:     j.Payload,
		Status:      j.Status,
		Attempts:    j.Attempts,
		MaxAttempts: j.MaxAttempts,
		LastError:   j.LastError,
		RunAt:       j.RunAt,
		CreatedAt:   j.CreatedAt,
		UpdatedAt:   j.UpdatedAt,
	}
}

// createJob handles POST /jobs.
func (s *Server) createJob(c *gin.Context) {
	var req createJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body: "+err.Error())
		return
	}

	if len(req.Payload) > 0 && !json.Valid(req.Payload) {
		badRequest(c, "payload must be valid JSON")
		return
	}

	maxAttempts := req.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = s.cfg.DefaultMaxAttempts
	}

	job, err := s.queue.Enqueue(c.Request.Context(), queue.EnqueueParams{
		Type:        req.Type,
		Payload:     req.Payload,
		MaxAttempts: maxAttempts,
		Delay:       time.Duration(req.DelaySeconds) * time.Second,
	})
	if err != nil {
		s.internalError(c, "enqueue job", err)
		return
	}

	claims, _ := auth.ClaimsFrom(c)
	s.log.Info("job enqueued",
		slog.String("job_id", job.ID.String()),
		slog.String("job_type", job.Type),
		slog.String("subject", subjectOf(claims)),
		slog.String("request_id", c.GetString(contextRequestID)))

	// 202, not 201: the job is accepted for later processing, not completed.
	c.JSON(http.StatusAccepted, toJobResponse(job))
}

// getJob handles GET /jobs/:id.
func (s *Server) getJob(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		badRequest(c, "job id must be a UUID")
		return
	}

	job, err := s.queue.Get(c.Request.Context(), id)
	if err != nil {
		// errors.Is walks the wrapping chain, so the fmt.Errorf("%w") in the queue
		// package does not hide the sentinel.
		if errors.Is(err, queue.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "job not found"})
			return
		}
		s.internalError(c, "get job", err)
		return
	}

	c.JSON(http.StatusOK, toJobResponse(job))
}

// healthz is liveness: "this process is running and not deadlocked". It must not
// touch the database — otherwise a DB blip makes ECS kill healthy tasks and you
// turn a degradation into an outage.
func (s *Server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz is readiness: "this process can serve traffic right now", which for the
// producer means Postgres is reachable. This is the one the ALB target group and
// the ECS deployment gate should use.
func (s *Server) readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := s.pool.Ping(ctx); err != nil {
		s.log.Warn("readiness check failed", slog.Any("error", err))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status":   "unavailable",
			"database": "unreachable",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready", "database": "ok"})
}

func badRequest(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": message})
}

// internalError logs the real error and returns a generic one — never leak
// driver/SQL detail to a client.
func (s *Server) internalError(c *gin.Context, op string, err error) {
	s.log.Error("request failed",
		slog.String("op", op),
		slog.Any("error", err),
		slog.String("request_id", c.GetString(contextRequestID)))

	c.JSON(http.StatusInternalServerError, gin.H{
		"error":      "internal_error",
		"message":    "something went wrong",
		"request_id": c.GetString(contextRequestID),
	})
}

func subjectOf(claims *auth.Claims) string {
	if claims == nil {
		return ""
	}
	return claims.Subject
}
