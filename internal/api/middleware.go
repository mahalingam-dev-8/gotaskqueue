package api

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	headerRequestID  = "X-Request-ID"
	contextRequestID = "request_id"
)

// requestID reuses an inbound X-Request-ID (the ALB / upstream may set one) or
// generates a fresh ULID-ish UUID, and echoes it back on the response.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(headerRequestID)
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(contextRequestID, id)
		c.Header(headerRequestID, id)
		c.Next()
	}
}

// requestLogger emits one structured access log line per request.
// Health probes are logged at Debug so ALB checks every 15s don't drown the logs.
func requestLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next() // run the rest of the chain, then log what happened

		attrs := []any{
			slog.String("method", c.Request.Method),
			slog.String("path", path),
			slog.Int("status", c.Writer.Status()),
			slog.Duration("duration", time.Since(start)),
			slog.String("request_id", c.GetString(contextRequestID)),
			slog.String("client_ip", c.ClientIP()),
		}
		if err := c.Errors.ByType(gin.ErrorTypePrivate).String(); err != "" {
			attrs = append(attrs, slog.String("error", err))
		}

		switch {
		case isProbe(path):
			log.Debug("request", attrs...)
		case c.Writer.Status() >= http.StatusInternalServerError:
			log.Error("request", attrs...)
		case c.Writer.Status() >= http.StatusBadRequest:
			log.Warn("request", attrs...)
		default:
			log.Info("request", attrs...)
		}
	}
}

// recovery turns a panic in a handler into a 500 instead of a dead process.
// Gin ships gin.Recovery(); this one logs structurally and hides the stack from
// the client.
func recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic recovered",
					slog.Any("panic", r),
					slog.String("path", c.Request.URL.Path),
					slog.String("request_id", c.GetString(contextRequestID)),
					slog.String("stack", string(debug.Stack())))

				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"error":      "internal_error",
					"message":    "something went wrong",
					"request_id": c.GetString(contextRequestID),
				})
			}
		}()
		c.Next()
	}
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}
