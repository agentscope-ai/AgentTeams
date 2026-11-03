package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Task meta conditional commit (#1471, follow-up to the project meta commit
// of #1402).
//
// The worker's filesync path used to mirror the whole task directory
// (`mc mirror`), including meta.json, with no version binding: a worker
// write-back started from a cached copy — submit/ack/report/request_attention
// all push the WHOLE task record — could overwrite what the Controller wrote
// in between. The Controller's CancelTask is exactly such a writer: it
// persists status=cancelled, cancel_reason, cancelled_at,
// replacement_task_id and a resolved continuation, and an ordinary worker
// sync from a stale cache erased them.
//
// This endpoint is the task-level counterpart of the project commit: the
// caller passes the ETag it read, the write is a strict If-Match CAS, and
// the server refuses to let a commit release the Controller's cancellation.
//
// The first creation is the same guarantee with If-None-Match: *
// (create-if-absent): the object is created only while absent, so a
// concurrent create + cancellation that lands between the caller's absence
// read and the write turns the create into a 409 — the caller re-reads the
// now-versioned record and converges on it — instead of clobbering the
// cancelled state with an unguarded directory push.

// taskMetaCommitMaxBytes bounds the commit body. A task meta carries the
// result fence, the continuity block and the capped transition history; it
// is far smaller than the project meta, but the same 1 MiB ceiling keeps one
// limit for both commit endpoints.
const taskMetaCommitMaxBytes = 1 << 20

// canonicalTaskStatuses are the exact task-status values the runtimes read:
// the Go project workflow (isTerminalTaskStatus and the completion math) and
// the TeamHarness transition table compare with ==, so any other spelling
// would persist a state no reader interprets as the writer meant.
var canonicalTaskStatuses = map[string]struct{}{
	"planned":     {},
	"prepared":    {},
	"assigned":    {},
	"in_progress": {},
	"submitted":   {},
	"completed":   {},
	"revision":    {},
	"blocked":     {},
	"cancelled":   {},
}

func canonicalTaskStatus(status string) bool {
	_, ok := canonicalTaskStatuses[status]
	return ok
}

// taskCancellationFields are the fields CancelTask writes in the same
// read-modify-write batch as status=cancelled. A cancelled task is terminal,
// so a later commit may keep them but never change or drop them.
var taskCancellationFields = []string{
	"cancel_reason",
	"cancelled_at",
	"replacement_task_id",
}

// exactTaskStatus returns the exact-key top-level "status" of a task meta and
// whether the key is present at all (raw value: the endpoint stores bytes
// verbatim, so validation must not normalize).
func exactTaskStatus(meta []byte) (status string, present bool, hasAlias bool) {
	if len(meta) == 0 {
		return "", false, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(meta, &top); err != nil {
		return "", false, false
	}
	for key, raw := range top {
		switch {
		case key == "status":
			present = true
			_ = json.Unmarshal(raw, &status)
		case strings.EqualFold(key, "status"):
			hasAlias = true
		}
	}
	return status, present, hasAlias
}

// rawJSONField walks a JSON object by exact key path and returns the raw
// value. It is used for byte-identical comparisons of values this endpoint
// protects (cancellation fields, the resolved continuation).
func rawJSONField(meta []byte, keys ...string) (json.RawMessage, bool) {
	var current json.RawMessage = meta
	for _, key := range keys {
		if len(current) == 0 {
			return nil, false
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(current, &object); err != nil {
			return nil, false
		}
		value, ok := object[key]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

// sameJSONValue reports whether two raw JSON values are semantically equal
// (key order and whitespace do not matter).
func sameJSONValue(a, b json.RawMessage) bool {
	ca, errA := compactJSON(a)
	cb, errB := compactJSON(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

func compactJSON(raw json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resolveTaskMetaCommitKey resolves {id} (plus an optional ?team= qualifier)
// to the stored task meta key. Task ids are project-scoped in the runtime but
// the object store keeps them per team, so the same id can exist in two
// teams; an ambiguous id is a 409 with the ?team= hint (same rule as the
// project endpoints).
func (h *ProjectHandler) resolveTaskMetaCommitKey(
	w http.ResponseWriter, r *http.Request, taskID string,
) (key string, team string, ok bool) {
	ctx := r.Context()
	caller := authpkg.CallerFromContext(ctx)
	teamFilter := strings.TrimSpace(r.URL.Query().Get("team"))

	prefixes, crToEffective, err := h.taskMetaPrefixes(ctx)
	if err != nil {
		writeK8sError(w, "resolve task prefixes", err)
		return "", "", false
	}
	type hit struct {
		key  string
		team string
	}
	var hits []hit
	for _, candidate := range prefixes {
		candidateKey := candidate + taskID + "/meta.json"
		if _, err := h.oss.StatMeta(ctx, candidateKey); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "stat task meta: "+err.Error())
			return "", "", false
		}
		hits = append(hits, hit{key: candidateKey, team: candidateTeam(candidate)})
	}
	if teamFilter != "" {
		filtered := hits[:0]
		for _, candidate := range hits {
			if candidate.team == teamFilter {
				filtered = append(filtered, candidate)
			}
		}
		hits = filtered
	}
	switch len(hits) {
	case 0:
		writeError(w, http.StatusNotFound, "task not found")
		return "", "", false
	case 1:
	default:
		httputil.WriteJSON(w, http.StatusConflict, struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{Code: "ambiguous_task", Message: "task id exists in several teams; pass ?team=<team>"})
		return "", "", false
	}
	key, team = hits[0].key, hits[0].team

	// Access boundary. The middleware cannot resolve task -> team (the
	// resource name carries only the id), so the handler is the real check:
	// a worker may commit only its own team's task; team leaders and L2
	// humans only tasks of a team they can access (404, never 403, so the
	// existence of another team's task is not probeable); admin/manager have
	// full access.
	if caller != nil {
		switch caller.Role {
		case authpkg.RoleWorker:
			if !caller.TeamMatches(team) {
				writeError(w, http.StatusForbidden, "worker can only commit the meta of a task in its own team")
				return "", "", false
			}
		case authpkg.RoleTeamLeader, authpkg.RoleHuman:
			if err := h.checkProjectAccess(caller, team, crToEffective); err != nil {
				if _, denied := err.(*accessDeniedError); denied {
					writeError(w, http.StatusNotFound, "task not found")
					return "", "", false
				}
				writeError(w, http.StatusForbidden, err.Error())
				return "", "", false
			}
		}
	}
	return key, team, true
}

// taskMetaPrefixes lists the task storage prefixes (per team, plus the
// standalone/global scope) and the CR -> effective team map used by the
// scoped access check.
func (h *ProjectHandler) taskMetaPrefixes(ctx context.Context) ([]string, map[string]string, error) {
	var teams v1beta1.TeamList
	if err := h.client.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		return nil, nil, err
	}
	prefixes := make([]string, 0, len(teams.Items))
	crToEffective := make(map[string]string, len(teams.Items))
	seen := map[string]bool{}
	for i := range teams.Items {
		effective := teams.Items[i].Spec.EffectiveTeamName(teams.Items[i].Name)
		crToEffective[teams.Items[i].Name] = effective
		for _, name := range []string{effective, teams.Items[i].Name} {
			prefix := "teams/" + name + "/shared/tasks/"
			if !seen[prefix] {
				seen[prefix] = true
				prefixes = append(prefixes, prefix)
			}
		}
	}
	prefixes = append(prefixes, "shared/tasks/")
	return prefixes, crToEffective, nil
}

// candidateTeam extracts the team segment from a task prefix
// ("teams/<team>/shared/tasks/" -> "<team>"; the global "shared/tasks/" -> "").
func candidateTeam(prefix string) string {
	trimmed := strings.TrimPrefix(prefix, "teams/")
	if trimmed == prefix {
		return ""
	}
	if idx := strings.Index(trimmed, "/"); idx >= 0 {
		return trimmed[:idx]
	}
	return trimmed
}

// PutTaskMeta conditionally commits a task's full meta.json against the
// version the caller read (#1471) — or creates it on the first creation
// (create-if-absent, If-None-Match: *). See the file header for why.
func (h *ProjectHandler) PutTaskMeta(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(r.PathValue("id"))
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	ifNoneMatch := strings.TrimSpace(r.Header.Get("If-None-Match"))
	switch {
	case ifMatch != "" && ifNoneMatch != "":
		writeError(w, http.StatusBadRequest,
			"pass exactly one of If-Match (update) or If-None-Match: * (create-if-absent), not both")
		return
	case ifMatch == "" && ifNoneMatch == "":
		writeError(w, http.StatusPreconditionRequired,
			"If-Match is required: read the task meta and pass its ETag; this endpoint never blind-writes")
		return
	case ifNoneMatch != "" && ifNoneMatch != "*":
		writeError(w, http.StatusBadRequest,
			`If-None-Match must be the literal "*" (create-if-absent); a specific ETag is not supported`)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "task meta commit body exceeds 1 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "read task meta commit body: "+err.Error())
		return
	}
	var asObject map[string]json.RawMessage
	if err := json.Unmarshal(body, &asObject); err != nil {
		writeError(w, http.StatusBadRequest, "task meta commit body must be a JSON object: "+err.Error())
		return
	}
	if asObject == nil {
		writeError(w, http.StatusBadRequest, "task meta commit body must be a JSON object, not null")
		return
	}
	if ifNoneMatch == "*" {
		h.putTaskMetaCreateIfAbsent(w, r, taskID, body)
		return
	}

	key, _, ok := h.resolveTaskMetaCommitKey(w, r, taskID)
	if !ok {
		return
	}

	bodyStatus, bodyStatusPresent, statusAlias := exactTaskStatus(body)
	if statusAlias {
		writeError(w, http.StatusBadRequest,
			`ambiguous case alias for "status": the runtimes persist and read the exact lowercase key`)
		return
	}
	statusCanonical := !bodyStatusPresent || canonicalTaskStatus(bodyStatus)

	current, readErr := h.oss.GetObject(r.Context(), key)
	switch {
	case readErr == nil:
	case errors.Is(readErr, os.ErrNotExist):
		// The resolution above stat'ed the object; a vanish between stat and
		// get means there is nothing to commit against.
		writeError(w, http.StatusNotFound, "task not found")
		return
	default:
		writeError(w, http.StatusInternalServerError, "read task meta: "+readErr.Error())
		return
	}
	currentStatus, currentStatusPresent, _ := exactTaskStatus(current)
	// The commit must not drop the status: a task meta without one is not a
	// state any runtime can interpret, and no legitimate client omits it
	// (the sync commits the merged record).
	if !bodyStatusPresent && currentStatusPresent {
		writeError(w, http.StatusBadRequest,
			`task meta commit must carry "status": omitting it would drop the field from the stored record`)
		return
	}
	if !statusCanonical && !(currentStatusPresent && bodyStatus == currentStatus) {
		writeError(w, http.StatusBadRequest,
			"noncanonical task status "+strconv.Quote(bodyStatus)+
				": the runtimes compare the exact string, so the stored value would not mean what it spells")
		return
	}

	// A cancelled task is terminal (the TeamHarness transition table and the
	// project workflow both treat it that way), so the Controller's
	// cancellation — and the fields written with it — are server-owned.
	if currentStatusPresent && currentStatus == "cancelled" {
		if bodyStatus != "cancelled" {
			writeError(w, http.StatusForbidden,
				"a cancelled task is terminal; the metadata commit cannot reopen it (status is server-owned)")
			return
		}
		for _, field := range taskCancellationFields {
			currentValue, currentHas := rawJSONField(current, field)
			if !currentHas {
				continue
			}
			bodyValue, bodyHas := rawJSONField(body, field)
			if !bodyHas || !sameJSONValue(currentValue, bodyValue) {
				writeError(w, http.StatusForbidden,
					"the task cancellation is server-owned; a meta commit cannot change "+strconv.Quote(field))
				return
			}
		}
		if currentContinuation, hasContinuation := rawJSONField(current, "continuation"); hasContinuation {
			status, _ := rawJSONField(currentContinuation, "status")
			resolution, _ := rawJSONField(currentContinuation, "resolution")
			if sameJSONValue(status, json.RawMessage(`"resolved"`)) &&
				sameJSONValue(resolution, json.RawMessage(`"cancelled"`)) {
				bodyContinuation, bodyHas := rawJSONField(body, "continuation")
				bodyStatusValue, _ := rawJSONField(bodyContinuation, "status")
				bodyResolution, _ := rawJSONField(bodyContinuation, "resolution")
				if !bodyHas ||
					!sameJSONValue(bodyStatusValue, json.RawMessage(`"resolved"`)) ||
					!sameJSONValue(bodyResolution, json.RawMessage(`"cancelled"`)) {
					writeError(w, http.StatusForbidden,
						"the resolved cancellation continuation is server-owned; a meta commit cannot reopen it")
					return
				}
			}
		}
	}

	// The unified exit is the STRICT conditional write, exactly like the
	// project meta commit: a backend whose If-Match can degrade to
	// compare-then-put — or that does not implement it — is rejected with
	// 503, never an unconditional write.
	strict, ok := h.oss.(conditionalWriter)
	if !ok {
		writeError(w, http.StatusServiceUnavailable,
			"task meta commit requires a backend with strict conditional writes: "+oss.ErrConditionalUnsupported.Error())
		return
	}
	if err := strict.PutObjectIfMatchStrict(r.Context(), key, body, ifMatch); err != nil {
		switch {
		case errors.Is(err, oss.ErrPreconditionFailed):
			writeMetaConflict(w)
		case errors.Is(err, oss.ErrConditionalUnsupported):
			writeError(w, http.StatusServiceUnavailable,
				"backend cannot provide conditional writes: "+oss.ErrConditionalUnsupported.Error())
		default:
			writeError(w, http.StatusInternalServerError, "commit task meta: "+err.Error())
		}
		return
	}
	committed, err := h.oss.StatMeta(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stat task meta after commit: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, struct {
		ETag string `json:"etag"`
	}{ETag: committed.ETag})
}

// putTaskMetaCreateIfAbsent commits the first creation of the task meta
// (If-None-Match: *). Unlike the update path it does NOT resolve the key
// through the stored object — the object is absent by definition, so it
// resolves the key deterministically from the ?team= qualifier (the
// worker's own team, or the global scope), validates the body the same
// way the update path does, and lands the STRICT create-if-absent write:
// a concurrent create that appears in between turns it into a 409
// (ErrPreconditionFailed), never a clobber of the concurrent record.
func (h *ProjectHandler) putTaskMetaCreateIfAbsent(
	w http.ResponseWriter, r *http.Request, taskID string, body []byte,
) {
	ctx := r.Context()
	caller := authpkg.CallerFromContext(ctx)
	teamFilter := strings.TrimSpace(r.URL.Query().Get("team"))

	prefixes, crToEffective, err := h.taskMetaPrefixes(ctx)
	if err != nil {
		writeK8sError(w, "resolve task prefixes", err)
		return
	}
	var key string
	if teamFilter != "" {
		teamPrefix := "teams/" + teamFilter + "/shared/tasks/"
		inPrefixes := false
		for _, candidate := range prefixes {
			if candidate == teamPrefix {
				inPrefixes = true
				break
			}
		}
		if !inPrefixes {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		key = teamPrefix + taskID + "/meta.json"
	} else {
		key = "shared/tasks/" + taskID + "/meta.json"
	}
	team := teamFilter

	// Access boundary (same rules as the update path).
	if caller != nil {
		switch caller.Role {
		case authpkg.RoleWorker:
			if !caller.TeamMatches(team) {
				writeError(w, http.StatusForbidden, "worker can only create the meta of a task in its own team")
				return
			}
		case authpkg.RoleTeamLeader, authpkg.RoleHuman:
			if err := h.checkProjectAccess(caller, team, crToEffective); err != nil {
				if _, denied := err.(*accessDeniedError); denied {
					writeError(w, http.StatusNotFound, "task not found")
					return
				}
				writeError(w, http.StatusForbidden, err.Error())
				return
			}
		}
	}

	bodyStatus, bodyStatusPresent, statusAlias := exactTaskStatus(body)
	if statusAlias {
		writeError(w, http.StatusBadRequest,
			`ambiguous case alias for "status": the runtimes persist and read the exact lowercase key`)
		return
	}
	if !bodyStatusPresent {
		writeError(w, http.StatusBadRequest,
			`task meta creation must carry "status": a task meta without one is not a state any runtime can interpret`)
		return
	}
	if !canonicalTaskStatus(bodyStatus) {
		writeError(w, http.StatusBadRequest,
			"noncanonical task status "+strconv.Quote(bodyStatus)+
				": the runtimes compare the exact string, so the stored value would not mean what it spells")
		return
	}

	// The object must not exist: an existing record (a concurrent create,
	// possibly already cancelled) is a conflict — the caller re-reads it
	// versioned and converges on it.
	if _, err := h.oss.StatMeta(ctx, key); err == nil {
		writeMetaConflict(w)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "stat task meta: "+err.Error())
		return
	}

	// The unified exit is the STRICT create-if-absent write, exactly like
	// the update path's strict If-Match write: a backend without it is
	// rejected with 503, never a check-then-create.
	strict, ok := h.oss.(createIfAbsentWriter)
	if !ok {
		writeError(w, http.StatusServiceUnavailable,
			"task meta creation requires a backend with strict create-if-absent writes: "+oss.ErrConditionalUnsupported.Error())
		return
	}
	if err := strict.PutObjectIfNoneMatchStrict(ctx, key, body); err != nil {
		switch {
		case errors.Is(err, oss.ErrPreconditionFailed):
			writeMetaConflict(w)
		case errors.Is(err, oss.ErrConditionalUnsupported):
			writeError(w, http.StatusServiceUnavailable,
				"backend cannot provide create-if-absent writes: "+oss.ErrConditionalUnsupported.Error())
		default:
			writeError(w, http.StatusInternalServerError, "create task meta: "+err.Error())
		}
		return
	}
	committed, err := h.oss.StatMeta(ctx, key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stat task meta after create: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, struct {
		ETag string `json:"etag"`
	}{ETag: committed.ETag})
}

// createIfAbsentWriter is the narrow backend capability the create path
// needs (same call-site assertion pattern as conditionalWriter: the strict
// write is intentionally not part of the wide StorageClient interface).
type createIfAbsentWriter interface {
	PutObjectIfNoneMatchStrict(ctx context.Context, key string, data []byte) error
}
