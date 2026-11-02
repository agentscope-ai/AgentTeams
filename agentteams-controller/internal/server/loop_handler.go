package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Valid values for the pause body's "source" field.
const (
	pauseSourceUser    = "user"
	pauseSourceProject = "project"
)

// LoopHandler serves the Loop control-plane endpoints (#1402):
// POST /api/v1/loops/{name}/pause and POST /api/v1/loops/{name}/resume.
//
// The authoritative pause state is the business layer: the project meta's
// `loop.pause_sources` (execution gating, issue #1431/#1451). The Loop CRD
// is read only to resolve the loop's team/project and is NEVER patched —
// spec.paused stays a legacy field owned by the CRD itself.
type LoopHandler struct {
	Client    client.Client
	Namespace string
	OSS       oss.StorageClient
}

// NewLoopHandler builds a LoopHandler.
func NewLoopHandler(c client.Client, namespace string, o oss.StorageClient) *LoopHandler {
	return &LoopHandler{Client: c, Namespace: namespace, OSS: o}
}

// PauseLoop handles POST /api/v1/loops/{name}/pause.
//
// Body (optional): {"source": "user" | "project"} (default "user").
// The source is added to the resolved project meta's `loop.pause_sources`;
// pauses are additive, never last-writer-wins (issue #1431 gap 3).
//
// Permission (INT-7b ruling): any authenticated role except worker —
// admin/manager unrestricted, teamLeader/human restricted to their own
// team (enforced here, see checkLoopAccess).
func (h *LoopHandler) PauseLoop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx := r.Context()

	loop := &v1beta1.Loop{}
	if err := h.Client.Get(ctx, h.loopKey(name), loop); err != nil {
		httputil.WriteError(w, http.StatusNotFound, fmt.Sprintf("loop %q not found", name))
		return
	}

	var body struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httputil.WriteError(w, http.StatusBadRequest, `invalid request body: expected a JSON object with an optional "source" field`)
		return
	}
	source := strings.ToLower(strings.TrimSpace(body.Source))
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

	key, projectID, found, reason, err := h.resolveLoopProject(ctx, loop)
	if err != nil {
		httputil.WriteError(w, http.StatusConflict,
			fmt.Sprintf("cannot determine the loop's project state: %v", err))
		return
	}
	if !found {
		// Legitimate not-paused state: the loop has no project state to
		// gate. The response must not claim the loop is paused.
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"id": loop.Name, "requested": true, "paused": false,
			"sources": []string{}, "reason": reason,
		})
		return
	}

	sources, gated, err := h.setProjectPauseSource(ctx, key, source, true)
	if err != nil {
		if errors.Is(err, oss.ErrConditionalUnsupported) {
			// The backend cannot provide the conditional-write guarantee
			// this update relies on: the operation was rejected without
			// writing, so the persisted pause state is unchanged.
			httputil.WriteError(w, http.StatusServiceUnavailable,
				"this operation requires a storage backend with conditional writes; the configured backend cannot provide that guarantee, so the request was rejected without writing")
			return
		}
		httputil.WriteError(w, http.StatusConflict,
			fmt.Sprintf("cannot update the loop's project state: %v", err))
		return
	}
	if !gated {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"id": loop.Name, "requested": true, "paused": false,
			"sources": sources,
			"reason":  fmt.Sprintf("project %s has no loop record; nothing to gate", projectID),
		})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"id": loop.Name, "requested": true, "paused": len(sources) > 0,
		"sources": sources,
		"reason":  fmt.Sprintf("pause source %q recorded in project %s", source, projectID),
	})
}

