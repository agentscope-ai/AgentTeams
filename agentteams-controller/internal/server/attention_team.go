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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Team-level attention inbox (#1394 S1).
//
// The dashboard inbox needs one read that returns every pending human
// question across a team's projects. The project-scoped endpoint
// (GetProjectAttention) answers a single project and carries neither the
// delivery outcome nor the deadline/provenance fields the inbox row
// contract requires, so this endpoint reads the same TaskMeta objects one
// level up.
//
// Read-only by contract: it never changes a task, and the only object it
// writes is the best-effort, idempotent `attention_overdue` signal event
// for an item whose deadline has already passed (the S1 overdue half — the
// row carries the boolean, the event keeps the single event stream
// complete). A failed event write never fails the read.

const (
	teamAttentionDefaultLimit = 200
	teamAttentionMaxLimit     = 1000
)

// teamAttentionNotified mirrors one TaskMeta `attention[].notification_log`
// entry in the row contract's camelCase shape (PR #1394 S4a). recipients /
// gaps are the #1495 additions; before that merge they are absent and the
// row reports empty lists.
type teamAttentionNotified struct {
	Sent            bool   `json:"sent"`
	RoomAccepted    bool   `json:"roomAccepted"`
	ChannelAccepted any    `json:"channelAccepted"`
	Recipients      []any  `json:"recipients"`
	Gaps            []any  `json:"gaps"`
	At              string `json:"at"`
}

// teamAttentionOption is one selectable answer (S0 options[], description
// optional and passed through verbatim).
type teamAttentionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// teamAttentionRow is one inbox row (the frontend consumes this shape
// verbatim — #1394 S1). Pointer fields serialize as null when absent.
type teamAttentionRow struct {
	TaskID      string                 `json:"taskId"`
	TeamID      string                 `json:"teamId"`
	ProjectID   string                 `json:"projectId"`
	Kind        string                 `json:"kind"`
	Question    string                 `json:"question"`
	Options     []teamAttentionOption  `json:"options"`
	RequestedAt string                 `json:"requestedAt"`
	TimeoutAt   string                 `json:"timeoutAt"`
	Overdue     bool                   `json:"overdue"`
	Resolved    bool                   `json:"resolved"`
	ResolvedVia *string                `json:"resolvedVia"`
	ResolvedBy  *string                `json:"resolvedBy"`
	ResolvedAt  *string                `json:"resolvedAt"`
	Resolution  *string                `json:"resolution"`
	Freeform    bool                   `json:"freeform"`
	Notified    *teamAttentionNotified `json:"notified"`
	RoomID      string                 `json:"roomId"`
	URL         string                 `json:"url"`
}

// teamAttentionProject is one project of the requested team, read in the
// scope its prefix belongs to (the task keys follow the prefix, mirroring
// projectMatch.team).
type teamAttentionProject struct {
	meta *projectMeta
	team string
}

// GetTeamAttention handles GET /api/v1/teams/{name}/attention.
//
// Query: status = open (default) | overdue | resolved | all, limit =
// 1..1000 (default 200; anything above the ceiling is a 400, not a clamp).
//
// Auth: same shape as GetTeam — the caller must be allowed to read the
// team, and an unknown or out-of-scope team is a 404 (never a 403 that
// would confirm existence). The response is the team's live rows, sorted
// by urgency: `overdue` by deadline ascending, every other filter by
// requestedAt descending.
//
// Partial results: when the filter matches more rows than `limit`, the
// response carries the first `limit` rows with `"truncated": true` in the
// body and the `X-Attention-Truncated: 1` header (absent otherwise).
// `X-Attention-Skipped` (present only when > 0) counts the projects
// skipped because a project meta or one of its task metas could not be
// read or parsed — it never includes truncated rows, so the caller can
// tell a limited result, a degraded result, and a genuinely empty inbox
// apart. `limit` bounds returned rows only: every project and task meta
// of the team is read before the cap is applied (the read cost of a
// truncated result is a full-team read — see the "Current read cost"
// section of docs/usage/project-workflow-api.md).
func (h *ProjectHandler) GetTeamAttention(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "team name is required")
		return
	}

	var tm v1beta1.Team
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &tm); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "team not found")
			return
		}
		writeK8sError(w, "get team attention", err)
		return
	}
	effective := tm.Spec.EffectiveTeamName(tm.Name)
	caller := authpkg.CallerFromContext(r.Context())
	if err := h.checkProjectAccess(caller, effective, map[string]string{tm.Name: effective}); err != nil {
		// Same policy as GetTeam: hide an out-of-scope team behind a 404 so
		// a scoped caller cannot probe team existence.
		httputil.WriteError(w, http.StatusNotFound, "team not found")
		return
	}

	status := strings.TrimSpace(r.URL.Query().Get("status"))
	switch status {
	case "":
		status = "open"
	case "open", "overdue", "resolved", "all":
	default:
		httputil.WriteError(w, http.StatusBadRequest, "status must be one of open, overdue, resolved, all")
		return
	}
	limit := teamAttentionDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httputil.WriteError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > teamAttentionMaxLimit {
			httputil.WriteError(w, http.StatusBadRequest, "limit must be at most 1000")
			return
		}
		limit = n
	}

	prefixes := teamAttentionPrefixes(effective, tm.Name)
	projects, skipped, err := h.readTeamAttentionProjects(r.Context(), prefixes)
	if err != nil {
		writeK8sError(w, "get team attention: list projects", err)
		return
	}

	now := time.Now().UTC()
	rows := make([]teamAttentionRow, 0)
	for _, proj := range projects {
		projectRows, projectSkipped := h.readProjectAttentionRows(r.Context(), proj, now)
		rows = append(rows, projectRows...)
		if projectSkipped {
			skipped++
		}
	}

	filtered := filterTeamAttentionRows(rows, status, now)
	sortTeamAttentionRows(filtered, status)
	// `limit` is applied last, after every object of the team has been
	// read: it caps returned rows, never the object-store work (the read
	// fan-out is pinned by BenchmarkGetTeamAttention).
	truncated := len(filtered) > limit
	if truncated {
		filtered = filtered[:limit]
	}

	if skipped > 0 {
		w.Header().Set("X-Attention-Skipped", strconv.Itoa(skipped))
	}
	if truncated {
		w.Header().Set("X-Attention-Truncated", "1")
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"team_id":   name,
		"items":     filtered,
		"count":     len(filtered),
		"truncated": truncated,
	})
}

