package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// annotationLoopPausedBy records the ACTIVE pause sources as a JSON array.
// Every source that requested the pause must be cleared before the loop
// resumes: user and project pauses are additive, never last-writer-wins
// (issue #1431 gap 3). Legacy single-value annotations ("user"/"project")
// are still accepted when parsed.
const annotationLoopPausedBy = "loops.agentteams.io/paused-by"

// Valid values for the pause body's "source" field.
const (
	pauseSourceUser    = "user"
	pauseSourceProject = "project"
)

// LoopHandler serves the Loop control plane endpoints (INT-7b):
// POST /api/v1/loops/{id}/pause and POST /api/v1/loops/{id}/resume.
// It patches spec only — the status subresource is written exclusively
// by the loop reconciler.
//
// Execution gating (issue #1431): the teamharness dispatch path reads
// `loop.pause_sources` from the project meta. When a pause request carries
// a projectId the handler projects the source into that meta FIRST (the
// execution-side flag), then patches the CRD; `effective` in the response
// reports whether the execution side actually received the pause.
type LoopHandler struct {
	Client    client.Client
	Namespace string
	OSS       oss.StorageClient
}

// NewLoopHandler builds a LoopHandler.
func NewLoopHandler(c client.Client, namespace string, o oss.StorageClient) *LoopHandler {
	return &LoopHandler{Client: c, Namespace: namespace, OSS: o}
}

