package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TH-4: the dashboard attention resolve endpoint.
//
// POST /api/v1/projects/{id}/attention/{taskId}/resolve
//
// A human dashboard user answers the task's pending attention request (the
// workbench decision modal; read surface: GetProjectAttention). The answer is
// written into the task meta's attention record as a read-modify-write bound
// to the read version (#1498 strict conditional write) carrying the resolve
// provenance — resolvedVia "dashboard", resolvedBy = the authenticated
// session identity, resolvedAt = server clock — and the handler posts the ✅
// line to the task room (best-effort) and emits the attention_resolved S2
// signal event.
//
// kind=approval closes additionally persist the SERVER-OWNED authority
// markers the #1506 consumer reads (teamharness review._has_approval_record
// and the frozen-task release in taskflow): resolvedRole = the caller's
// authenticated role, resolvedActor = "role:account" with the account in
// the consumer's canonical form. Both derive from the authenticated
// CallerIdentity — a payload key of either name has no field in
// resolveAttentionRequest and is dropped at decode, never copied — and
// resolvedBy stays display provenance: the consumer never decides on it,
// and a record without the server markers is not an approval (fail closed).
//
// The handler never touches decision_status: the stall-driven "frozen"
// state is released by the consumer DERIVING it from this marked resolved
// approval record (the taskflow frozen-unfreeze guard), so writing the
// record shape correctly is this side's whole obligation. The consumer-side
// combined test (frozen task + this record shape -> r1 acceptance +
// unfreeze) lands with #1506.
//
// The validation order is fixed (spec):
//
//	1) project / task not found                 -> 404
//	2) no pending attention                     -> 409 attention_not_pending
//	                                                 (exact re-submit of the last
//	                                                  resolution                   -> 200 idempotent
//	                                                  otherwise already-resolved   -> 409 attention_already_resolved)
//	3) caller not in the team's human roster    -> 403 not_a_human_member
//	4) decision resolution not an option id, no
//	   freeform declaration                      -> 400 resolution_not_an_option
//	5) comment > 500 chars / evidence > 4KB     -> 400
//	6) CAS conflict                             -> whole-step retry (<=3 full
//	                                                 attempts), exhausted         -> 409 task_changed_during_resolve
//
// Workers never reach the handler: the route is ActionUpdate + "project",
// which the authorizer denies to the worker role (the cancel/pause precedent)
// — only humans named in the team's human roster can resolve.

const (
	attentionResolveMaxCommentRunes  = 500
	attentionResolveMaxEvidenceBytes = 4 * 1024
	attentionResolveMaxAttempts      = 3
	attentionResolveByMaxRunes       = 200
)

type resolveAttentionRequest struct {
	Resolution string          `json:"resolution"`
	Comment    string          `json:"comment"`
	Evidence   json.RawMessage `json:"evidence"`
}

type resolveAttentionResponse struct {
	TaskID      string `json:"taskId"`
	Resolved    bool   `json:"resolved"`
	Idempotent  bool   `json:"idempotent"`
	ResolvedVia string `json:"resolvedVia"`
	ResolvedBy  string `json:"resolvedBy"`
	ResolvedAt  string `json:"resolvedAt"`
	EventID     string `json:"eventId"`
}

// resolveValidationError is one rejected step of the fixed validation order.
type resolveValidationError struct {
	status int
	code   string
	msg    string
}

func (e *resolveValidationError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.status, e.code, e.msg)
}

func resolveErr(status int, code, msg string) *resolveValidationError {
	return &resolveValidationError{status: status, code: code, msg: msg}
}

// writeResolveError writes the machine-readable resolve error body (the same
// {code, message} shape as the meta commit conflict body).
func writeResolveError(w http.ResponseWriter, status int, code, msg string) {
	httputil.WriteJSON(w, status, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: msg})
}