// teamAttentionPrefixes lists the project prefixes of one team, deduped and
// in a deterministic order. Both the CR name and the effective storage name
// are enumerated (they differ when spec.teamName overrides the CR name and
// the workers resolve either spelling). No global prefix: the team endpoint
// must never surface standalone projects.
func teamAttentionPrefixes(effective, crName string) []string {
	out := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, name := range []string{effective, crName} {
		if name == "" {
			continue
		}
		prefix := "teams/" + name + "/shared/projects/"
		if !seen[prefix] {
			seen[prefix] = true
			out = append(out, prefix)
		}
	}
	return out
}

// readTeamAttentionProjects reads every project meta of the team. The list
// calls are infrastructure-level: a failure aborts the read (500) instead
// of returning a silently empty inbox. A per-project read/parse failure is
// skipped and counted (the inbox must not go blank because one project's
// object is broken) — same policy as ListProjects' W5.
func (h *ProjectHandler) readTeamAttentionProjects(ctx context.Context, prefixes []string) ([]teamAttentionProject, int, error) {
	type candidate struct {
		prefix string
		key    string
	}
	var cands []candidate
	for _, prefix := range prefixes {
		children, err := h.oss.ListObjects(ctx, prefix)
		if err != nil {
			return nil, 0, err
		}
		for _, child := range children {
			key, ok := metaKeyFromListResult(prefix, child)
			if !ok {
				continue
			}
			cands = append(cands, candidate{prefix: prefix, key: key})
		}
	}

	const concurrency = 8
	type result struct {
		candidate
		data []byte
		err  error
	}
	results := make([]result, len(cands))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c candidate) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := h.oss.GetObject(ctx, c.key)
			results[i] = result{candidate: c, data: data, err: err}
		}(i, c)
	}
	wg.Wait()

	projects := make([]teamAttentionProject, 0, len(results))
	skipped := 0
	seen := map[string]bool{}
	for _, res := range results {
		if res.err != nil {
			if !errors.Is(res.err, os.ErrNotExist) {
				skipped++ // unreadable project object: skip and report
			}
			continue
		}
		var meta projectMeta
		if err := json.Unmarshal(res.data, &meta); err != nil {
			skipped++ // malformed meta: skip and report
			continue
		}
		if meta.ProjectID == "" {
			skipped++
			continue
		}
		team := teamFromPrefix(res.prefix)
		if meta.TeamID == "" {
			meta.TeamID = team
		}
		// Project ids are only unique per workspace upstream; dedupe by
		// (team, project id) so a project visible under both the CR-name
		// and the effective-name prefix cannot contribute twice.
		dedupeKey := meta.TeamID + "\x00" + meta.ProjectID
		if seen[dedupeKey] {
			continue
		}
		seen[dedupeKey] = true
		projects = append(projects, teamAttentionProject{meta: &meta, team: team})
	}
	return projects, skipped, nil
}

