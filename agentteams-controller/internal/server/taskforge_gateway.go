package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/taskforge"
)

const (
	taskForgeHealthTTL = 5 * time.Second

	// maxTaskForgeResponse bounds the buffered upstream body (task 26 §5.1);
	// larger answers are refused with 502 taskforge_response_too_large.
	maxTaskForgeResponse = 8 << 20 // 8 MiB
)

// taskForgeResourceIDRe is the fixed whitelist for {id}/{name} path values
// (task 26 §5.2): they are spliced into upstream paths, so anything outside
// the charset is refused before any upstream contact.
var taskForgeResourceIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// TaskForgeGateway is the fixed-path front door to the TaskForge runner
// (1.2.7, task 26). It forwards a FIXED set of routes (never a generic
// reverse proxy — repository discipline), signs every request with the
// shared claims secret, and degrades to 503 whenever the runner is
// unreachable ("宁可 503，不假装成功", task 26 §5.3).
type TaskForgeGateway struct {
	client   *taskforge.Client
	upstream string       // the configured AGENTTEAMS_TASKFORGE_URL (status/debug only)
	health   atomic.Value // cachedHealth
	mu       sync.Mutex   // single-flight for the health probe
	now      func() time.Time
}

type cachedHealth struct {
	status  taskforge.HealthStatus
	checked time.Time
}

// NewTaskForgeGateway builds the gateway from loaded config, or returns nil
// when AGENTTEAMS_TASKFORGE_URL is empty (gateway off — no taskforge routes
// are registered and the controller behaves exactly as before, task 26 §5.1).
func NewTaskForgeGateway(cfg *config.Config) *TaskForgeGateway {
	if cfg == nil || cfg.TaskForgeURL == "" {
		return nil
	}
	base, err := url.Parse(cfg.TaskForgeURL)
	if err != nil {
		return nil // LoadConfig panics on an invalid URL; defensive only
	}
	return newTaskForgeGateway(taskforge.NewClient(base, cfg.TaskForgeClaimsSecret, cfg.TaskForgeTimeout), cfg.TaskForgeURL)
}

// newTaskForgeGateway is the internal constructor used by the http.go
// wiring (task 26 §6.3: ServerDeps carries the client, not the config).
func newTaskForgeGateway(client *taskforge.Client, upstream string) *TaskForgeGateway {
	return &TaskForgeGateway{client: client, upstream: upstream, now: time.Now}
}

