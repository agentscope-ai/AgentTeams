package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	var values [2]any
	for i, raw := range []json.RawMessage{a, b} {
		if !json.Valid(raw) {
			return false
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		// Keep large integers distinct when comparing authority fields.
		decoder.UseNumber()
		if err := decoder.Decode(&values[i]); err != nil {
			return false
		}
	}
	return reflect.DeepEqual(values[0], values[1])
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
// version the caller read (#1471). See the file header for why.
func (h *ProjectHandler) PutTaskMeta(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(r.PathValue("id"))
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if ifMatch == "" {
		writeError(w, http.StatusPreconditionRequired,
			"If-Match is required: read the task meta and pass its ETag; this endpoint never blind-writes")
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

	key, team, ok := h.resolveTaskMetaCommitKey(w, r, taskID)
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

	// W2 decision-authority guard (#1467 P1b). The ETag/CAS above protects
	// against concurrent versions but not against a same-team worker forging
	// a decision it is not entitled to make: with a real worker bearer it
	// could otherwise resolve an approval record, flip the decision fields,
	// or complete/reopen the task. This is the server-side half of the W2
	// gate (the runtime keeps its own Python check; the controller must not
	// trust the client). It runs after the cancellation guard (cancellation
	// stays terminal and server-owned regardless of the caller) and before
	// the strict write, so a forbidden commit is rejected before any write.
	if !h.validateW2DecisionAuthority(w, r.Context(), current, body,
		authpkg.CallerFromContext(r.Context()), team) {
		return
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

// --- W2 decision-authority guard (#1467 P1b) --------------------------------
//
// The commit endpoint is the single write path for task meta. The ETag/CAS
// protects against concurrent versions and the cancellation guard above keeps
// a cancelled task terminal and server-owned. What neither covers is whether
// the caller is entitled to make the decision at all: a same-team worker that
// holds a real bearer could otherwise mark an approval record resolved,
// rewrite the decision fields / history, or complete or reopen the task — and
// a plain leader accept_task_result would then pass with no human approval.
// The runtime keeps its own (client-side) W2 gate; this block is the
// server-side half. It fails closed: anything it cannot read or verify is
// rejected rather than allowed, and it never trusts the committed body.
//
// The rules come from taskbook-1467-p1b. A worker reports but never decides;
// only a decider (team leader / human / manager / admin) decides, and a close
// must name a real, non-assignee human decider.

// approvalAuthorityFields are the seven fields that make an approval record
// authoritative. They are server-owned: a worker's commit must keep them
// semantically unchanged, and a decider may change them only through a
// provenance-legal close.
var approvalAuthorityFields = []string{
	"resolved",
	"resolvedRole",
	"resolvedVia",
	"resolvedActor",
	"resolvedAt",
	"resolvedBy",
	"resolveEvidence",
}

// humanResolveChannels are the only channels through which a human approval
// may be closed (mirrors the runtime's _HUMAN_RESOLVE_CHANNELS).
var humanResolveChannels = map[string]struct{}{
	"im_reply":   {},
	"im_command": {},
	"dashboard":  {},
}

// decisionClassIDs is the fixed decision_class vocabulary (mirrors the
// runtime's DECISION_CLASS_IDS).
var decisionClassIDs = map[string]struct{}{
	"r1": {}, "r2": {}, "r3": {}, "r4": {}, "r5": {}, "r6": {},
}

// taskStatusTransitions is the Go projection of the single source of truth
// plugins/agentteams-teamharness/contracts/task-transitions.json (it mirrors
// the runtime's TRANSITIONS). The parity test asserts the two agree. Terminal
// states carry an empty target list so a transition out of them is never
// legal.
var taskStatusTransitions = map[string][]string{
	"planned":     {"prepared", "assigned", "in_progress", "submitted", "cancelled"},
	"prepared":    {"assigned", "cancelled"},
	"assigned":    {"in_progress", "submitted", "cancelled"},
	"in_progress": {"submitted", "cancelled"},
	"submitted":   {"completed", "revision", "blocked", "cancelled"},
	"completed":   {},
	"revision":    {},
	"blocked":     {},
	"cancelled":   {},
}

// terminalTaskStatuses are the states no transition leaves (mirrors the
// contract's "terminal" list and the runtime's TERMINAL_TASK_STATUSES).
var terminalTaskStatuses = map[string]struct{}{
	"completed": {},
	"revision":  {},
	"blocked":   {},
	"cancelled": {},
}

// taskStatusIsTerminal reports whether status is a terminal task state.
func taskStatusIsTerminal(status string) bool {
	_, ok := terminalTaskStatuses[status]
	return ok
}

// legalStatusTransition reports whether from -> to is an edge in the contract
// transition table. A missing from-state, or a target not in its list (terminal
// states have an empty list), is not legal.
func legalStatusTransition(from, to string) bool {
	for _, target := range taskStatusTransitions[from] {
		if target == to {
			return true
		}
	}
	return false
}

// workerStatusLane is the narrow set of status moves a worker may make:
// planned/prepared/assigned -> in_progress (start work) and in_progress ->
// submitted (hand off). Everything else — completing, revising, blocking,
// cancelling, reopening — is decision-authority owned. (Same-value writes are
// handled by the caller as idempotent, so from != to here.)
func workerStatusLane(from, to string) bool {
	switch to {
	case "in_progress":
		return from == "planned" || from == "prepared" || from == "assigned"
	case "submitted":
		return from == "in_progress"
	default:
		return false
	}
}

// commitActorClass is the coarse W2 authority class a commit caller belongs to.
type commitActorClass int

const (
	actorWorker commitActorClass = iota
	actorDecider
)

// classifyCommitActor maps a caller onto its W2 authority class. Anything that
// is not an explicit decider role — including a nil caller — is treated as a
// worker (the most restrictive class), so a missing or unknown identity can
// never unlock decision authority.
func classifyCommitActor(caller *authpkg.CallerIdentity) commitActorClass {
	if caller == nil {
		return actorWorker
	}
	switch caller.Role {
	case authpkg.RoleTeamLeader, authpkg.RoleHuman, authpkg.RoleManager, authpkg.RoleAdmin:
		return actorDecider
	default:
		return actorWorker
	}
}

// canonicalAccount canonicalizes a Matrix account the way the runtime does
// (_canonical_account): trim, and when the value is a full Matrix ID (starts
// with "@" and contains ":"), keep only the localpart; then lowercase. Non-Matrix
// spellings are lowercased as-is, so "luo" and "@luo:example.test" compare equal.
func canonicalAccount(value string) string {
	text := strings.TrimSpace(value)
	if strings.HasPrefix(text, "@") && strings.Contains(text, ":") {
		text = text[1:]
		if idx := strings.Index(text, ":"); idx >= 0 {
			text = text[:idx]
		}
	}
	return strings.ToLower(text)
}

// parseActor splits a "<role>:<account>" decider identity the way the runtime
// does (_decider_identity): partition on the FIRST colon. A value with no colon,
// or with an empty account, is not a decider identity (account returned "").
func parseActor(decider string) (role, account string) {
	idx := strings.Index(decider, ":")
	if idx < 0 {
		return decider, ""
	}
	account = decider[idx+1:]
	if account == "" {
		return decider, ""
	}
	return decider[:idx], account
}

// assigneeAccounts returns the set of canonical accounts the stored task is
// assigned to, from the three spellings the runtime uses. Used to forbid
// self-approval (a decider closing an approval it is assigned to).
func assigneeAccounts(cur map[string]json.RawMessage) map[string]struct{} {
	set := make(map[string]struct{})
	for _, key := range []string{"assigned_to", "assigned_to_name", "assignee"} {
		raw, ok := cur[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			continue
		}
		set[canonicalAccount(s)] = struct{}{}
	}
	return set
}

// recordIdentity derives a stable identity for an attention record so a body
// record can be matched to the current record it updates. The runtime keys a
// record by (kind, attempt) and never renumbers it, so that pair is stable
// across a legitimate close. A record lacking a non-empty kind or an attempt is
// unstable and must be matched by full semantic equality instead.
func recordIdentity(rec map[string]json.RawMessage) (id string, stable bool) {
	kindRaw, kindOk := rec["kind"]
	attemptRaw, attemptOk := rec["attempt"]
	if !kindOk || !attemptOk {
		return "", false
	}
	var kind string
	if json.Unmarshal(kindRaw, &kind) != nil || kind == "" {
		return "", false
	}
	compact, err := compactJSON(attemptRaw)
	if err != nil {
		return "", false
	}
	return kind + "\x00" + string(compact), true
}

// attentionRecords parses the top-level "attention" of a task meta. Absent
// attention is fine (nil, ok). A present but malformed value (not an array of
// objects) is an error the caller must fail closed on.
func attentionRecords(meta map[string]json.RawMessage) ([]map[string]json.RawMessage, bool) {
	raw, ok := meta["attention"]
	if !ok {
		return nil, true
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false
	}
	return arr, true
}

// findSemanticMatch returns the first candidate semantically equal to target
// (or nil). Both are maps, and Go marshals map keys in sorted order, so a byte
// comparison of the marshaled forms is a valid semantic equality.
func findSemanticMatch(candidates []map[string]json.RawMessage, target map[string]json.RawMessage) map[string]json.RawMessage {
	var targetBytes []byte
	if b, err := json.Marshal(target); err == nil {
		targetBytes = b
	}
	for _, cand := range candidates {
		if b, err := json.Marshal(cand); err == nil && bytes.Equal(b, targetBytes) {
			return cand
		}
	}
	return nil
}

// authorityFieldsEqual reports whether two approval records agree on all seven
// authoritative fields, distinguishing an absent field from an explicit null
// (absent is not the same as null, as the runtime requires).
func authorityFieldsEqual(a, b map[string]json.RawMessage) bool {
	for _, field := range approvalAuthorityFields {
		aRaw, aHas := a[field]
		bRaw, bHas := b[field]
		if aHas != bHas {
			return false
		}
		if aHas && !sameJSONValue(aRaw, bRaw) {
			return false
		}
	}
	return true
}

// jsonBoolTrue reports whether raw is the JSON value true.
func jsonBoolTrue(raw json.RawMessage) bool {
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

// recordStringField returns a record field decoded as a string (ok false when
// absent or not a JSON string).
func recordStringField(rec map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := rec[key]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// approvalRecordIsOpen reports whether an approval record is raised open:
// resolved absent or exactly false, and no resolved* provenance field present.
// A newly added approval record must be raised open by any role — nobody may
// commit a record that is already closed.
func approvalRecordIsOpen(rec map[string]json.RawMessage) bool {
	if raw, ok := rec["resolved"]; ok {
		if string(bytes.TrimSpace(raw)) != "false" {
			return false
		}
	}
	for _, field := range []string{
		"resolvedRole", "resolvedVia", "resolvedActor",
		"resolvedAt", "resolvedBy", "resolveEvidence",
	} {
		if _, ok := rec[field]; ok {
			return false
		}
	}
	return true
}

// humanOwnAccounts returns the set of canonical accounts a human caller may
// commit on its own behalf: its Username and, when a Human CR exists, the
// localpart of its status.matrixUserID. A missing CR is not an error (the human
// may be known by Username alone); a transport error is (fail closed).
func (h *ProjectHandler) humanOwnAccounts(
	w http.ResponseWriter, ctx context.Context, caller *authpkg.CallerIdentity,
) (map[string]struct{}, bool) {
	own := map[string]struct{}{}
	if caller.Username != "" {
		own[canonicalAccount(caller.Username)] = struct{}{}
	}
	var human v1beta1.Human
	err := h.client.Get(ctx, client.ObjectKey{
		Namespace: h.namespace,
		Name:      caller.Username,
	}, &human)
	if apierrors.IsNotFound(err) {
		return own, true
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"approval provenance: failed to read the human CR: "+err.Error())
		return nil, false
	}
	if human.Status.MatrixUserID != "" {
		own[canonicalAccount(human.Status.MatrixUserID)] = struct{}{}
	}
	return own, true
}

// teamRosterAccounts returns the set of canonical accounts on the team's roster
// (HumanMembers and Admin, each by Name and MatrixUserID). A transport error is
// a 500; a roster that does not exist for the task's team is a 403 fail-closed
// (the decider cannot be verified, so the close is refused).
func (h *ProjectHandler) teamRosterAccounts(
	w http.ResponseWriter, ctx context.Context, team string,
) (map[string]struct{}, bool) {
	var teams v1beta1.TeamList
	if err := h.client.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		writeError(w, http.StatusInternalServerError,
			"approval provenance: failed to read the team roster: "+err.Error())
		return nil, false
	}
	roster := map[string]struct{}{}
	found := false
	for i := range teams.Items {
		item := &teams.Items[i]
		if item.Spec.EffectiveTeamName(item.Name) != team && item.Name != team {
			continue
		}
		found = true
		for _, member := range item.Spec.HumanMembers {
			for _, value := range []string{member.Name, member.MatrixUserID} {
				if value != "" {
					roster[canonicalAccount(value)] = struct{}{}
				}
			}
		}
		if item.Spec.Admin != nil {
			for _, value := range []string{item.Spec.Admin.Name, item.Spec.Admin.MatrixUserID} {
				if value != "" {
					roster[canonicalAccount(value)] = struct{}{}
				}
			}
		}
	}
	if !found {
		writeError(w, http.StatusForbidden,
			"approval provenance: no team roster found for the task's team")
		return nil, false
	}
	return roster, true
}

// rejectSelfApproval enforces the self-approval ban (taskbook rule 5): the
// decider account a close names may not be the task's assignee.
func rejectSelfApproval(w http.ResponseWriter, account string, assignees map[string]struct{}) bool {
	if _, isAssignee := assignees[canonicalAccount(account)]; isAssignee {
		writeError(w, http.StatusForbidden,
			"approval provenance: self-approval is forbidden: the decider account is the task's assignee")
		return false
	}
	return true
}

// authorizeApprovalClose enforces the decider-identity rules (taskbook rules 4
// and 5) on the account a close names. It returns false (after writing the
// error) when the caller may not commit a close on behalf of that account.
func (h *ProjectHandler) authorizeApprovalClose(
	w http.ResponseWriter,
	ctx context.Context,
	caller *authpkg.CallerIdentity,
	team string,
	account string,
	assignees map[string]struct{},
) bool {
	// Rule 4: a human may only commit an approval it names itself.
	if caller.Role == authpkg.RoleHuman {
		own, ok := h.humanOwnAccounts(w, ctx, caller)
		if !ok {
			return false
		}
		if _, isOwn := own[canonicalAccount(account)]; !isOwn {
			writeError(w, http.StatusForbidden,
				"human approval must be committed by the human it names: "+account)
			return false
		}
		return rejectSelfApproval(w, account, assignees)
	}
	switch caller.Role {
	case authpkg.RoleTeamLeader:
		if team == "" {
			writeError(w, http.StatusForbidden,
				"approval provenance: standalone tasks may only be closed by a manager or admin")
			return false
		}
		roster, ok := h.teamRosterAccounts(w, ctx, team)
		if !ok {
			return false
		}
		if _, onRoster := roster[canonicalAccount(account)]; !onRoster {
			writeError(w, http.StatusForbidden,
				"approval provenance: the decider account is not on the team roster")
			return false
		}
		return rejectSelfApproval(w, account, assignees)
	case authpkg.RoleManager, authpkg.RoleAdmin:
		// A manager/admin closes any (team or standalone) task's approval;
		// the roster check is waived, the self-approval check still applies.
		return rejectSelfApproval(w, account, assignees)
	default:
		// Any other role reaching here is not a decider; fail closed.
		writeError(w, http.StatusForbidden,
			"approval provenance: the caller may not resolve an approval record")
		return false
	}
}

// validateApprovalClose enforces the close-legality rules (taskbook rules 1-3)
// on an approval record being closed, then delegates the caller-identity rules
// (4-5) to authorizeApprovalClose. When the commit closes a record that was
// still open it increments *newlyClosed, which the decision_status guard uses
// to permit an unfreeze.
func (h *ProjectHandler) validateApprovalClose(
	w http.ResponseWriter,
	ctx context.Context,
	bRec, cRec map[string]json.RawMessage,
	caller *authpkg.CallerIdentity,
	team string,
	assignees map[string]struct{},
	newlyClosed *int,
) bool {
	// Rule 1: resolvedRole must be exactly "human".
	role, ok := recordStringField(bRec, "resolvedRole")
	if !ok || role != "human" {
		writeError(w, http.StatusForbidden,
			`approval provenance: resolvedRole must be "human"`)
		return false
	}
	// Rule 2: resolvedVia must be a sanctioned human channel.
	via, ok := recordStringField(bRec, "resolvedVia")
	if !ok {
		writeError(w, http.StatusForbidden,
			"approval provenance: resolvedVia is required to close an approval")
		return false
	}
	if _, ok := humanResolveChannels[via]; !ok {
		writeError(w, http.StatusForbidden,
			"approval provenance: resolvedVia must be im_reply, im_command or dashboard")
		return false
	}
	// Rule 3: resolvedActor must be "<role>:<account>" with a non-empty account.
	actor, ok := recordStringField(bRec, "resolvedActor")
	if !ok {
		writeError(w, http.StatusForbidden,
			"approval provenance: resolvedActor is required to close an approval")
		return false
	}
	_, account := parseActor(actor)
	if account == "" {
		writeError(w, http.StatusForbidden,
			"approval provenance: resolvedActor must be <role>:<account> with a non-empty account")
		return false
	}
	if !h.authorizeApprovalClose(w, ctx, caller, team, account, assignees) {
		return false
	}
	// This commit closes a record that was still open.
	if jsonBoolTrue(bRec["resolved"]) && !jsonBoolTrue(cRec["resolved"]) {
		*newlyClosed++
	}
	return true
}

// checkAttention enforces the attention-record rules for the commit's caller
// class: no record may be deleted (any role), a worker may not change the
// authoritative fields of an existing approval record, a decider may only close
// one through a provenance-legal close, and any newly added approval record
// must be raised open. It counts new closes into *newlyClosed.
//
// Invariant: a duplicate stable identity is refused on the stored side and on
// the proposed side alike, so the stored and proposed records correspond
// one-to-one by identity and the loops below check every record — a matched
// one through the authority-field / close rules, a new one through the open
// rule — with no proposed record ever skipped unchecked.
func (h *ProjectHandler) checkAttention(
	w http.ResponseWriter,
	ctx context.Context,
	cur, bod map[string]json.RawMessage,
	class commitActorClass,
	caller *authpkg.CallerIdentity,
	team string,
	assignees map[string]struct{},
	newlyClosed *int,
) bool {
	curRecs, curOk := attentionRecords(cur)
	bodRecs, bodOk := attentionRecords(bod)
	if !curOk || !bodOk {
		writeError(w, http.StatusForbidden,
			"worker may not change the W2 decision fields: malformed attention")
		return false
	}
	bodByID := make(map[string]map[string]json.RawMessage, len(bodRecs))
	curIDs := make(map[string]struct{}, len(curRecs))
	for _, cRec := range curRecs {
		if id, stable := recordIdentity(cRec); stable {
			if _, seen := curIDs[id]; seen {
				writeError(w, http.StatusForbidden,
					"stored attention records contain an ambiguous duplicate identity (kind, attempt)")
				return false
			}
			curIDs[id] = struct{}{}
		}
	}
	for _, bRec := range bodRecs {
		if id, stable := recordIdentity(bRec); stable {
			if _, seen := bodByID[id]; seen {
				writeError(w, http.StatusForbidden,
					"proposed attention records contain an ambiguous duplicate identity (kind, attempt)")
				return false
			}
			bodByID[id] = bRec
		}
	}
	// Every current record must still be present in the body (nothing is
	// deleted). Match by stable identity, or by full semantic equality when the
	// record has no stable identity.
	for _, cRec := range curRecs {
		id, stable := recordIdentity(cRec)
		var bRec map[string]json.RawMessage
		if stable {
			bRec = bodByID[id]
		} else {
			bRec = findSemanticMatch(bodRecs, cRec)
		}
		if bRec == nil {
			writeError(w, http.StatusForbidden,
				"worker may not change the W2 decision fields: an existing attention record was deleted")
			return false
		}
		kind, _ := recordStringField(cRec, "kind")
		if kind == "approval" && !authorityFieldsEqual(cRec, bRec) {
			if class == actorWorker {
				writeError(w, http.StatusForbidden,
					"worker may not resolve an approval record: the authoritative fields of an existing approval record are server-owned")
				return false
			}
			// A decider changed the authoritative fields: it must be a legal
			// close, and nothing else.
			if !h.validateApprovalClose(w, ctx, bRec, cRec, caller, team, assignees, newlyClosed) {
				return false
			}
		}
	}
	// Every body record not matching an existing one is newly added. A new
	// approval record must be raised open, by any role; other kinds are
	// ordinary (worker) reporting and pass.
	for _, bRec := range bodRecs {
		var isNew bool
		if id, stable := recordIdentity(bRec); stable {
			_, known := curIDs[id]
			isNew = !known
		} else {
			isNew = findSemanticMatch(curRecs, bRec) == nil
		}
		if !isNew {
			continue
		}
		kind, _ := recordStringField(bRec, "kind")
		if kind == "approval" && !approvalRecordIsOpen(bRec) {
			if class == actorWorker {
				writeError(w, http.StatusForbidden,
					"worker may not resolve an approval record: a new approval record must be raised open")
			} else {
				writeError(w, http.StatusForbidden,
					"approval provenance: a new approval record must be raised open")
			}
			return false
		}
	}
	return true
}

// decisionClassSpellings are the two spellings the runtimes read for the
// decision class: the canonical decision_class and the camelCase alias. The
// W2 acceptance guard (Python _decision_class_of,
// plugins/agentteams-teamharness/mcp/lifecycle/review.py) resolves the
// effective value canonical-first from either spelling, so the commit guard
// must treat the stored bytes of BOTH as protected: a worker that rewrites
// only the alias would otherwise change the effective approval requirement
// without tripping a spelling-aware check.
var decisionClassSpellings = []string{"decision_class", "decisionClass"}

// checkDecisionClass enforces, per spelling, that a worker keeps the decision
// class server-owned (the key's presence and value may not change), and that
// a leader/human does not introduce an out-of-vocabulary value under any
// spelling (manager/admin are not content-validated, mirroring the runtime).
func (h *ProjectHandler) checkDecisionClass(
	w http.ResponseWriter,
	cur, bod map[string]json.RawMessage,
	class commitActorClass,
	caller *authpkg.CallerIdentity,
) bool {
	if class == actorWorker {
		for _, key := range decisionClassSpellings {
			curRaw, curHas := cur[key]
			bodRaw, bodHas := bod[key]
			if curHas != bodHas || (curHas && !sameJSONValue(curRaw, bodRaw)) {
				writeError(w, http.StatusForbidden,
					"worker may not change the W2 decision fields: decision_class / decisionClass is decision-authority owned")
				return false
			}
		}
		return true
	}
	if caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman {
		for _, key := range decisionClassSpellings {
			bodRaw, bodHas := bod[key]
			if !bodHas {
				continue
			}
			var s string
			if json.Unmarshal(bodRaw, &s) == nil {
				if _, ok := decisionClassIDs[s]; !ok {
					writeError(w, http.StatusForbidden,
						key+" must be one of r1, r2, r3, r4, r5 or r6")
					return false
				}
			}
		}
	}
	return true
}

// checkReview enforces that a worker keeps review semantically unchanged. The
// runtime does not validate review's content, so the controller mirrors that:
// no content check for any role, only worker-ownership.
func (h *ProjectHandler) checkReview(
	w http.ResponseWriter,
	cur, bod map[string]json.RawMessage,
	class commitActorClass,
) bool {
	if class != actorWorker {
		return true
	}
	curRaw, curHas := cur["review"]
	bodRaw, bodHas := bod["review"]
	if curHas != bodHas || (curHas && !sameJSONValue(curRaw, bodRaw)) {
		writeError(w, http.StatusForbidden,
			"worker may not change the W2 decision fields: review is decision-authority owned")
		return false
	}
	return true
}

// checkDecisionStatus enforces the decision_status rules: a worker may not
// leave a frozen decision_status nor return a blocked/frozen one to open, and a
// decider may only unfreeze a frozen decision in a commit that also closes at
// least one approval record.
func (h *ProjectHandler) checkDecisionStatus(
	w http.ResponseWriter,
	cur, bod map[string]json.RawMessage,
	class commitActorClass,
	newlyClosed int,
) bool {
	var curDS, bodDS string
	if raw, ok := cur["decision_status"]; ok {
		_ = json.Unmarshal(raw, &curDS)
	}
	if raw, ok := bod["decision_status"]; ok {
		_ = json.Unmarshal(raw, &bodDS)
	}
	if class == actorWorker {
		// Sticky frozen: a frozen decision_status may not leave frozen.
		if curDS == "frozen" && bodDS != "frozen" {
			writeError(w, http.StatusForbidden,
				"worker may not change the W2 decision fields: a frozen decision_status stays frozen")
			return false
		}
		// No reopen: a blocked or frozen decision_status may not return to open.
		if (curDS == "blocked" || curDS == "frozen") && bodDS == "open" {
			writeError(w, http.StatusForbidden,
				"worker may not change the W2 decision fields: a blocked/frozen decision_status cannot return to open")
			return false
		}
		return true
	}
	// Decider: unfreezing requires a newly closed approval in this commit. An
	// omitted decision_status on a frozen task counts as leaving frozen (fail
	// closed).
	if curDS == "frozen" && bodDS != "frozen" && newlyClosed < 1 {
		writeError(w, http.StatusForbidden,
			"unfreezing a frozen task requires a newly resolved approval record")
		return false
	}
	return true
}

// checkDecisionHistory enforces append-only decision_status_history for every
// caller except manager/admin (who may rewrite it as operators). The body's
// history must keep the current history verbatim as a prefix.
func (h *ProjectHandler) checkDecisionHistory(
	w http.ResponseWriter,
	cur, bod map[string]json.RawMessage,
	caller *authpkg.CallerIdentity,
) bool {
	if caller != nil && (caller.Role == authpkg.RoleManager || caller.Role == authpkg.RoleAdmin) {
		return true
	}
	var curArr, bodArr []json.RawMessage
	if raw, ok := cur["decision_status_history"]; ok {
		_ = json.Unmarshal(raw, &curArr)
	}
	if raw, ok := bod["decision_status_history"]; ok {
		if err := json.Unmarshal(raw, &bodArr); err != nil {
			writeError(w, http.StatusForbidden,
				"worker may not rewrite decision_status_history: history is append-only")
			return false
		}
	}
	if len(bodArr) < len(curArr) {
		writeError(w, http.StatusForbidden,
			"worker may not rewrite decision_status_history: history is append-only")
		return false
	}
	for i := 0; i < len(curArr); i++ {
		if !sameJSONValue(curArr[i], bodArr[i]) {
			writeError(w, http.StatusForbidden,
				"worker may not rewrite decision_status_history: existing entries must be kept verbatim")
			return false
		}
	}
	return true
}

// checkStatusLane enforces status-transition authority. A worker may only make
// its narrow lane (start / hand off); a decider may make any transition the
// lifecycle contract allows. A same-value status is idempotent and allowed for
// both, and a commit that omits status is already rejected upstream.
func (h *ProjectHandler) checkStatusLane(
	w http.ResponseWriter,
	cur, bod map[string]json.RawMessage,
	class commitActorClass,
) bool {
	var curStatus, bodStatus string
	bodHas := false
	if raw, ok := bod["status"]; ok {
		bodHas = true
		_ = json.Unmarshal(raw, &bodStatus)
	}
	if raw, ok := cur["status"]; ok {
		_ = json.Unmarshal(raw, &curStatus)
	}
	if !bodHas {
		// Upstream already rejects a commit that drops a present status.
		return true
	}
	if bodStatus == curStatus {
		// Idempotent same-value write (including staying terminal).
		return true
	}
	if class == actorWorker {
		if !workerStatusLane(curStatus, bodStatus) {
			writeError(w, http.StatusForbidden,
				"worker may not complete or reopen a task: only planned/prepared/assigned -> in_progress and in_progress -> submitted are worker lanes")
			return false
		}
		return true
	}
	if !legalStatusTransition(curStatus, bodStatus) {
		writeError(w, http.StatusForbidden,
			"the requested status transition is not allowed by the task lifecycle contract")
		return false
	}
	return true
}

// validateW2DecisionAuthority is the server-side W2 decision-authority guard
// for a task meta commit. It runs after the cancellation guard and before the
// strict write, and fails closed: any field it cannot read or verify rejects
// the commit rather than allowing it.
func (h *ProjectHandler) validateW2DecisionAuthority(
	w http.ResponseWriter,
	ctx context.Context,
	current, body []byte,
	caller *authpkg.CallerIdentity,
	team string,
) bool {
	var cur map[string]json.RawMessage
	if err := json.Unmarshal(current, &cur); err != nil || cur == nil {
		writeError(w, http.StatusForbidden,
			"worker may not change the W2 decision fields: the stored task meta is not a readable object")
		return false
	}
	var bod map[string]json.RawMessage
	if err := json.Unmarshal(body, &bod); err != nil || bod == nil {
		writeError(w, http.StatusBadRequest,
			"task meta commit body must be a JSON object")
		return false
	}
	class := classifyCommitActor(caller)
	assignees := assigneeAccounts(cur)
	newlyClosed := 0
	if !h.checkAttention(w, ctx, cur, bod, class, caller, team, assignees, &newlyClosed) {
		return false
	}
	if !h.checkDecisionClass(w, cur, bod, class, caller) {
		return false
	}
	if !h.checkReview(w, cur, bod, class) {
		return false
	}
	if !h.checkDecisionStatus(w, cur, bod, class, newlyClosed) {
		return false
	}
	if !h.checkDecisionHistory(w, cur, bod, caller) {
		return false
	}
	if !h.checkStatusLane(w, cur, bod, class) {
		return false
	}
	return true
}