// ResumeLoop handles POST /api/v1/loops/{name}/resume.
//
// Body (optional): {"source": "user" | "project"} (default "user").
// Clears ONE pause source; while any other source remains the loop stays
// paused and the response reports the remaining set (200, not 409 — a
// partial release is a success, issue #1402: the old handler's 409 on
// remaining sources is the bug being fixed).
//
// Permission: the "user" source is a human veto — only an admin may
// release it (a paused loop is a human decision; agents do not release it
// themselves). The "project" source keeps the original ruling: admin plus
// an L2 human of the loop's own team.
func (h *LoopHandler) ResumeLoop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx := r.Context()

	loop := &v1beta1.Loop{}
	if err := h.Client.Get(ctx, h.loopKey(name), loop); err != nil {
		httputil.WriteError(w, http.StatusNotFound, fmt.Sprintf("loop %q not found", name))
		return
	}

	var body struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httputil.WriteError(w, http.StatusBadRequest, `invalid request body: expected a JSON object with an optional "source" field`)
		return
	}
	source := strings.ToLower(strings.TrimSpace(body.Source))
	if source == "" {
		source = pauseSourceUser
	}
	if source != pauseSourceUser && source != pauseSourceProject {
		httputil.WriteError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid pause source %q: must be %q or %q", source, pauseSourceUser, pauseSourceProject))
		return
	}

	if source == pauseSourceUser {
		// Releasing the user's veto is admin-only. A nil caller fails
		// CLOSED (403): an unauthenticated invocation must never
		// implicitly release a human pause.
		caller := auth.CallerFromContext(ctx)
		if caller == nil || caller.Role != auth.RoleAdmin {
			httputil.WriteError(w, http.StatusForbidden,
				"authorization denied: resuming the user pause source requires admin")
			return
		}
	} else if err := h.checkLoopAccess(ctx, loop, true); err != nil {
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	key, projectID, found, reason, err := h.resolveLoopProject(ctx, loop)
	if err != nil {
		httputil.WriteError(w, http.StatusConflict,
			fmt.Sprintf("cannot determine the loop's project state: %v", err))
		return
	}
	if !found {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"id": loop.Name, "requested": true, "paused": false,
			"sources": []string{}, "reason": reason,
		})
		return
	}

	sources, gated, err := h.setProjectPauseSource(ctx, key, source, false)
	if err != nil {
		if errors.Is(err, oss.ErrConditionalUnsupported) {
			// The backend cannot provide the conditional-write guarantee
			// this update relies on: the operation was rejected without
			// writing, so the persisted pause state is unchanged.
			httputil.WriteError(w, http.StatusServiceUnavailable,
				"this operation requires a storage backend with conditional writes; the configured backend cannot provide that guarantee, so the request was rejected without writing")
			return
		}
		httputil.WriteError(w, http.StatusConflict,
			fmt.Sprintf("cannot update the loop's project state: %v", err))
		return
	}
	if !gated {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"id": loop.Name, "requested": true, "paused": false,
			"sources": sources,
			"reason":  fmt.Sprintf("project %s has no loop record; nothing to gate", projectID),
		})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"id": loop.Name, "requested": true, "paused": len(sources) > 0,
		"sources": sources,
		"reason":  fmt.Sprintf("pause source %q released from project %s", source, projectID),
	})
}

func (h *LoopHandler) loopKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: h.Namespace, Name: name}
}

// ---- project-state resolution -------------------------------------------

// resolveLoopProject resolves, in two hops, the object key of the project
// meta that gates the loop: Loop.Spec.TaskID -> task meta (its
// project_id field) -> project meta. A missing anchor is a legitimate
// "nothing to gate" state and is reported via found=false + reason;
// corrupted state (unreadable/invalid meta) is an error the caller maps
// to 409 (never a silent success).
func (h *LoopHandler) resolveLoopProject(ctx context.Context, loop *v1beta1.Loop) (key, projectID string, found bool, reason string, err error) {
	if h.OSS == nil {
		return "", "", false, "no object storage configured; no project state to gate", nil
	}
	taskID := strings.TrimSpace(loop.Spec.TaskID)
	if taskID == "" {
		return "", "", false,
			fmt.Sprintf("loop %q has no task anchor (spec.taskID empty); no project state to gate", loop.Name), nil
	}

	taskKey, taskRaw, found, err := firstExisting(ctx, h.OSS, taskMetaKeys(taskID, loop.Spec.TeamName))
	if err != nil {
		return "", "", false, "", err
	}
	if !found {
		return "", "", false,
			fmt.Sprintf("no task meta found for task %s; cannot locate the project to gate", taskID), nil
	}

	var taskMeta map[string]any
	if err := json.Unmarshal(taskRaw, &taskMeta); err != nil {
		return "", "", false, "", fmt.Errorf("task meta %s is not valid JSON: %w", taskKey, err)
	}
	projectID = strings.TrimSpace(stringField(taskMeta, "project_id", "projectId"))
	if projectID == "" {
		return "", "", false, "",
			fmt.Errorf("task meta %s carries no project_id; cannot determine the owning project", taskKey)
	}

	projectKey, _, found, err := firstExisting(ctx, h.OSS, projectMetaKeys(projectID, loop.Spec.TeamName))
	if err != nil {
		return "", "", false, "", err
	}
	if !found {
		return "", "", false,
			fmt.Sprintf("project %s has no meta.json; no project state to gate", projectID), nil
	}
	return projectKey, projectID, true, "", nil
}

