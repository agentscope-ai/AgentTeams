package resources

// The controller -> service observed-state writeback (58 §5).
//
// POST /internal/v1/workers/status is the batch route the controller uses
// to report execution state (phase, container state, Matrix identity, room,
// heartbeat and task counters). The handler maps every entry to one
// Store.UpdateStatus call, which writes observed_* columns only: generation,
// spec_hash, version and the outbox are untouched, and a callback whose
// generation is older than the stored one is discarded by the store's
// generation predicate (58 §4.1 / §5 out-of-order protection).
//
// The route carries no authentication of its own: it is served by the
// internal face, where httpapi.InternalAuth guards the whole /internal/v1
// prefix (58 §5: never exposed publicly).

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
)

// StatusHandler serves the controller status writeback.
type StatusHandler struct {
	Store Store
	// Log is the handler logger; nil -> slog.Default().
	Log *slog.Logger
	// Now is the handler clock; nil -> time.Now. The Store stamps the row
	// timestamps itself, so this exists for symmetry with the other handlers
	// and for tests.
	Now func() time.Time
}

// Register mounts the writeback route on mux.
func (h *StatusHandler) Register(m *http.ServeMux) {
	m.HandleFunc("POST /internal/v1/workers/status", h.receive)
}

// statusReport is the batch request body (58 §5):
//
//	{"workers":[{"name":"w1","generation":3,"phase":"Running"}]}
type statusReport struct {
	Workers []statusWorker `json:"workers"`
}

// statusWorker is one entry of the batch. The JSON tags mirror the
// controller's execution report (adapter.Execution / WorkerResponse names);
// encoding/json matches them case-insensitively, so a matrixUserId spelling
// is accepted too.
type statusWorker struct {
	Name             string `json:"name"`
	Generation       int64  `json:"generation"`
	Phase            string `json:"phase"`
	ContainerState   string `json:"containerState,omitempty"`
	MatrixUserID     string `json:"matrixUserID,omitempty"`
	RoomID           string `json:"roomID,omitempty"`
	Message          string `json:"message,omitempty"`
	LastActiveAt     string `json:"lastActiveAt,omitempty"`
	AgentStatus      string `json:"agentStatus,omitempty"`
	RunningTaskCount *int   `json:"runningTaskCount,omitempty"`
	LastRunAt        string `json:"lastRunAt,omitempty"`
	LastFinishAt     string `json:"lastFinishAt,omitempty"`
}

// statusResult is the 200 body: how many rows moved to the reported state
// and how many stale generations were discarded.
type statusResult struct {
	Updated   int `json:"updated"`
	Discarded int `json:"discarded"`
}

// receive applies one batch. Validation happens before any write, so a
// malformed batch never leaves a half-applied set of rows; a store failure
// aborts the batch with the frozen 500 envelope (the controller resends the
// whole report — per-row writes are idempotent).
func (h *StatusHandler) receive(w http.ResponseWriter, r *http.Request) {
	var report statusReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, httputil.CodeInvalidRequest, "invalid JSON: "+err.Error())
		return
	}
	updates, err := parseStatusReport(report)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, httputil.CodeInvalidRequest, err.Error())
		return
	}

	var out statusResult
	for _, u := range updates {
		updated, err := h.Store.UpdateStatus(r.Context(), u.name, u.observed, u.generation)
		if err != nil {
			h.logger().Error("status writeback failed",
				"worker", u.name, "generation", u.generation, "err", err,
				"request_id", httputil.RequestID(r))
			httputil.WriteError(w, http.StatusInternalServerError, httputil.CodeInternal, "status writeback: internal error")
			return
		}
		if updated {
			out.Updated++
		} else {
			out.Discarded++
		}
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}

// statusUpdate is one validated report entry.
type statusUpdate struct {
	name       string
	generation int64
	observed   Observed
}

// parseStatusReport validates and converts the batch. Times are RFC3339
// (fractional seconds accepted); an empty string means "not reported" and
// maps to nil, which the store writes as NULL.
func parseStatusReport(report statusReport) ([]statusUpdate, error) {
	out := make([]statusUpdate, 0, len(report.Workers))
	for i, in := range report.Workers {
		if in.Name == "" {
			return nil, fmt.Errorf("workers[%d]: name is required", i)
		}
		lastActive, err := parseStatusTime(in.LastActiveAt)
		if err != nil {
			return nil, fmt.Errorf("workers[%d] %q: lastActiveAt: %w", i, in.Name, err)
		}
		lastRun, err := parseStatusTime(in.LastRunAt)
		if err != nil {
			return nil, fmt.Errorf("workers[%d] %q: lastRunAt: %w", i, in.Name, err)
		}
		lastFinish, err := parseStatusTime(in.LastFinishAt)
		if err != nil {
			return nil, fmt.Errorf("workers[%d] %q: lastFinishAt: %w", i, in.Name, err)
		}
		out = append(out, statusUpdate{
			name:       in.Name,
			generation: in.Generation,
			observed: Observed{
				Phase:            in.Phase,
				ContainerState:   in.ContainerState,
				MatrixUserID:     in.MatrixUserID,
				RoomID:           in.RoomID,
				Message:          in.Message,
				LastActiveAt:     lastActive,
				AgentStatus:      in.AgentStatus,
				RunningTaskCount: in.RunningTaskCount,
				LastRunAt:        lastRun,
				LastFinishAt:     lastFinish,
			},
		})
	}
	return out, nil
}

// parseStatusTime parses one RFC3339 timestamp; "" is "not reported" (nil).
func parseStatusTime(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("invalid RFC3339 timestamp %q: %w", value, err)
	}
	t = t.UTC()
	return &t, nil
}

// logger returns the configured logger or the process default.
func (h *StatusHandler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}