// readProjectAttentionRows builds the inbox rows of one project. The second
// return reports whether any task read failed (counted once per project by
// the caller): one broken task object must not hide the rest of the project.
func (h *ProjectHandler) readProjectAttentionRows(ctx context.Context, proj teamAttentionProject, now time.Time) ([]teamAttentionRow, bool) {
	meta := proj.meta
	tasks := meta.Tasks
	if meta.Loop != nil && len(meta.Loop.Tasks) > 0 {
		tasks = meta.Loop.Tasks
	}

	rows := make([]teamAttentionRow, 0, len(tasks))
	failed := false
	seen := map[string]bool{}
	for _, node := range tasks {
		if node.TaskID == "" || seen[node.TaskID] {
			continue
		}
		seen[node.TaskID] = true
		taskRows, ok := h.readTaskAttentionRows(ctx, proj, node, now)
		if !ok {
			failed = true
			continue
		}
		rows = append(rows, taskRows...)
	}
	return rows, failed
}

// readTaskAttentionRows reads one task's attention list and materializes the
// inbox rows. A missing TaskMeta (or one without an attention list) simply
// contributes nothing; the read reports false only when the object exists
// but cannot be decoded (the caller counts the project as skipped).
func (h *ProjectHandler) readTaskAttentionRows(ctx context.Context, proj teamAttentionProject, node projectTaskMeta, now time.Time) ([]teamAttentionRow, bool) {
	key := taskMetaKeys(node.TaskID, proj.team)[0]
	data, err := h.oss.GetObject(ctx, key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, true // no TaskMeta yet: nothing to show, nothing broken
		}
		return nil, false
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false // truncated/legacy TaskMeta: skip the project
	}
	list, ok := raw["attention"].([]any)
	if !ok {
		return nil, true
	}
	title, status := str(raw["title"]), str(raw["status"])
	if title == "" {
		title = node.Title
	}
	if status == "" {
		status = node.Status
	}
	roomID := str(raw["room_id"])
	if roomID == "" {
		roomID = str(raw["roomId"])
	}

	rows := make([]teamAttentionRow, 0, len(list))
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		timeoutAt := str(m["timeoutAt"])
		if timeoutAt == "" {
			timeoutAt = str(m["expiresAt"])
		}
		resolved := m["resolved"] == true
		overdue := !resolved && attentionOverdue(timeoutAt, now)
		row := teamAttentionRow{
			TaskID:      node.TaskID,
			TeamID:      proj.meta.TeamID,
			ProjectID:   proj.meta.ProjectID,
			Kind:        str(m["kind"]),
			Question:    str(m["question"]),
			Options:     teamAttentionOptions(m["options"]),
			RequestedAt: str(m["requestedAt"]),
			TimeoutAt:   timeoutAt,
			Overdue:     overdue,
			Resolved:    resolved,
			ResolvedVia: optionalString(m["resolvedVia"]),
			ResolvedBy:  optionalString(m["resolvedBy"]),
			ResolvedAt:  optionalString(m["resolvedAt"]),
			Resolution:  optionalString(m["resolution"]),
			Freeform:    m["freeform"] == true,
			Notified:    teamAttentionNotifiedFrom(m["notification_log"]),
			RoomID:      roomID,
			URL:         "/projects/" + proj.meta.ProjectID + "/tasks/" + node.TaskID,
		}
		if row.Overdue {
			h.emitAttentionOverdue(ctx, proj, node.TaskID, m, row)
		}
		rows = append(rows, row)
	}
	return rows, true
}

// attentionOverdue reports whether a stored deadline is strictly in the
// past. A missing or unparseable deadline is never overdue (S1: the boolean
// is derived from `timeoutAt`, equality does not count).
func attentionOverdue(timeoutAt string, now time.Time) bool {
	timeoutAt = strings.TrimSpace(timeoutAt)
	if timeoutAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, timeoutAt)
	if err != nil {
		return false
	}
	return t.Before(now)
}

// teamAttentionOptions coerces the optional S0 options list; the row always
// carries an array (empty when the record has none).
func teamAttentionOptions(v any) []teamAttentionOption {
	out := make([]teamAttentionOption, 0)
	list, ok := v.([]any)
	if !ok {
		return out
	}
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, teamAttentionOption{
			ID:          str(m["id"]),
			Label:       str(m["label"]),
			Description: str(m["description"]),
		})
	}
	return out
}