// pauseSourceCASBackoffs is the conflict-retry schedule for
// setProjectPauseSource: 10–50ms across the 5 allowed retries (with up to
// 50% additive jitter, the same scheme as the audit append loop).
var pauseSourceCASBackoffs = [5]time.Duration{
	10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond,
	40 * time.Millisecond, 50 * time.Millisecond,
}

// conditionalWriter is the strict conditional write that pause-source
// updates require. It is an optional interface (deliberately NOT part of
// oss.StorageClient) so backends whose conditional write can degrade to a
// compare-then-put — or that report no ETag — are rejected for
// conflict-sensitive writes while keeping the weak path available for the
// callers that accept it.
type conditionalWriter interface {
	PutObjectIfMatchStrict(context.Context, string, []byte, string) error
}

// setProjectPauseSource adds or removes one source in the project meta's
// `loop.pause_sources` at the given key. The read/modify/write runs as an
// ETag-optimistic-concurrency loop (same pattern as the audit append
// loop): stat, get, modify, strict conditional write, and retry with
// backoff when a concurrent writer won the race — so two overlapping pause
// requests can never silently erase each other's source. It re-reads the
// object after the write and returns the PERSISTED source set, so the
// response reports what the dispatcher will actually see. gated=false
// means the meta has no loop record (nothing on the execution side to
// gate) or the meta vanished between resolution and write — the caller
// must not claim the loop is paused. An exhausted retry is an error, never
// a silent success.
//
// Losing a source is a correctness bug (a released human veto, a lost
// project hold), so the write is conflict-safe by construction: it goes
// through the backend's STRICT conditional write (no compare-then-put
// degradation) and requires a non-empty ETag. A backend without a strict
// write, or one that reports no ETag for the existing object, is rejected
// with oss.ErrConditionalUnsupported (mapped to 503) — never an
// unconditional write.
func (h *LoopHandler) setProjectPauseSource(ctx context.Context, key, source string, set bool) (sources []string, gated bool, err error) {
	strict, ok := h.OSS.(conditionalWriter)
	if !ok {
		return nil, false, fmt.Errorf("pause-source updates require a backend with conditional writes: %w", oss.ErrConditionalUnsupported)
	}
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			delay := pauseSourceCASBackoffs[attempt-1]
			delay += time.Duration(rand.Int63n(int64(delay/2) + 1))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
		}

		// Stat before get: if a concurrent writer commits between the two
		// calls, the body read below is NEWER than the captured ETag, so
		// the conditional write fails with 412 and retries (fail-safe).
		// Reading the body first would pair a stale body with a fresh
		// ETag and silently drop the concurrent writer's pause source.
		var data []byte
		etag := ""
		if meta, merr := h.OSS.StatMeta(ctx, key); merr == nil {
			etag = meta.ETag
			data, err = h.OSS.GetObject(ctx, key)
			switch {
			case err == nil:
			case errors.Is(err, os.ErrNotExist):
				// Vanished between stat and get: there is no loop record
				// to gate, so stop without writing.
				return []string{}, false, nil
			default:
				return nil, false, fmt.Errorf("get %s: %w", key, err)
			}
		} else if !errors.Is(merr, os.ErrNotExist) {
			return nil, false, fmt.Errorf("stat %s: %w", key, merr)
		} else {
			// No object at all: no loop record to gate; never invent a
			// project meta.
			return []string{}, false, nil
		}

		var metaMap map[string]any
		if err := json.Unmarshal(data, &metaMap); err != nil {
			return nil, false, fmt.Errorf("decode %s: %w", key, err)
		}
		loop, ok := metaMap["loop"].(map[string]any)
		if !ok || loop == nil {
			return []string{}, false, nil // no loop record: nothing to gate
		}
		next := parsePauseSourcesMeta(loop)
		if set {
			next = addPauseSource(next, source)
		} else {
			next = removePauseSource(next, source)
		}
		loop["pause_sources"] = next
		// Keep the legacy project flag consistent for older readers. The
		// flag and the source set travel in the SAME conditional write,
		// so they can never drift apart.
		if source == pauseSourceProject {
			loop["paused_by_project"] = set
		}
		metaMap["loop"] = loop
		updated, uerr := json.MarshalIndent(metaMap, "", "  ")
		if uerr != nil {
			return nil, false, fmt.Errorf("encode %s: %w", key, uerr)
		}
		updated = append(updated, '\n')

		if etag == "" {
			// The object exists but the backend reported no ETag: no
			// conditional write is possible, and a plain put would race
			// with a concurrent writer — reject instead of writing
			// unconditionally.
			return nil, false, fmt.Errorf("put-if-match %s: %w", key, oss.ErrConditionalUnsupported)
		}
		if perr := strict.PutObjectIfMatchStrict(ctx, key, updated, etag); perr != nil {
			if errors.Is(perr, oss.ErrPreconditionFailed) && attempt < len(pauseSourceCASBackoffs) {
				continue // someone wrote concurrently; re-read and retry
			}
			// ErrConditionalUnsupported is not retried: the backend
			// cannot provide the guarantee at all, so retrying changes
			// nothing.
			return nil, false, fmt.Errorf("put-if-match %s: %w", key, perr)
		}
		sources = next
		break
	}
	// Re-read the persisted state (the response must reflect durable
	// state, not intent).
	data, err := h.OSS.GetObject(ctx, key)
	if err != nil {
		return nil, false, err
	}
	var reRead map[string]any
	if err := json.Unmarshal(data, &reRead); err != nil {
		return nil, false, err
	}
	if loop, ok := reRead["loop"].(map[string]any); ok && loop != nil {
		sources = parsePauseSourcesMeta(loop)
	}
	return sources, true, nil
}