// ResolveAttention implements POST /api/v1/projects/{id}/attention/{taskId}/resolve.
func (h *ProjectHandler) ResolveAttention(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	taskID := r.PathValue("taskId")
	if projectID == "" || taskID == "" {
		writeResolveError(w, http.StatusBadRequest, "missing_id", "project id and task id are required")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		// Unreachable behind the RequireAuthz route; kept so a direct call
		// denies (human-only surface) instead of dereferencing.
		writeResolveError(w, http.StatusForbidden, "not_a_human_member", "caller is not a human member of the team")
		return
	}

	var req resolveAttentionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeResolveError(w, http.StatusBadRequest, "invalid_body", "request body must be a JSON object")
		return
	}
	resolution := strings.TrimSpace(req.Resolution)
	comment := strings.TrimSpace(req.Comment)
	if resolution == "" {
		writeResolveError(w, http.StatusBadRequest, "resolution_required", "resolution is required")
		return
	}
	var evidence map[string]any
	if len(req.Evidence) > 0 {
		if err := json.Unmarshal(req.Evidence, &evidence); err != nil || evidence == nil {
			writeResolveError(w, http.StatusBadRequest, "evidence_not_object", "evidence must be a JSON object")
			return
		}
	}
	freeform := evidence != nil && evidence["freeform"] == true

	// 1) project + task must exist and belong to the project — all three
	// cases (unknown project, task outside the graph, missing meta) are 404
	// so the endpoint never probes existence.
	teamFilter := r.URL.Query().Get("team")
	prefixes, crToEffective, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "resolve attention: resolve prefixes", err)
		return
	}
	matches, err := h.resolveProjectMeta(r.Context(), projectID, prefixes, teamFilter, caller, crToEffective)
	if err != nil {
		writeK8sError(w, "resolve attention: resolve project", err)
		return
	}
	match, ok := h.resolveSingleProjectMatch(w, matches)
	if !ok {
		return
	}
	if err := h.checkProjectAccess(caller, match.team, crToEffective); err != nil {
		if _, denied := err.(*accessDeniedError); denied {
			writeResolveError(w, http.StatusNotFound, "not_found", "project not found")
			return
		}
		writeResolveError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	graphTasks := match.meta.Tasks
	if match.meta.PlanType == "loop" && match.meta.Loop != nil {
		graphTasks = match.meta.Loop.Tasks
	}
	inGraph := false
	for _, node := range graphTasks {
		if node.TaskID == taskID {
			inGraph = true
			break
		}
	}
	if !inGraph {
		writeResolveError(w, http.StatusNotFound, "not_found", "task not found in project")
		return
	}
	taskKey := taskMetaKeys(taskID, match.team)[0]
	data, err := h.oss.GetObject(r.Context(), taskKey)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeResolveError(w, http.StatusNotFound, "not_found", "task not found")
			return
		}
		writeK8sError(w, "resolve attention: read task meta", err)
		return
	}
	taskData, etag, err := parseTaskMetaVersioned(data)
	if err != nil {
		writeResolveError(w, http.StatusNotFound, "not_found", "task not found")
		return
	}
	if str(taskData["task_id"]) != taskID || str(taskData["project_id"]) != projectID {
		writeResolveError(w, http.StatusNotFound, "not_found", "task not found in project")
		return
	}

	strict, ok := h.oss.(conditionalWriter)
	if !ok {
		writeResolveError(w, http.StatusServiceUnavailable, "conditional_unsupported", "storage backend does not support strict conditional writes")
		return
	}

	resolvedBy := truncateRunes(caller.Username, attentionResolveByMaxRunes)
	resolvedAt := utcTimestamp()
	// Server-owned approval authority markers (#1515, consumer contract
	// #1467 round 8): derived from the authenticated identity, never from
	// the request payload (resolveAttentionRequest has no such fields).
	// The account is written in the consumer's canonical form
	// (canonicalAccount mirrors the teamharness _canonical_account), so
	// this dashboard close and the IM/room close mark the same decider
	// identically.
	resolvedRole := caller.Role
	resolvedActor := caller.Role + ":" + canonicalAccount(caller.Username)
	em := signalEmitters["ResolveAttention"]
	teamID := signalTeamID(match.meta, match.team)

	// 2-6) read-modify-write loop: up to attentionResolveMaxAttempts full
	// read→validate→write attempts; each conflict re-reads the LATEST version
	// and re-runs every check on it (a concurrent writer may have resolved
	// the attention inside the window).
	for attempt := 0; attempt < attentionResolveMaxAttempts; attempt++ {
		if attempt > 0 {
			data, err = h.oss.GetObject(r.Context(), taskKey)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					writeResolveError(w, http.StatusNotFound, "not_found", "task not found")
					return
				}
				writeK8sError(w, "resolve attention: re-read task meta", err)
				return
			}
			taskData, etag, err = parseTaskMetaVersioned(data)
			if err != nil {
				writeResolveError(w, http.StatusNotFound, "not_found", "task not found")
				return
			}
		}
		record, idempotent, valErr := h.evaluateResolveAttempt(r.Context(), match.team, taskData, resolvedBy, resolution, comment, req.Evidence, evidence, freeform)
		if valErr != nil {
			writeResolveError(w, valErr.status, valErr.code, valErr.msg)
			return
		}
		if idempotent != nil {
			// 7) exact re-submit of the last resolution: 200 with the
			// ORIGINAL provenance; no room message, no re-emit.
			ref := attentionResolveRef(taskID, intFromAny(idempotent["attempt"]), str(idempotent["resolution"]), str(idempotent["resolvedAt"]))
			fp := sha256.Sum256([]byte(ref))
			eventID := signalEventID(teamID, projectID, taskID, "", em.EventType, em.SourceEvent, nil, hex.EncodeToString(fp[:]), 0, intFromAny(taskData["audit_seq"]))
			httputil.WriteJSON(w, http.StatusOK, resolveAttentionResponse{
				TaskID:      taskID,
				Resolved:    true,
				Idempotent:  true,
				ResolvedVia: str(idempotent["resolvedVia"]),
				ResolvedBy:  str(idempotent["resolvedBy"]),
				ResolvedAt:  str(idempotent["resolvedAt"]),
				EventID:     eventID,
			})
			return
		}
		// Mutate the pending record in place (attempt is preserved).
		record["resolved"] = true
		record["resolution"] = resolution
		if comment != "" {
			record["comment"] = comment
		} else {
			delete(record, "comment")
		}
		if evidence != nil {
			record["resolveEvidence"] = evidence
		} else {
			delete(record, "resolveEvidence")
		}
		record["freeform"] = freeform
		record["resolvedVia"] = "dashboard"
		record["resolvedBy"] = resolvedBy
		record["resolvedAt"] = resolvedAt
		if str(record["kind"]) == "approval" {
			// Server-owned authority markers (#1515): written from the
			// authenticated identity, never from the request payload — a
			// payload-supplied resolvedRole / resolvedActor cannot reach
			// the record (no such struct field, dropped at decode). The
			// #1506 r1 gate decides ONLY on these: resolvedRole "human"
			// plus the resolvedActor account; resolvedBy is display
			// provenance, and a record without the markers is not an
			// approval (fail closed).
			record["resolvedRole"] = resolvedRole
			record["resolvedActor"] = resolvedActor
		}
		out, err := json.Marshal(taskData)
		if err != nil {
			writeResolveError(w, http.StatusInternalServerError, "marshal_failed", "marshal task meta: "+err.Error())
			return
		}
		if err := strict.PutObjectIfMatchStrict(r.Context(), taskKey, out, etag); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue // a concurrent writer won; re-read and re-validate
			}
			if errors.Is(err, oss.ErrConditionalUnsupported) {
				writeResolveError(w, http.StatusServiceUnavailable, "conditional_unsupported", "storage backend conditional write rejected")
				return
			}
			writeK8sError(w, "resolve attention: write task meta", err)
			return
		}
		// The write committed: emit the signal event (best-effort, deduped)
		// and post the room line (best-effort, first resolve only).
		ref := attentionResolveRef(taskID, intFromAny(record["attempt"]), resolution, resolvedAt)
		eventID := h.emitSignalEventID(r.Context(), em, match.team, teamID, projectID, taskID, nil, ref, []byte(ref), map[string]any{
			"project_id":  projectID,
			"resolution":  resolution,
			"freeform":    freeform,
			"resolved_by": resolvedBy,
		}, intFromAny(taskData["audit_seq"]))
		roomID := str(taskData["room_id"])
		if roomID == "" {
			roomID = str(taskData["roomId"])
		}
		h.resolveAttentionNotify(r.Context(), roomID, attentionResolveAnswer(record, resolution), resolvedBy)
		httputil.WriteJSON(w, http.StatusOK, resolveAttentionResponse{
			TaskID:      taskID,
			Resolved:    true,
			Idempotent:  false,
			ResolvedVia: "dashboard",
			ResolvedBy:  resolvedBy,
			ResolvedAt:  resolvedAt,
			EventID:     eventID,
		})
		return
	}
	writeResolveError(w, http.StatusConflict, "task_changed_during_resolve", "task meta kept changing during resolve; re-read and retry")
}