// PauseLoop handles POST /api/v1/loops/{id}/pause.
//
// Body (optional): {"source": "user" | "project", "projectId": "<id>"}.
// Adds the source to the active pause set, sets spec.paused=true and, when
// projectId is given, projects the source into the project meta so the
// execution side (teamharness) refuses new dispatch/progression.
//
// Permission (INT-7b ruling): any authenticated role except worker —
// admin/manager unrestricted, teamLeader/human restricted to their own
// team (enforced here, see checkLoopAccess).
func (h *LoopHandler) PauseLoop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	loop := &v1beta1.Loop{}
	if err := h.Client.Get(ctx, h.loopKey(id), loop); err != nil {
		httputil.WriteError(w, http.StatusNotFound, fmt.Sprintf("loop %q not found", id))
		return
	}

	var body struct {
		Source    string `json:"source"`
		ProjectID string `json:"projectId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httputil.WriteError(w, http.StatusBadRequest, `invalid request body: expected a JSON object with optional "source"/"projectId" fields`)
		return
	}
	source := strings.TrimSpace(body.Source)
	if source == "" {
		source = pauseSourceUser
	}
	if source != pauseSourceUser && source != pauseSourceProject {
		httputil.WriteError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid pause source %q: must be %q or %q", source, pauseSourceUser, pauseSourceProject))
		return
	}

	if err := h.checkLoopAccess(ctx, loop, false); err != nil {
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	sources := addPauseSource(parsePauseSources(loop.Annotations), source)

	// Ordering (issue #1431): write the execution-side flag FIRST. A
	// successful response with effective=true therefore cannot allow new
	// dispatch — the gate it reports is the one the dispatcher reads.
	effective := false
	if projectID := strings.TrimSpace(body.ProjectID); projectID != "" {
		ok, err := h.setProjectPauseSource(ctx, projectID, loop.Spec.TeamName, source, true)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError,
				fmt.Sprintf("failed to project the pause into project %q: %v", projectID, err))
			return
		}
		effective = ok
	}

	patch := client.MergeFrom(loop.DeepCopy())
	loop.Spec.Paused = true
	if loop.Annotations == nil {
		loop.Annotations = map[string]string{}
	}
	encoded, err := json.Marshal(sources)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to encode pause sources")
		return
	}
	loop.Annotations[annotationLoopPausedBy] = string(encoded)
	if err := h.Client.Patch(ctx, loop, patch); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, fmt.Sprintf("failed to pause loop: %v", err))
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"id":            loop.Name,
		"requested":     true,
		"paused":        true,
		"effective":     effective,
		"pause_sources": sources,
		"project_id":    strings.TrimSpace(body.ProjectID),
	})
}

// ResumeLoop handles POST /api/v1/loops/{id}/resume.
//
// Body (optional): {"source": "user" | "project", "projectId": "<id>"}.
// Clears ONE pause source; the loop stays paused while any other source
// remains (409 with the remaining set). When the last source is cleared the
// CRD and (with a projectId) the project meta are both released, the project
// meta first.
//
// Permission (INT-7b ruling): admin + L2 human only — manager, team
// leader, and worker are denied deliberately (a paused loop is a human
// decision; agents do not release it themselves).
func (h *LoopHandler) ResumeLoop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	loop := &v1beta1.Loop{}
	if err := h.Client.Get(ctx, h.loopKey(id), loop); err != nil {
		httputil.WriteError(w, http.StatusNotFound, fmt.Sprintf("loop %q not found", id))
		return
	}

	if err := h.checkLoopAccess(ctx, loop, true); err != nil {
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	var body struct {
		Source    string `json:"source"`
		ProjectID string `json:"projectId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httputil.WriteError(w, http.StatusBadRequest, `invalid request body: expected a JSON object with optional "source"/"projectId" fields`)
		return
	}

	sources := parsePauseSources(loop.Annotations)
	if !loop.Spec.Paused && len(sources) == 0 {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"id": loop.Name, "requested": true, "paused": false, "resumed": false,
			"effective": true, "pause_sources": []string{},
		})
		return
	}

	target := strings.ToLower(strings.TrimSpace(body.Source))
	if target == "" {
		switch len(sources) {
		case 0:
			// Legacy paused loop with no recorded source: clear it.
		case 1:
			target = sources[0]
		default:
			httputil.WriteJSON(w, http.StatusBadRequest, map[string]any{
				"message":       "multiple pause sources are active; specify which source to resume",
				"pause_sources": sources,
			})
			return
		}
	}
	if target != "" && !containsSource(sources, target) {
		// Idempotent: the source is already cleared; report the remaining set.
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"id": loop.Name, "requested": true, "paused": loop.Spec.Paused,
			"resumed": false, "effective": true, "pause_sources": sources,
		})
		return
	}

	remaining := removePauseSource(sources, target)
	if len(remaining) > 0 {
		// Another reason is still active: clearing the CRD here would
		// silently unpause work that a different source paused (#1431).
		httputil.WriteJSON(w, http.StatusConflict, map[string]any{
			"message":           fmt.Sprintf("loop is still paused by %v; clear every source before resuming", remaining),
			"remaining_sources": remaining,
			"pause_sources":     sources,
		})
		return
	}

	// Release the execution side first, then the CRD (mirror of pause).
	effective := false
	if projectID := strings.TrimSpace(body.ProjectID); projectID != "" {
		ok, err := h.setProjectPauseSource(ctx, projectID, loop.Spec.TeamName, target, false)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError,
				fmt.Sprintf("failed to release the pause in project %q: %v", projectID, err))
			return
		}
		effective = ok
	}

	patch := client.MergeFrom(loop.DeepCopy())
	loop.Spec.Paused = false
	if loop.Annotations != nil {
		delete(loop.Annotations, annotationLoopPausedBy)
	}
	if err := h.Client.Patch(ctx, loop, patch); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, fmt.Sprintf("failed to resume loop: %v", err))
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"id": loop.Name, "requested": true, "paused": false, "resumed": true,
		"effective": effective, "pause_sources": []string{},
		"project_id": strings.TrimSpace(body.ProjectID),
	})
}

func (h *LoopHandler) loopKey(id string) types.NamespacedName {
	return types.NamespacedName{Namespace: h.Namespace, Name: id}
}

// ---- pause-source set helpers ------------------------------------------

