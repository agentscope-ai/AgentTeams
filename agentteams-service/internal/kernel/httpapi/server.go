// Package httpapi serves the kernel health face:
//
//   - GET /healthz → 200 {"status":"ok"} while the process is alive
//   - GET /readyz  → aggregates the ready checks (DB + modules); any
//     down check → 503 {"status":"degraded","checks":{...}}, degraded-only
//     → 200 with the same body shape
//
// plus the kernel middleware: X-Request-Id passthrough-or-generate,
// panic recovery (500 + log), and access logging (method/path/status/
// duration). Business routes (2b/2c) reuse this handler.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/module"
)

// Server is the kernel HTTP face.
type Server struct {
	log    *slog.Logger
	mux    *http.ServeMux
	checks []namedCheck
}

type namedCheck struct {
	name string
	fn   func(ctx context.Context) module.Health
}

// New builds the health face. lg nil → slog.Default().
func New(lg *slog.Logger) *Server {
	if lg == nil {
		lg = slog.Default()
	}
	s := &Server{log: lg, mux: http.NewServeMux()}
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)
	return s
}

// AddReadyCheck registers one readiness component (e.g. "db", "modules").
// Checks run in registration order on every /readyz request.
func (s *Server) AddReadyCheck(name string, fn func(ctx context.Context) module.Health) {
	s.checks = append(s.checks, namedCheck{name: name, fn: fn})
}

// maxRequestBodyBytes bounds every request body before a handler decodes
// it (a worker create carries soul/agents text; 1 MiB is far above the
// observed payloads). Without it a single client could make the process
// buffer an arbitrarily large JSON document.
const maxRequestBodyBytes = 1 << 20

// Handler returns the middleware-wrapped face (request ID → panic
// recovery → access log → body limit → routes).
func (s *Server) Handler() http.Handler {
	return s.requestID(s.recoverPanic(s.accessLog(s.limitBody(s.mux))))
}

// HealthHandler returns the middleware-wrapped face with only /healthz
// and /readyz mounted (same checks as Handler). The UDS socket must stay
// health-only (uds package doc) even though the TCP face carries the full
// route tree, so the socket mounts this entry, not Handler.
func (s *Server) HealthHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", s.handleHealthz)
	m.HandleFunc("/readyz", s.handleReadyz)
	return s.requestID(s.recoverPanic(s.accessLog(s.limitBody(m))))
}

// limitBody wraps the body in http.MaxBytesReader. Handlers surface the
// read error as their normal 400 invalid_request; the connection is closed
// by the server after the response (MaxBytesReader's contract).
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// --- routes ---

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httputil.WriteError(w, http.StatusMethodNotAllowed, httputil.CodeInvalidRequest, "GET only")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": string(module.StatusOK)})
}

// checkOut is one entry of the /readyz "checks" object.
type checkOut struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// readyOut is the non-ok /readyz body (status "degraded",
// per-check detail, 503 only when some check is down).
type readyOut struct {
	Status string              `json:"status"`
	Checks map[string]checkOut `json:"checks"`
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httputil.WriteError(w, http.StatusMethodNotAllowed, httputil.CodeInvalidRequest, "GET only")
		return
	}
	ctx := r.Context()
	checks := make(map[string]checkOut, len(s.checks))
	worst := module.StatusOK
	for _, c := range s.checks {
		h := c.fn(ctx)
		co := checkOut{Status: string(h.Status)}
		if h.Detail != "" {
			co.Detail = h.Detail
		}
		checks[c.name] = co
		if h.Status.WorseThan(worst) {
			worst = h.Status
		}
	}
	if worst == module.StatusOK {
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": string(module.StatusOK)})
		return
	}
	status := http.StatusOK
	if worst == module.StatusDown {
		status = http.StatusServiceUnavailable
	}
	httputil.WriteJSON(w, status, readyOut{Status: string(module.StatusDegraded), Checks: checks})
}

// --- middleware ---

type requestIDKey struct{}

// RequestIDFromContext returns the request ID set by the middleware
// ("" when the middleware did not run).
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered",
					"err", rec,
					"request_id", RequestIDFromContext(r.Context()),
					"path", r.URL.Path,
					"stack", string(debug.Stack()))
				httputil.WriteError(w, http.StatusInternalServerError, httputil.CodeInternal, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	if sr.status == 0 {
		sr.status = code
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	return sr.ResponseWriter.Write(b)
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", RequestIDFromContext(r.Context()))
	})
}