// evaluateResolveAttempt runs the fixed validation order (steps 2-5) against
// ONE version of the task meta:
//
//  2. no pending attention -> 409 attention_not_pending, or the
//     already-resolved sub-states: exact resolution+resolvedBy match ->
//     idempotent (returns the stored record), otherwise 409
//     attention_already_resolved
//  3. caller not in the team's human roster -> 403 not_a_human_member
//  4. decision kind: resolution not in options[].id without a freeform
//     declaration -> 400 resolution_not_an_option
//  5. oversized comment / evidence -> 400
//
// On success it returns the pending record to mutate (a live reference into
// taskData["attention"]); the caller mutates it and commits the whole meta.
func (h *ProjectHandler) evaluateResolveAttempt(ctx context.Context, team string, taskData map[string]any, resolvedBy, resolution, comment string, evidenceRaw json.RawMessage, evidence map[string]any, freeform bool) (record, idempotent map[string]any, valErr *resolveValidationError) {
	list, _ := taskData["attention"].([]any)
	pending, lastResolved := attentionResolveTargets(list)
	switch {
	case pending != nil:
		record = pending
	case lastResolved != nil:
		if str(lastResolved["resolution"]) == resolution && str(lastResolved["resolvedBy"]) == resolvedBy {
			idempotent = lastResolved
			return
		}
		valErr = resolveErr(http.StatusConflict, "attention_already_resolved", "attention already resolved with different content")
		return
	default:
		valErr = resolveErr(http.StatusConflict, "attention_not_pending", "no pending attention request on this task")
		return
	}
	if !h.isHumanRosterMember(ctx, team, resolvedBy) {
		// The roster is never revealed: no member names in the message.
		valErr = resolveErr(http.StatusForbidden, "not_a_human_member", "caller is not a human member of the team")
		return
	}
	if str(record["kind"]) == "decision" && !resolutionMatchesOption(record, resolution) && !freeform {
		valErr = resolveErr(http.StatusBadRequest, "resolution_not_an_option", "resolution must be one of the option ids (or declare evidence.freeform)")
		return
	}
	if utf8.RuneCountInString(comment) > attentionResolveMaxCommentRunes {
		valErr = resolveErr(http.StatusBadRequest, "comment_too_long", fmt.Sprintf("comment must be at most %d characters", attentionResolveMaxCommentRunes))
		return
	}
	if len(evidenceRaw) > attentionResolveMaxEvidenceBytes {
		valErr = resolveErr(http.StatusBadRequest, "evidence_too_large", fmt.Sprintf("evidence must be at most %d bytes", attentionResolveMaxEvidenceBytes))
		return
	}
	return
}