// parsePauseSources decodes the paused-by annotation. Current format is a
// JSON array; the legacy single value ("user"/"project") stays readable.
func parsePauseSources(annotations map[string]string) []string {
	if annotations == nil {
		return nil
	}
	raw := strings.TrimSpace(annotations[annotationLoopPausedBy])
	if raw == "" {
		return nil
	}
	var values []string
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			values = nil
		}
	} else {
		values = strings.Split(raw, ",")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		item := strings.ToLower(strings.TrimSpace(value))
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func containsSource(sources []string, source string) bool {
	for _, item := range sources {
		if item == source {
			return true
		}
	}
	return false
}

func addPauseSource(sources []string, source string) []string {
	if containsSource(sources, source) {
		return sources
	}
	out := append(append([]string{}, sources...), source)
	sort.Strings(out)
	return out
}

func removePauseSource(sources []string, source string) []string {
	out := make([]string, 0, len(sources))
	for _, item := range sources {
		if item != source {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}

// ---- execution-side projection -----------------------------------------

// projectMetaKeys returns the candidate object keys for a project meta,
// team-scoped first (the writer uses the owning team's scope).
func projectMetaKeys(projectID, teamName string) []string {
	keys := make([]string, 0, 2)
	if teamName != "" {
		keys = append(keys, fmt.Sprintf("teams/%s/shared/projects/%s/meta.json", teamName, projectID))
	}
	return append(keys, fmt.Sprintf("shared/projects/%s/meta.json", projectID))
}

// setProjectPauseSource adds or removes one source in the project's
// `loop.pause_sources`. Returns false when the project has no loop (nothing
// to gate) or no meta could be read; the caller reports effective=false and
// must not claim execution is paused.
func (h *LoopHandler) setProjectPauseSource(
	ctx context.Context,
	projectID, teamName, source string,
	paused bool,
) (bool, error) {
	if h.OSS == nil || projectID == "" {
		return false, nil
	}
	var (
		key  string
		data []byte
	)
	found := false
	for _, candidate := range projectMetaKeys(projectID, teamName) {
		raw, err := h.OSS.GetObject(ctx, candidate)
		if err == nil {
			key, data, found = candidate, raw, true
			break
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return false, err
	}
	if !found {
		return false, nil
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return false, err
	}
	loop, ok := meta["loop"].(map[string]any)
	if !ok || loop == nil {
		return false, nil // no loop record: nothing on the execution side to gate
	}
	sources := []string{}
	if raw, ok := loop["pause_sources"].([]any); ok {
		for _, item := range raw {
			if value, ok := item.(string); ok && strings.TrimSpace(value) != "" {
				sources = append(sources, strings.ToLower(strings.TrimSpace(value)))
			}
		}
	}
	if paused {
		sources = addPauseSource(sources, source)
	} else {
		sources = removePauseSource(sources, source)
	}
	loop["pause_sources"] = sources
	// Keep the legacy project flag consistent for older readers.
	if source == pauseSourceProject {
		loop["paused_by_project"] = paused
	}
	meta["loop"] = loop
	updated, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return false, err
	}
	if err := h.OSS.PutObject(ctx, key, append(updated, '\n')); err != nil {
		return false, err
	}
	return true, nil
}

// checkLoopAccess re-checks the caller against the loop. It is defense
// in depth on top of the RequireAuthz middleware, and the REAL
// team-scope boundary: the middleware's resolveResourceTeam only
// resolves workers, so a loop's ResourceTeam arrives empty and
// requireSameTeam short-circuits — the same pattern as the project
// endpoints.
func (h *LoopHandler) checkLoopAccess(ctx context.Context, loop *v1beta1.Loop, resume bool) error {
	caller := auth.CallerFromContext(ctx)
	if caller == nil {
		// No identity in context (e.g. direct handler invocation
		// without authentication): the middleware layer is responsible;
		// nothing to check here.
		return nil
	}
	switch caller.Role {
	case auth.RoleAdmin:
		return nil
	case auth.RoleManager:
		if resume {
			return fmt.Errorf("authorization denied: loop resume is restricted to admin and L2 human")
		}
		return nil
	case auth.RoleTeamLeader:
		if resume {
			return fmt.Errorf("authorization denied: loop resume is restricted to admin and L2 human")
		}
		return loopTeamAccess(caller, loop.Spec.TeamName)
	case auth.RoleHuman:
		return loopTeamAccess(caller, loop.Spec.TeamName)
	case auth.RoleWorker:
		return fmt.Errorf("authorization denied: worker may not control loops")
	default:
		return fmt.Errorf("authorization denied: unknown role %q", caller.Role)
	}
}

// loopTeamAccess mirrors auth.requireSameTeam semantics against the
// loop's own team: a caller with no team at all is denied; a loop
// without a team (standalone) is reachable by any team-scoped caller;
// otherwise the caller must belong to the loop's team.
func loopTeamAccess(caller *auth.CallerIdentity, teamName string) error {
	if caller.Team == "" && len(caller.Teams) == 0 {
		return fmt.Errorf("authorization denied: %s %q has no team", caller.Role, caller.Username)
	}
	if teamName == "" {
		return nil
	}
	if !caller.TeamMatches(teamName) {
		return fmt.Errorf("authorization denied: %s %q cannot access loop in team %q",
			caller.Role, caller.Username, teamName)
	}
	return nil
}
