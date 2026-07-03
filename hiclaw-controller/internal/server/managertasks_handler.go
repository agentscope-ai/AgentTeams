package server

import (
	"encoding/json"
	"net/http"
	"os"
)

// ManagerTasksHandler serves the Manager Agent's task-tracking state
// (state.json, maintained by manage-state.sh — see plan §10.2 (2), Option 1
// controller-side) as raw, opaque-ish JSON for the dashboard's task table
// (plan #17). It performs NO re-modeling: the response body is exactly the
// bytes found on disk, so newly added state.json fields (e.g. M1's
// cancelled_tasks[], blocked/blocked_since, last_digest_sent_at) are exposed
// automatically without a handler change.
//
// Availability is embedded-only: the state.json file lives on the local
// filesystem shared between the Manager container and the embedded
// controller (both bind the host's HICLAW_WORKSPACE_DIR — see
// install/hiclaw-install.sh:3153 and :3535 / install/hiclaw-install.ps1:3168).
// In incluster (k8s) mode there is no such shared mount, so the file will
// simply never be found and this endpoint 404s by design — a cross-node
// (Option 2) implementation is future work per §10.2.
//
// Example:
//
//	curl -s https://controller.example.com/api/v1/manager-tasks \
//	  -H "Authorization: Bearer $TOKEN"
type ManagerTasksHandler struct {
	// StateFilePath returns the absolute path to the Manager's state.json.
	// It is resolved lazily (rather than once at construction) so tests and
	// callers can point it at a path that doesn't exist yet at handler
	// creation time.
	StateFilePath func() string
}

// NewManagerTasksHandler constructs a ManagerTasksHandler that reads the
// given state.json path.
func NewManagerTasksHandler(stateFilePath func() string) *ManagerTasksHandler {
	return &ManagerTasksHandler{StateFilePath: stateFilePath}
}

// GetManagerTasks handles GET /api/v1/manager-tasks. It reads the Manager's
// state.json from disk and passes it through unmodified:
//   - file missing            -> 404 {"error":"manager state not available"}
//   - unreadable or not JSON  -> 502 {"error":"manager state unreadable"}
//   - success                 -> 200 application/json, raw file contents
func (h *ManagerTasksHandler) GetManagerTasks(w http.ResponseWriter, r *http.Request) {
	path := h.StateFilePath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeManagerTasksError(w, http.StatusNotFound, "manager state not available")
			return
		}
		writeManagerTasksError(w, http.StatusBadGateway, "manager state unreadable")
		return
	}

	if !json.Valid(data) {
		writeManagerTasksError(w, http.StatusBadGateway, "manager state unreadable")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeManagerTasksError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