// currentHealth returns the cached runner health, probing (single-flight)
// when the cache is older than taskForgeHealthTTL. A failed probe reports
// unhealthy — fail closed, task 26 §5.3.
func (g *TaskForgeGateway) currentHealth(ctx context.Context) cachedHealth {
	if v, ok := g.health.Load().(cachedHealth); ok && g.now().Sub(v.checked) < taskForgeHealthTTL {
		return v
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok := g.health.Load().(cachedHealth); ok && g.now().Sub(v.checked) < taskForgeHealthTTL {
		return v
	}
	st, err := g.client.Health(ctx, g.systemClaims())
	ch := cachedHealth{checked: g.now()}
	if err != nil {
		ch.status = taskforge.HealthStatus{Healthy: false}
	} else {
		ch.status = st
	}
	g.health.Store(ch)
	return ch
}

// markUnhealthy poisons the cache after a failed forward so the next
// request takes the 503 path instead of paying another round trip.
func (g *TaskForgeGateway) markUnhealthy() {
	g.health.Store(cachedHealth{status: taskforge.HealthStatus{Healthy: false}, checked: g.now()})
}

// systemClaims signs runner probes with the "system" actor: the identity
// "system" is reserved for controller-internal calls (health probe),
// distinct from real operators and workers (task 26 design decision).
func (g *TaskForgeGateway) systemClaims() taskforge.Claims {
	return taskforge.Claims{ActorType: taskforge.ActorSystem, IssuedAt: g.now().Unix()}
}

// callerClaims maps the authenticated caller to signed claims.
// OnBehalfOf and TokenJTI stay empty in v1 (no delegation; SA tokens carry
// no JTI) — the canonical string keeps their placeholders (task 26 §3.1).
// TeamID is the caller's team; L2 humans carry no single team (their
// accessibleTeams is a list) and the controller has no task -> team
// resolution, so their TeamID stays empty and TaskForge resolves the scope
// from its own data.
func (g *TaskForgeGateway) callerClaims(caller *authpkg.CallerIdentity, requestID, traceID string) taskforge.Claims {
	return taskforge.Claims{
		ActorType: actorTypeForRole(caller.Role),
		ActorID:   caller.Username,
		TeamID:    caller.Team,
		RequestID: requestID,
		TraceID:   traceID,
		IssuedAt:  g.now().Unix(),
	}
}

// actorTypeForRole maps controller roles to claim actor types. Admin is a
// human operator (L1), so it signs as "human"; "system" is reserved for
// controller-internal calls like the health probe (task 26 design
// decision).
func actorTypeForRole(role string) taskforge.ActorType {
	switch role {
	case authpkg.RoleWorker:
		return taskforge.ActorWorker
	case authpkg.RoleManager:
		return taskforge.ActorManager
	case authpkg.RoleTeamLeader:
		return taskforge.ActorLeader
	default:
		return taskforge.ActorHuman
	}
}

// Forward returns a handler that forwards one fixed upstream route:
// internalPath is a fixed /internal/v1/... path with at most one %s
// segment, filled from the validated {id}/{name} path value. The route is
// registered behind mw.RequireAuthz (task 26 §5.2); the authorization
// matrix is pinned by the authorizer tests, not here.
func (g *TaskForgeGateway) Forward(internalPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// {id}/{name} are spliced into the upstream path, so they must pass
		// the whitelist BEFORE any upstream contact (task 26 §5.2).
		id := r.PathValue("id")
		name := r.PathValue("name")
		if (id != "" && !taskForgeResourceIDRe.MatchString(id)) || (name != "" && !taskForgeResourceIDRe.MatchString(name)) {
			writeTaskForgeError(w, http.StatusBadRequest, "invalid_resource_id", "invalid resource id")
			return
		}
		seg := id
		if seg == "" {
			seg = name
		}
		target := internalPath
		if strings.Count(internalPath, "%s") == 1 && seg != "" {
			target = fmt.Sprintf(internalPath, seg)
		}

		caller := authpkg.CallerFromContext(r.Context())
		if caller == nil {
			// Routes are registered behind RequireAuthz in production, so a
			// caller is always present; a request without one must fail
			// closed rather than forward with an empty actor.
			httputil.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		// Health gate: "宁可 503，不假装成功" (task 26 §5.3).
		if h := g.currentHealth(r.Context()); !h.status.Healthy {
			w.Header().Set("Retry-After", "5")
			writeTaskForgeError(w, http.StatusServiceUnavailable, "taskforge_unavailable", "task system unavailable")
			return
		}

		// Request/trace ids: caller-supplied values win, otherwise the
		// gateway mints one so the signed claim is always complete.
		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = newRequestID()
		}
		traceID := r.Header.Get("X-Trace-Id")
		if traceID == "" {
			traceID = requestID
		}

		ctx := r.Context()
		if idemKey := r.Header.Get("Idempotency-Key"); idemKey != "" {
			// If-Match is forwarded whenever Idempotency-Key is present; the
			// client layer sets only non-empty values (task 26 §3.2).
			ctx = taskforge.WithForwardedHeaders(ctx, idemKey, r.Header.Get("If-Match"))
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		q := url.Values{}
		// Query whitelist (task 26 §5.2): anything outside these five names
		// is dropped — never an arbitrary query pass-through.
		for _, k := range []string{"limit", "before", "after", "status", "include"} {
			if v := r.URL.Query().Get(k); v != "" {
				q.Set(k, v)
			}
		}

		resp, err := g.client.Do(ctx, g.callerClaims(caller, requestID, traceID), r.Method, target, q, body)
		if err != nil {
			if errors.Is(err, taskforge.ErrUnavailable) {
				g.markUnhealthy()
				w.Header().Set("Retry-After", "5")
				writeTaskForgeError(w, http.StatusServiceUnavailable, "taskforge_unavailable", "task system unavailable")
				return
			}
			writeTaskForgeError(w, http.StatusBadGateway, "taskforge_bad_gateway", "task system request failed")
			return
		}
		defer resp.Body.Close()

		// Bound the buffered body (task 26 §5.1): read at most 8 MiB + 1 so
		// an over-limit answer is detected BEFORE any byte is written.
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxTaskForgeResponse+1))
		if err != nil {
			writeTaskForgeError(w, http.StatusBadGateway, "taskforge_bad_gateway", "task system response failed")
			return
		}
		if len(data) > maxTaskForgeResponse {
			writeTaskForgeError(w, http.StatusBadGateway, "taskforge_response_too_large", "task system response too large")
			return
		}

		if resp.StatusCode >= 400 {
			// Upstream error: pass through unchanged when the body is JSON
			// carrying error.code (its contract shape); anything else is a
			// 502 with a generic message — never a raw upstream body that
			// could leak deployment details (task 26 §5.3).
			if taskForgeErrorCode(data) != "" {
				copyTaskForgeHeaders(w.Header(), resp.Header)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(data)
				return
			}
			writeTaskForgeError(w, http.StatusBadGateway, "taskforge_bad_gateway", "task system request failed")
			return
		}

		copyTaskForgeHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
	}
}

// StatusHandler serves GET /api/v1/runner-modules/taskforge/status locally
// (task 26 §5.2: this route does NOT forward). It reports the runner module
// state from the health cache, probing when stale.
func (g *TaskForgeGateway) StatusHandler(w http.ResponseWriter, r *http.Request) {
	h := g.currentHealth(r.Context())
	checkedAt := ""
	if !h.checked.IsZero() {
		checkedAt = h.checked.UTC().Format(time.RFC3339)
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"enabled":    true,
		"healthy":    h.status.Healthy,
		"version":    h.status.Version,
		"protocol":   h.status.Protocol,
		"checked_at": checkedAt,
	})
}

// newRequestID mints a gateway request id (16 hex chars) so every signed
// claim carries a non-empty X-Request-Id even when the caller did not.
func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// copyTaskForgeHeaders copies the whitelist of hop-by-hop-safe response
// headers; everything else (Connection, Keep-Alive, Transfer-Encoding,
// Upgrade, Proxy-*) is stripped hop-by-hop (task 26 §5.1).
func copyTaskForgeHeaders(dst, src http.Header) {
	for _, k := range []string{"ETag", "Retry-After"} {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

// taskForgeErrorCode extracts error.code from a TaskForge error envelope
// ({"error":{"code":...}}); "" when the body is not that shape.
func taskForgeErrorCode(body []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" {
		return ""
	}
	return env.Error.Code
}

// writeTaskForgeError writes the gateway's OWN error envelope
// ({"error":{"code":...,"message":...}}, the doc 21 model). It is
// deliberately NOT the httputil.WriteError {"message":...} shape: a client
// must be able to tell a TaskForge degradation apart from a plain
// controller error.
func writeTaskForgeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