// attentionResolveTargets picks the records the resolve flow acts on: the
// MOST RECENT unresolved entry (the pending target — Python taskflow parity,
// reversed-list scan) and, when nothing is pending, the most recent resolved
// entry (the idempotency / already-resolved comparison target).
func attentionResolveTargets(list []any) (pending, lastResolved map[string]any) {
	for i := len(list) - 1; i >= 0; i-- {
		entry, ok := list[i].(map[string]any)
		if !ok {
			continue
		}
		if entry["resolved"] == true {
			if lastResolved == nil {
				lastResolved = entry
			}
			continue
		}
		if pending == nil {
			pending = entry
		}
	}
	return
}

// resolutionMatchesOption reports whether resolution is one of the record's
// options[].id values.
func resolutionMatchesOption(record map[string]any, resolution string) bool {
	options, ok := record["options"].([]any)
	if !ok {
		return false
	}
	for _, entry := range options {
		option, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if str(option["id"]) == resolution {
			return true
		}
	}
	return false
}

// isHumanRosterMember reports whether username is named in the team's human
// roster (Team.spec.humanMembers — the Human CR names the dashboard
// authenticates against, the same source the human scope sync matches on).
// A missing or unreadable roster denies: the resolve surface is human-only
// by design.
func (h *ProjectHandler) isHumanRosterMember(ctx context.Context, team, username string) bool {
	if username == "" {
		return false
	}
	var teams v1beta1.TeamList
	if err := h.client.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		return false
	}
	for i := range teams.Items {
		if teams.Items[i].Spec.EffectiveTeamName(teams.Items[i].Name) != team {
			continue
		}
		for _, member := range teams.Items[i].Spec.HumanMembers {
			if member.Name == username {
				return true
			}
		}
		return false
	}
	return false
}