// projectMetaKeys returns the candidate object keys for a project meta,
// team-scoped first (the writer uses the owning team's scope).
func projectMetaKeys(projectID, teamName string) []string {
	keys := make([]string, 0, 2)
	if teamName != "" {
		keys = append(keys, fmt.Sprintf("teams/%s/shared/projects/%s/meta.json", teamName, projectID))
	}
	return append(keys, fmt.Sprintf("shared/projects/%s/meta.json", projectID))
}

// firstExisting returns the first candidate key present in the store, with
// its content. found=false when none exist (os.ErrNotExist); any other
// storage error is propagated.
func firstExisting(ctx context.Context, store oss.StorageClient, keys []string) (string, []byte, bool, error) {
	for _, key := range keys {
		data, err := store.GetObject(ctx, key)
		if err == nil {
			return key, data, true, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return "", nil, false, err
	}
	return "", nil, false, nil
}

// stringField returns the first non-empty string among the given keys.
func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// ---- pause-source set helpers -------------------------------------------

// parsePauseSourcesMeta normalizes meta["loop"]["pause_sources"] into a
// clean set (lowercased, trimmed, deduped, sorted).
func parsePauseSourcesMeta(loop map[string]any) []string {
	out := []string{}
	raw, _ := loop["pause_sources"].([]any)
	seen := map[string]bool{}
	for _, item := range raw {
		value, ok := item.(string)
		if !ok {
			continue
		}
		source := strings.ToLower(strings.TrimSpace(value))
		if source == "" || seen[source] {
			continue
		}
		seen[source] = true
		out = append(out, source)
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

// ---- authorization -------------------------------------------------------

// checkLoopAccess re-checks the caller against the loop. It is defense
// in depth on top of the RequireAuthz middleware, and the REAL
// team-scope boundary: the middleware cannot resolve loop -> team, so the
// handler enforces the team scope (same pattern as the project endpoints).
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