// teamAttentionNotifiedFrom picks the newest notification_log entry (the
// largest attempt; later entries win a tie) and maps it to the row shape.
// `channel_accepted` is passed through verbatim and is never inferred from
// room acceptance (S4a contract). No entries -> nil (the row reports null).
func teamAttentionNotifiedFrom(v any) *teamAttentionNotified {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var best map[string]any
	bestAttempt := -1
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		attempt := intFromAny(m["attempt"])
		if best == nil || attempt >= bestAttempt {
			best = m
			bestAttempt = attempt
		}
	}
	if best == nil {
		return nil
	}
	sent, hasSent := best["sent"].(bool)
	if !hasSent {
		sent = best["room_accepted"] == true
	}
	return &teamAttentionNotified{
		Sent:            sent,
		RoomAccepted:    best["room_accepted"] == true,
		ChannelAccepted: best["channel_accepted"],
		Recipients:      anyList(best["recipients"]),
		Gaps:            anyList(best["gaps"]),
		At:              str(best["ts"]),
	}
}

// anyList returns the stored list as-is, or an empty slice when absent.
func anyList(v any) []any {
	if list, ok := v.([]any); ok && list != nil {
		return list
	}
	return []any{}
}

// filterTeamAttentionRows applies the status filter (open = pending and not
// yet overdue; overdue = pending and past its deadline).
func filterTeamAttentionRows(rows []teamAttentionRow, status string, now time.Time) []teamAttentionRow {
	out := make([]teamAttentionRow, 0, len(rows))
	for _, row := range rows {
		switch status {
		case "all":
			out = append(out, row)
		case "resolved":
			if row.Resolved {
				out = append(out, row)
			}
		case "overdue":
			if row.Overdue {
				out = append(out, row)
			}
		default: // open
			if !row.Resolved && !row.Overdue {
				out = append(out, row)
			}
		}
	}
	return out
}

// sortTeamAttentionRows orders the inbox by urgency: the overdue view is a
// deadline queue (soonest deadline first); every other view is
// newest-requested first, with stable tie-breakers so a re-read is byte
// identical.
func sortTeamAttentionRows(rows []teamAttentionRow, status string) {
	if status == "overdue" {
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].TimeoutAt != rows[j].TimeoutAt {
				return rows[i].TimeoutAt < rows[j].TimeoutAt
			}
			return teamAttentionRowLess(rows[i], rows[j])
		})
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].RequestedAt != rows[j].RequestedAt {
			return rows[i].RequestedAt > rows[j].RequestedAt
		}
		return teamAttentionRowLess(rows[i], rows[j])
	})
}

// teamAttentionRowLess is the deterministic tie-breaker shared by both
// orders (task, kind, attempt, then question).
func teamAttentionRowLess(a, b teamAttentionRow) bool {
	if a.TaskID != b.TaskID {
		return a.TaskID < b.TaskID
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.Question < b.Question
}

// optionalString returns the stored string as a pointer (null when absent)
// so resolved provenance is explicit on the wire.
func optionalString(v any) *string {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	return &s
}

// attentionOverdueRef is the stable identity of one attention record
// (task, attempt, requestedAt) — hashed into the overdue event id per the
// contract id_rule (no timestamp of its own).
func attentionOverdueRef(taskID string, attempt int, requestedAt string) (string, []byte) {
	raw := fmt.Sprintf("%s|%d|%s", taskID, attempt, requestedAt)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:]), []byte(raw)
}

// emitAttentionOverdue emits the S1 overdue event for one pending item,
// best-effort and idempotent (the deterministic id makes a re-read a
// no-op). audit_seq is the project's committed-operation revision; the
// attempt participates through the ref, exactly like the other emitters
// (their id input pins attempt 0 and carries the operation identity in the
// fingerprint).
func (h *ProjectHandler) emitAttentionOverdue(ctx context.Context, proj teamAttentionProject, taskID string, m map[string]any, row teamAttentionRow) {
	ref, refBytes := attentionOverdueRef(taskID, intFromAny(m["attempt"]), row.RequestedAt)
	h.emitSignalEvent(ctx, signalEmitters["AttentionOverdueSweep"], proj.team, signalTeamID(proj.meta, proj.team), proj.meta.ProjectID, taskID, nil, ref, refBytes, map[string]any{
		"project_id":   proj.meta.ProjectID,
		"task_id":      taskID,
		"kind":         row.Kind,
		"attempt":      intFromAny(m["attempt"]),
		"requested_at": row.RequestedAt,
		"timeout_at":   row.TimeoutAt,
	}, proj.meta.AuditSeq)
}