// attentionResolveRef is the stable event ref of one committed resolve:
// task|attempt|resolution|resolvedAt — the identity of the logical operation
// result (never a raw request wall clock).
func attentionResolveRef(taskID string, attempt int, resolution, resolvedAt string) string {
	return fmt.Sprintf("%s|%d|%s|%s", taskID, attempt, resolution, resolvedAt)
}

// attentionResolveAnswer renders the room line's answer text: the option's
// label when the resolution is an option id, the free text otherwise.
func attentionResolveAnswer(record map[string]any, resolution string) string {
	if options, ok := record["options"].([]any); ok {
		for _, entry := range options {
			option, ok := entry.(map[string]any)
			if !ok || str(option["id"]) != resolution {
				continue
			}
			if label := str(option["label"]); label != "" {
				return label
			}
		}
	}
	return resolution
}

// resolveAttentionNotify posts the ✅ line to the task room (best-effort, the
// same policy as interventionNotify): the meta write already committed, so a
// delivery failure is logged and swallowed, never rolled back.
func (h *ProjectHandler) resolveAttentionNotify(ctx context.Context, roomID, answer, resolvedBy string) {
	if h.matrix == nil {
		return
	}
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return
	}
	msg := fmt.Sprintf("✅ 已答复：%s（@%s，via dashboard）", answer, resolvedBy)
	if err := h.matrix.SendMessageAsAdmin(ctx, roomID, msg); err != nil {
		log.FromContext(ctx).Error(err, "attention resolve notification failed", "roomID", roomID)
	}
}

// parseTaskMetaVersioned decodes one task meta read and returns it with the
// ETag of the exact bytes read (contentETag — the version the conditional
// write binds to).
func parseTaskMetaVersioned(data []byte) (map[string]any, string, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", err
	}
	return raw, contentETag(data), nil
}

// truncateRunes caps s at n runes (parity with the Python writer's
// resolvedBy[:200]).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// canonicalAccount (identity spelling: MXID localpart, casefolded) lives in
// task_meta_commit.go — the conditional task-meta commit path and this resolve
// write path share the one definition.
