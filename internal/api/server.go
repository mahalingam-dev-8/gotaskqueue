// Package api exposes the producer-side REST interface over the queue.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ekkomd/gotaskqueue/internal/auth"
	"github.com/ekkomd/gotaskqueue/internal/config"
	"github.com/ekkomd/gotaskqueue/internal/queue"
)

// Server owns the HTTP server and its dependencies.
//
// Everything it needs is passed into New — no package-level singletons. This is the
// Go substitute for NestJS's DI container: explicit constructor wiring in main().
type Server struct {
	cfg   *config.APIConfig
	queue *queue.Queue
	pool  *pgxpool.Pool
	auth  *auth.Manager
	log   *slog.Logger
	http  *http.Server
}

func New(cfg *config.APIConfig, q *queue.Queue, pool *pgxpool.Pool, authMgr *auth.Manager, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, queue: q, pool: pool, auth: authMgr, log: log}

	s.http = &http.Server{
		Addr:    cfg.HTTP.Addr(),
		Handler: s.routes(),
		// Always set these. The zero value means "no timeout", which is how a slow
		// client (or a stalled ALB connection) exhausts your file descriptors.
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}
	return s
}

func (s *Server) routes() *gin.Engine {
	if s.cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	// gin.New() (not gin.Default()) so we install our own slog-based logger and
	// recovery instead of Gin's text ones.
	r := gin.New()

	// The ALB terminates the client connection, so the real client IP arrives in
	// X-Forwarded-For. Trusting it unconditionally is a spoofing vector; in ECS the
	// only hop in front of you is the ALB inside your VPC.
	r.SetTrustedProxies(nil)
	r.ForwardedByClientIP = true

	r.Use(requestID(), requestLogger(s.log), recovery(s.log))

	// Probes are unauthenticated: the ALB health check cannot present a JWT.
	r.GET("/healthz", s.healthz)
	r.GET("/readyz", s.readyz)

	v1 := r.Group("/api/v1")
	{
		jobs := v1.Group("/jobs", auth.Middleware(s.auth))
		{
			jobs.POST("", auth.RequireScope(auth.ScopeJobsWrite), s.createJob)
			jobs.GET("/:id", auth.RequireScope(auth.ScopeJobsRead), s.getJob)
		}
	}

	// Convenience aliases so the paths in the brief work verbatim.
	authed := r.Group("/jobs", auth.Middleware(s.auth))
	{
		authed.POST("", auth.RequireScope(auth.ScopeJobsWrite), s.createJob)
		authed.GET("/:id", auth.RequireScope(auth.ScopeJobsRead), s.getJob)
	}

	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "no such route"})
	})
	return r
}

// Start blocks serving requests. http.ErrServerClosed is the expected return after
// a graceful Shutdown, so callers treat it as success.
func (s *Server) Start() error {
	s.log.Info("http server listening", slog.String("addr", s.cfg.HTTP.Addr()))
	return s.http.ListenAndServe()
}

// Shutdown stops accepting new connections and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
