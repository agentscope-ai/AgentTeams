package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
)

func withControllerHTTPMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		metrics.ObserveControllerHTTP(r.Method, routePattern(r), start, rec.statusCode)
	})
}

func routePattern(r *http.Request) string {
	pattern := r.Pattern
	if pattern == "" {
		return "unmatched"
	}
	prefix := r.Method + " "
	if strings.HasPrefix(pattern, prefix) {
		return strings.TrimPrefix(pattern, prefix)
	}
	return pattern
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
