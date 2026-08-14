package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; read-only queries here

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProjectHandler exposes project / workflow data stored in object storage
// (shared/projects/{id}/meta.json) so humans and frontends can inspect and
// (optionally) intervene in agent-orchestrated workflows.
//
// The TeamHarness MCP projectflow/taskflow actions run inside the worker
// process (stdio JSON-RPC), so the Controller cannot call them directly.
// Instead this handler reads the same MinIO objects the workers sync.
//
// Response schema is aligned with LangGraph Graph.to_json() / StateSnapshot
// (MIT License, © LangChain, Inc.) — see the W-PR design doc.
type ProjectHandler struct {
	client    client.Client
	namespace string
	oss       oss.StorageClient
}

// NewProjectHandler creates a handler reading project state from object storage.
func NewProjectHandler(c client.Client, namespace string, o oss.StorageClient) *ProjectHandler {
	return &ProjectHandler{client: c, namespace: namespace, oss: o}
}

// --- internal model (mirrors meta.json, tolerant to extra fields) ---

type projectMeta struct {
	ProjectID       string            `json:"project_id"`
	Title           string            `json:"title"`
	Status          string            `json:"status"`
	PlanType        string            `json:"plan_type"`
	TeamID          string            `json:"team_id"`
	Mode            string            `json:"mode"`
	Source          string            `json:"source,omitempty"`
	Tasks           []projectTaskMeta `json:"tasks"`
	Loop            *loopMeta         `json:"loop,omitempty"`
	Requester       string            `json:"requester,omitempty"`
	RequesterReport map[string]any    `json:"requester_report,omitempty"`
	ReplyRoute      map[string]any    `json:"reply_route,omitempty"`
	SourceRoomID    string            `json:"source_room_id,omitempty"`
	// W2: human-intervention audit fields (written by W-PR-2 Controller API;
	// tolerated by json.Unmarshal when absent, and passed through here so
	// consumers can show who paused/resumed and why).
	UpdatedBy   string `json:"updated_by,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
}

type projectTaskMeta struct {
	TaskID     string   `json:"task_id"`
	Title      string   `json:"title"`
	AssignedTo string   `json:"assigned_to"`
	DependsOn  []string `json:"depends_on"`
	Status     string   `json:"status"`
}

type loopMeta struct {
	Goal              string            `json:"goal"`
	StopCondition     string            `json:"stop_condition"`
	IterationTemplate string            `json:"iteration_template,omitempty"`
	CurrentIteration  int               `json:"current_iteration"`
	MaxIterations     int               `json:"max_iterations"`
	Status            string            `json:"status"`
	Tasks             []projectTaskMeta `json:"tasks,omitempty"`
	History           []json.RawMessage `json:"history,omitempty"`
}

// --- workflow response (LangGraph-aligned) ---

type workflowResponse struct {
	ProjectID       string              `json:"project_id"`
	Title           string              `json:"title"`
	Status          string              `json:"status"`
	PlanType        string              `json:"plan_type"`
	TeamID          string              `json:"team_id"`
	Mode            string              `json:"mode"`
	Source          string              `json:"source,omitempty"`
	Nodes           []workflowNode      `json:"nodes"`
	Edges           []workflowEdge      `json:"edges"`
	Next            []string            `json:"next"`
	Interrupts      []workflowInterrupt `json:"interrupts"`
	Values          *workflowValues     `json:"values,omitempty"`
	Loop            *loopMeta           `json:"loop,omitempty"`
	Requester       string              `json:"requester,omitempty"`
	RequesterReport map[string]any      `json:"requester_report,omitempty"`
	ReplyRoute      map[string]any      `json:"reply_route,omitempty"`
	SourceRoomID    string              `json:"source_room_id,omitempty"`
	// TasksDetail is populated only when ?includeTasks=true; otherwise it is
	// omitted so the default workflow response stays lightweight.
	TasksDetail []taskDetail `json:"tasks_detail,omitempty"`
	// W2: human-intervention audit fields.
	UpdatedBy   string `json:"updated_by,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
}

// workflowValues is the current state summary (LangGraph StateSnapshot.values
// analog): project-level fields plus a per-status task count.
type workflowValues struct {
	ProjectID string         `json:"project_id"`
	Title     string         `json:"title"`
	Status    string         `json:"status"`
	PlanType  string         `json:"plan_type"`
	TeamID    string         `json:"team_id"`
	Mode      string         `json:"mode"`
	TaskCount map[string]int `json:"task_count"`
}

type workflowNode struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Assignee string `json:"assignee,omitempty"`
}

type workflowEdge struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Conditional bool   `json:"conditional"`
}

type workflowInterrupt struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// taskDetail is the per-task detail payload returned when
// ?includeTasks=true. It surfaces TaskMeta (shared/tasks/{id}/meta.json)
// fields that are not part of the project-level tasks[] node summary: the
// spec path, submission summary/result status, deliverables list and result
// path. TaskMeta is written by TeamHarness taskflow (delegate_task creates
// spec.md + meta.json, submit_task adds summary/result_status/deliverables/
// result_path) and pushed to shared storage via _sync_task, so the same
// dual-prefix scan used for projects applies here.
type taskDetail struct {
	TaskID       string `json:"task_id"`
	ProjectID    string `json:"project_id,omitempty"`
	Status       string `json:"status,omitempty"`
	SpecPath     string `json:"spec_path,omitempty"`
	AssignedTo   string `json:"assigned_to,omitempty"`
	Summary      string `json:"summary,omitempty"`
	ResultStatus string `json:"result_status,omitempty"`
	Deliverables []any  `json:"deliverables,omitempty"`
	ResultPath   string `json:"result_path,omitempty"`
	CancelReason string `json:"cancel_reason,omitempty"`
}

// normalizeTaskStatus maps ProjectMeta task status to the frontend-friendly
// enum: pending | delegated | in-progress | completed | revision | blocked.
//
// Project task nodes are updated by TeamHarness taskflow through their full
// lifecycle (server.py _update_project_task): planned (create/plan_dag) →
// assigned (delegate_task) → in_progress (ack_task) → submitted (submit_task)
// → completed/revision (accept_task_result), plus cancelled (cancel_task).
func normalizeTaskStatus(status string) string {
	switch status {
	case "planned", "":
		return "pending"
	case "assigned":
		return "delegated"
	case "in_progress", "submitted":
		return "in-progress"
	case "completed":
		return "completed"
	case "revision":
		return "revision"
	case "blocked", "cancelled":
		return "blocked"
	default:
		return "pending"
	}
}

// --- prefix resolution ---

// teamProjectPrefixes returns the MinIO prefixes that hold project metadata.
// Team members sync shared/ to teams/{team}/shared/, standalone agents use the
// global shared/ prefix (see sync.py and runtime_config.go SharedPrefix).
//
// The storage prefix uses TeamSpec.EffectiveTeamName (spec.teamName when set,
// else the Team CR name) — the same value EnsureTeamStorage uses. However
// workers resolve their team from the Worker API response (resp.Team = CR
// name), so when spec.teamName differs from the CR name we enumerate BOTH
// prefixes to tolerate the mismatch. The second return maps CR name →
// effective team name for TeamLeader scoping.
func (h *ProjectHandler) teamProjectPrefixes(ctx context.Context) ([]string, map[string]string, error) {
	var teams v1beta1.TeamList
	if err := h.client.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		return nil, nil, err
	}
	prefixes := make([]string, 0, len(teams.Items)+1)
	crToEffective := make(map[string]string, len(teams.Items))
	seen := map[string]bool{}
	for i := range teams.Items {
		effective := teams.Items[i].Spec.EffectiveTeamName(teams.Items[i].Name)
		crToEffective[teams.Items[i].Name] = effective
		for _, name := range []string{effective, teams.Items[i].Name} {
			prefix := "teams/" + name + "/shared/projects/"
			if !seen[prefix] {
				seen[prefix] = true
				prefixes = append(prefixes, prefix)
			}
		}
	}
	prefixes = append(prefixes, "shared/projects/")
	return prefixes, crToEffective, nil
}

// metaKeyFromListResult builds the full object key (relative to the storage
// prefix) for a project's meta.json from an mc ls child entry.
//
// oss.ListObjects runs `mc ls <prefix>` and returns the bare child names —
// project directories end with "/" (e.g. "demo-project-001/"). The meta.json
// lives one level below, so the full key is prefix + dir + "meta.json".
func metaKeyFromListResult(prefix, child string) (string, bool) {
	if !strings.HasSuffix(child, "/") {
		return "", false
	}
	return prefix + child + "meta.json", true
}

// projectMatch is one project id resolved at a specific storage prefix.
type projectMatch struct {
	meta *projectMeta
	team string
}

// resolveProjectMeta locates and reads a project's meta.json across the given
// prefixes and returns ALL matches (deduplicated by owning team). Project ids
// are only unique per worker workspace upstream, so two teams may hold the
// same id; handlers turn 1 match into a resolved project and >1 matches into
// a 409 asking for an explicit ?team= qualifier (never a silent first-match
// that would hide one team's project).
//
// teamFilter narrows the scan to one team's prefix (the ?team= query param).
// Scoped callers (team leader / L2 human) additionally see only matches for
// teams they may access; a scoped caller whose team shares the id is served
// their own team's project without ambiguity.
//
// prefixes must come from teamProjectPrefixes so callers that also need the
// crToEffective map (e.g. for access checks) can share a single K8s List call
// instead of paying two round-trips per request.
func (h *ProjectHandler) resolveProjectMeta(ctx context.Context, projectID string, prefixes []string, teamFilter string, caller *authpkg.CallerIdentity, crToEffective map[string]string) ([]projectMatch, error) {
	var matches []projectMatch
	seenTeam := map[string]bool{}
	accessible := callerAccessiblePrefixes(caller, crToEffective)
	for _, prefix := range prefixes {
		if teamFilter != "" && teamFromPrefix(prefix) != teamFilter {
			continue
		}
		if accessible != nil && !accessible[prefix] {
			continue
		}
		children, err := h.oss.ListObjects(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			key, ok := metaKeyFromListResult(prefix, child)
			if !ok || child != projectID+"/" {
				continue
			}
			data, err := h.oss.GetObject(ctx, key)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue // project dir exists but meta.json not yet written
				}
				return nil, err
			}
			var meta projectMeta
			if err := json.Unmarshal(data, &meta); err != nil {
				// meta.json is written non-atomically upstream (write_text);
				// a crash can leave truncated JSON. Treat as not found for
				// this prefix and keep scanning rather than 500.
				continue
			}
			team := teamFromPrefix(prefix)
			if meta.TeamID == "" {
				meta.TeamID = team
			}
			// The same team may be reachable under both its CR name and its
			// effective name (teamProjectPrefixes emits both); dedupe by the
			// team recorded in the meta so one project never looks like two.
			if seenTeam[meta.TeamID] {
				continue
			}
			seenTeam[meta.TeamID] = true
			matches = append(matches, projectMatch{meta: &meta, team: team})
		}
	}
	return matches, nil
}

// resolveSingleProject applies the 0/1/many resolution of resolveProjectMeta
// and writes the corresponding error responses. Returns (meta, team, true)
// when exactly one match was resolved.
func (h *ProjectHandler) resolveSingleProject(w http.ResponseWriter, matches []projectMatch) (*projectMeta, string, bool) {
	switch len(matches) {
	case 0:
		httputil.WriteError(w, http.StatusNotFound, "project not found")
		return nil, "", false
	case 1:
		return matches[0].meta, matches[0].team, true
	default:
		httputil.WriteError(w, http.StatusConflict, "project id is ambiguous across teams; retry with ?team=")
		return nil, "", false
	}
}

// teamFromPrefix extracts the team name from a project prefix, or "" for the
// global shared/ prefix.
func teamFromPrefix(prefix string) string {
	if strings.HasPrefix(prefix, "teams/") {
		parts := strings.SplitN(prefix, "/", 3)
		if len(parts) == 3 {
			return parts[1]
		}
	}
	return ""
}

// checkProjectAccess performs the handler-side team check for scoped readers
// (team leaders and L2 humans). They may only access projects owned by their
// team(s) (team-scoped prefix). Global projects and other teams' projects are
// denied.
//
// caller.Team is the legacy single Team CR name; caller.Teams is the L2 human
// multi-team set (Human CR accessibleTeams, CR names). crToEffective
// translates each to the effective storage team name (spec.teamName may
// differ from the CR name), so both the CR name and the effective name match.
func (h *ProjectHandler) checkProjectAccess(caller *authpkg.CallerIdentity, team string, crToEffective map[string]string) error {
	if caller == nil || (caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleHuman) {
		return nil
	}
	teams := caller.Teams
	if len(teams) == 0 && caller.Team != "" {
		teams = []string{caller.Team}
	}
	for _, t := range teams {
		eff := t
		if mapped, ok := crToEffective[t]; ok && mapped != "" {
			eff = mapped
		}
		if eff == team {
			return nil
		}
	}
	return &accessDeniedError{msg: "team-leader cannot access project outside team " + caller.Team}
}

// callerAccessiblePrefixes expands a scoped reader's accessible teams (legacy
// single Team or L2 human multi-team set) into the set of project prefixes
// they may read. Projects live under the effective storage name
// (TeamSpec.EffectiveTeamName via EnsureTeamStorage), so each accessible CR
// name maps to its effective prefix; an unresolvable team falls back to its
// own name.
func callerAccessiblePrefixes(caller *authpkg.CallerIdentity, crToEffective map[string]string) map[string]bool {
	if caller == nil || (caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleHuman) {
		return nil
	}
	teams := caller.Teams
	if len(teams) == 0 && caller.Team != "" {
		teams = []string{caller.Team}
	}
	out := make(map[string]bool, len(teams))
	for _, t := range teams {
		eff := t
		if mapped, ok := crToEffective[t]; ok && mapped != "" {
			eff = mapped
		}
		out["teams/"+eff+"/shared/projects/"] = true
	}
	return out
}

type accessDeniedError struct{ msg string }

func (e *accessDeniedError) Error() string { return e.msg }

// --- handlers ---

// GET /api/v1/projects
func (h *ProjectHandler) ListProjects(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	teamFilter := r.URL.Query().Get("team")

	prefixes, crToEffective, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "list projects: resolve prefixes", err)
		return
	}

	type projectSummary struct {
		ProjectID string `json:"project_id"`
		Title     string `json:"title"`
		Status    string `json:"status"`
		PlanType  string `json:"plan_type"`
		TeamID    string `json:"team_id"`
		Mode      string `json:"mode"`
	}
	projects := make([]projectSummary, 0)
	seen := map[string]bool{}

	// Team leaders (legacy single-team SA or L2 human multi-team) only scan
	// their accessible prefixes. The caller's Teams are Team CR names;
	// crToEffective expands to the effective storage names too.
	accessible := callerAccessiblePrefixes(caller, crToEffective)

	// W7: collect all candidate keys first, then fetch meta.json concurrently.
	// Each GetObject is a separate mc subprocess, so N projects would
	// otherwise pay N process spawns serially. A small worker pool collapses
	// that to ~ceil(N/concurrency) rounds.
	type candidate struct {
		prefix string
		key    string
	}
	var cands []candidate
	for _, prefix := range prefixes {
		if accessible != nil && !accessible[prefix] {
			continue
		}
		// ?team= filter: skip prefixes that cannot hold the requested team
		// before hitting OSS (O2). teamFromPrefix("shared/projects/") is ""
		// and standalone projects only match when no filter is set — identical
		// to the meta-level filter below, so this is a pure early-exit.
		if teamFilter != "" && teamFromPrefix(prefix) != teamFilter {
			continue
		}
		children, err := h.oss.ListObjects(r.Context(), prefix)
		if err != nil {
			writeK8sError(w, "list projects", err)
			return
		}
		for _, child := range children {
			key, ok := metaKeyFromListResult(prefix, child)
			if !ok {
				continue
			}
			cands = append(cands, candidate{prefix: prefix, key: key})
		}
	}

	const listConcurrency = 8
	type result struct {
		candidate
		data []byte
		err  error
	}
	results := make([]result, len(cands))
	sem := make(chan struct{}, listConcurrency)
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c candidate) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := h.oss.GetObject(r.Context(), c.key)
			results[i] = result{candidate: c, data: data, err: err}
		}(i, c)
	}
	wg.Wait()

	for _, res := range results {
		if res.err != nil {
			if errors.Is(res.err, os.ErrNotExist) {
				continue // project dir without meta.json yet; skip, not 500
			}
			// W5: a single project read failure must not fail the whole
			// list (one bad/transient object would 500 everything else).
			// Infrastructure-level failures (ListObjects) still 500;
			// per-object data failures are skipped like malformed meta.
			continue
		}
		var meta projectMeta
		if err := json.Unmarshal(res.data, &meta); err != nil {
			continue // skip malformed meta instead of failing the whole list
		}
		if meta.ProjectID == "" {
			continue
		}
		team := teamFromPrefix(res.prefix)
		if meta.TeamID == "" {
			meta.TeamID = team
		}
		// Project ids are only unique per worker workspace upstream; two teams
		// may hold the same id. Dedupe by (team, project_id) so both appear
		// (disambiguated by team_id) instead of one hiding the other.
		if seen[meta.TeamID+"\x00"+meta.ProjectID] {
			continue
		}
		seen[meta.TeamID+"\x00"+meta.ProjectID] = true
		// Optional ?team= filter (mirrors ListWorkers). Team leaders are
		// already scoped by their own prefix; standalone projects have an
		// empty team and are only matched when no filter is set.
		if teamFilter != "" && meta.TeamID != teamFilter {
			continue
		}
		projects = append(projects, projectSummary{
			ProjectID: meta.ProjectID,
			Title:     meta.Title,
			Status:    meta.Status,
			PlanType:  meta.PlanType,
			TeamID:    meta.TeamID,
			Mode:      meta.Mode,
		})
	}

	// Deterministic ordering across prefixes.
	sort.Slice(projects, func(i, j int) bool { return projects[i].ProjectID < projects[j].ProjectID })

	httputil.WriteJSON(w, http.StatusOK, map[string]any{"projects": projects, "total": len(projects)})
}

// GET /api/v1/projects/{id}/workflow
func (h *ProjectHandler) GetProjectWorkflow(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "project id is required")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	includeTasks := r.URL.Query().Get("includeTasks") == "true"
	teamFilter := r.URL.Query().Get("team")

	// Single K8s List for both meta resolution and the access check (O1).
	prefixes, crToEffective, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "get project workflow: resolve prefixes", err)
		return
	}
	matches, err := h.resolveProjectMeta(r.Context(), projectID, prefixes, teamFilter, caller, crToEffective)
	if err != nil {
		writeK8sError(w, "get project workflow", err)
		return
	}
	meta, team, ok := h.resolveSingleProject(w, matches)
	if !ok {
		return
	}
	// W4: hide project existence from scoped callers (L2 / team leader) who
	// do not own this project. resolveProjectMeta scans all prefixes, so a
	// cross-team project is found; returning 403 would let callers enumerate
	// other teams' project ids. Return 404 to hide existence (same as a
	// non-existent id). Admin/Manager (checkProjectAccess returns nil) are
	// unaffected.
	if err := h.checkProjectAccess(caller, team, crToEffective); err != nil {
		if _, ok := err.(*accessDeniedError); ok {
			httputil.WriteError(w, http.StatusNotFound, "project not found")
			return
		}
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusOK, h.buildWorkflow(meta, team, includeTasks))
}

// buildWorkflow converts project meta into the LangGraph-aligned response.
// When includeTasks is true it additionally reads per-task TaskMeta from
// shared storage and attaches tasks_detail.
func (h *ProjectHandler) buildWorkflow(meta *projectMeta, team string, includeTasks bool) *workflowResponse {
	nodes := make([]workflowNode, 0, len(meta.Tasks))
	edges := make([]workflowEdge, 0)
	next := make([]string, 0)
	interrupts := make([]workflowInterrupt, 0)

	// For loop plans the executable graph lives in loop.tasks; otherwise in
	// project.tasks. Mirrors _ready_nodes / _ready_loop_nodes semantics.
	graphTasks := meta.Tasks
	if meta.PlanType == "loop" && meta.Loop != nil {
		graphTasks = meta.Loop.Tasks
	}

	completed := map[string]bool{}
	for _, t := range graphTasks {
		if t.Status == "completed" {
			completed[t.TaskID] = true
		}
		if t.Status == "blocked" || t.Status == "cancelled" {
			interrupts = append(interrupts, workflowInterrupt{ID: t.TaskID, Value: "blocked"})
		}
		nodes = append(nodes, workflowNode{
			ID:       t.TaskID,
			Name:     t.Title,
			Status:   normalizeTaskStatus(t.Status),
			Assignee: t.AssignedTo,
		})
		for _, dep := range t.DependsOn {
			edges = append(edges, workflowEdge{Source: dep, Target: t.TaskID, Conditional: false})
		}
	}

	// Ready nodes: only for active projects (and an active loop); tasks
	// pending/delegated whose dependencies are all completed (mirrors
	// _ready_nodes / _ready_loop_nodes semantics).
	loopBlocked := false
	if meta.Loop != nil {
		if meta.Loop.Status == "completed" || meta.Loop.Status == "blocked" || meta.Loop.Status == "waiting_user" {
			loopBlocked = true
		}
	}
	if !loopBlocked && (meta.Status == "" || meta.Status == "active") {
		for _, t := range graphTasks {
			// Mirror upstream _ready_nodes/_ready_loop_nodes exactly: only
			// tasks whose raw status is planned/assigned can be ready.
			// Checking the raw status (not the normalized output) avoids
			// treating "" or unknown statuses as pending — upstream skips
			// those, so a consumer must not see them as executable.
			if t.Status != "planned" && t.Status != "assigned" {
				continue
			}
			allDone := true
			for _, dep := range t.DependsOn {
				if !completed[dep] {
					allDone = false
					break
				}
			}
			if allDone {
				next = append(next, t.TaskID)
			}
		}
	}

	// Loop interrupts mirror _ready_loop_nodes: a loop waiting on a human
	// decision or blocked surfaces as an interrupt.
	if meta.Loop != nil {
		if meta.Loop.Status == "waiting_user" {
			interrupts = append(interrupts, workflowInterrupt{ID: "loop", Value: "waiting for human decision"})
		} else if meta.Loop.Status == "blocked" {
			interrupts = append(interrupts, workflowInterrupt{ID: "loop", Value: "blocked"})
		}
	}

	// W1: a paused project is a human interrupt in LangGraph terms — the
	// workflow is suspended awaiting a human decision (resume). Surfacing it
	// as an interrupt (in addition to status=paused) lets consumers show
	// "paused by human" without parsing project status separately.
	if meta.Status == "paused" {
		interrupts = append(interrupts, workflowInterrupt{ID: "project", Value: "paused"})
	}

	// values: current state summary (LangGraph StateSnapshot.values analog).
	taskCount := map[string]int{}
	for _, n := range nodes {
		taskCount[n.Status]++
	}

	// includeTasks: read per-task TaskMeta (shared/tasks/{id}/meta.json)
	// from the same dual-prefix layout as projects and attach tasks_detail.
	// Tasks without a TaskMeta file are skipped (the node summary remains
	// authoritative); per-task read errors are skipped too so one bad task
	// does not fail the whole workflow response.
	var tasksDetail []taskDetail
	if includeTasks {
		tasksDetail = h.readTasksDetail(meta, team)
	}

	return &workflowResponse{
		ProjectID:  meta.ProjectID,
		Title:      meta.Title,
		Status:     meta.Status,
		PlanType:   meta.PlanType,
		TeamID:     meta.TeamID,
		Mode:       meta.Mode,
		Source:     meta.Source,
		Nodes:      nodes,
		Edges:      edges,
		Next:       next,
		Interrupts: interrupts,
		Values: &workflowValues{
			ProjectID: meta.ProjectID,
			Title:     meta.Title,
			Status:    meta.Status,
			PlanType:  meta.PlanType,
			TeamID:    meta.TeamID,
			Mode:      meta.Mode,
			TaskCount: taskCount,
		},
		Loop:            meta.Loop,
		Requester:       meta.Requester,
		RequesterReport: meta.RequesterReport,
		ReplyRoute:      meta.ReplyRoute,
		SourceRoomID:    meta.SourceRoomID,
		TasksDetail:     tasksDetail,
		UpdatedBy:       meta.UpdatedBy,
		UpdatedAt:       meta.UpdatedAt,
		PauseReason:     meta.PauseReason,
	}
}

// readTasksDetail reads TaskMeta (shared/tasks/{id}/meta.json) for every task
// in the project's graph and returns the detail list in node order.
//
// TaskMeta is stored under the same dual-prefix layout as projects
// (teams/{team}/shared/tasks/{id}/meta.json for team members, shared/tasks/
// {id}/meta.json for standalone workers) — _sync_task pushes the local
// shared/tasks/{id} directory after delegate/ack/submit/cancel. We probe the
// task prefix belonging to this project's team first, then the global
// prefix, mirroring resolveProjectMeta. Reads are concurrent (W7 pattern) so
// N tasks cost ~ceil(N/8) mc subprocess rounds instead of N serial spawns.
func (h *ProjectHandler) readTasksDetail(meta *projectMeta, team string) []taskDetail {
	// Collect unique task ids from the graph (project tasks, or loop tasks
	// for loop plans — same set buildWorkflow renders).
	graphTasks := meta.Tasks
	if meta.PlanType == "loop" && meta.Loop != nil {
		graphTasks = meta.Loop.Tasks
	}
	taskIDs := make([]string, 0, len(graphTasks))
	seen := map[string]bool{}
	for _, t := range graphTasks {
		if t.TaskID == "" || seen[t.TaskID] {
			continue
		}
		seen[t.TaskID] = true
		taskIDs = append(taskIDs, t.TaskID)
	}
	if len(taskIDs) == 0 {
		return nil
	}

	// Candidate TaskMeta keys: the project's owning scope ONLY, via
	// taskMetaKeys — no cross-scope fallback (reviewer feedback: a team
	// project whose TaskMeta is missing must not leak a global TaskMeta for
	// the same task id).
	var keys []string
	for _, id := range taskIDs {
		keys = append(keys, taskMetaKeys(id, team)...)
	}

	const detailConcurrency = 8
	type keyResult struct {
		taskID string
		key    string
		data   []byte
	}
	results := make([]keyResult, len(keys))
	sem := make(chan struct{}, detailConcurrency)
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, taskID, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := h.oss.GetObject(context.Background(), key)
			if err != nil {
				results[i] = keyResult{taskID: taskID, key: key}
				return
			}
			results[i] = keyResult{taskID: taskID, key: key, data: data}
		}(i, keyForTaskID(key, taskIDs), key)
	}
	wg.Wait()

	// First non-empty match per task id wins (team prefix takes precedence
	// because it is listed first; a task published to the global prefix is a
	// fallback for standalone projects).
	detailByTask := make(map[string]taskDetail, len(taskIDs))
	for _, res := range results {
		if len(res.data) == 0 {
			continue
		}
		if _, ok := detailByTask[res.taskID]; ok {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(res.data, &raw); err != nil {
			continue // malformed TaskMeta; keep node summary only
		}
		// Ownership check: TaskMeta must name exactly this project's graph
		// task — both task_id and project_id must match; absent or mismatched
		// fields are rejected, never mixed in.
		if str(raw["task_id"]) != res.taskID || str(raw["project_id"]) != meta.ProjectID {
			continue
		}
		detail := taskDetail{
			TaskID:       str(taskIDFromRaw(raw, res.taskID)),
			Status:       str(raw["status"]),
			SpecPath:     str(raw["spec_path"]),
			AssignedTo:   str(raw["assigned_to"]),
			Summary:      str(raw["summary"]),
			ResultStatus: str(raw["result_status"]),
			ResultPath:   str(raw["result_path"]),
			CancelReason: str(raw["cancel_reason"]),
		}
		if raw["project_id"] != nil {
			detail.ProjectID = str(raw["project_id"])
		}
		if raw["deliverables"] != nil {
			if list, ok := raw["deliverables"].([]any); ok {
				detail.Deliverables = list
			}
		}
		detailByTask[res.taskID] = detail
	}

	// Return in graph order for stable output.
	out := make([]taskDetail, 0, len(taskIDs))
	for _, id := range taskIDs {
		if d, ok := detailByTask[id]; ok {
			out = append(out, d)
		}
	}
	return out
}

// keyForTaskID maps a candidate key back to the task id by extracting the
// directory component after ".../tasks/". All keys are built from taskIDs in
// readTasksDetail, so a simple suffix search is sufficient; if the lookup
// fails the key index order (team-first) still yields the right task id.
func keyForTaskID(key string, taskIDs []string) string {
	for _, id := range taskIDs {
		if strings.Contains(key, "/tasks/"+id+"/") {
			return id
		}
	}
	return ""
}

// GetTaskArtifact serves the result artifact of one task.
//
// GET /api/v1/projects/{id}/tasks/{taskId}/artifact
//
// The artifact path is NOT taken from the client: it is read from the task's
// TaskMeta (shared/tasks/{id}/meta.json) `result_path` field, which is written
// by TeamHarness submit_task when the worker publishes a result. This keeps
// the endpoint read-only and prevents callers from downloading arbitrary
// objects. The path is then validated against a strict allowlist of prefixes
// so a malicious/compromised worker cannot craft a path that escapes the
// project/task storage area (path traversal / arbitrary file read).
func (h *ProjectHandler) GetTaskArtifact(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	taskID := r.PathValue("taskId")
	if projectID == "" || taskID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "project id and task id are required")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	requestedPath := r.URL.Query().Get("path")
	teamFilter := r.URL.Query().Get("team")

	// Single K8s List for both meta resolution and the access check (O1
	// pattern). Reuse the same dual-prefix layout as GetProjectWorkflow.
	prefixes, crToEffective, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "get task artifact: resolve prefixes", err)
		return
	}
	matches, err := h.resolveProjectMeta(r.Context(), projectID, prefixes, teamFilter, caller, crToEffective)
	if err != nil {
		writeK8sError(w, "get task artifact", err)
		return
	}
	meta, team, ok := h.resolveSingleProject(w, matches)
	if !ok {
		return
	}
	// W4: hide project existence from scoped callers who do not own this
	// project (L2 / team leader). Same 404 semantics as GetProjectWorkflow.
	if err := h.checkProjectAccess(caller, team, crToEffective); err != nil {
		if _, ok := err.(*accessDeniedError); ok {
			httputil.WriteError(w, http.StatusNotFound, "project not found")
			return
		}
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	// The task must belong to this project's graph (project.tasks, or
	// loop.tasks for loop plans). This prevents downloading a result from a
	// task id that exists in shared storage but belongs to another project.
	graphTasks := meta.Tasks
	if meta.PlanType == "loop" && meta.Loop != nil {
		graphTasks = meta.Loop.Tasks
	}
	belongsToProject := false
	for _, t := range graphTasks {
		if t.TaskID == taskID {
			belongsToProject = true
			break
		}
	}
	if !belongsToProject {
		httputil.WriteError(w, http.StatusNotFound, "task not found")
		return
	}

	// Read TaskMeta from the dual-prefix layout (team first, global
	// fallback), mirroring readTasksDetail. Collect the artifact paths the
	// task declares: result_path (published result), spec_path (task spec)
	// and deliverables (published artifact list). The client may request any
	// of them via ?path=; without ?path= the result_path is served (default).
	resultPath, specPath := "", ""
	deliverables := []string{}
	for _, key := range taskMetaKeys(taskID, team) {
		data, err := h.oss.GetObject(r.Context(), key)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			httputil.WriteError(w, http.StatusInternalServerError, "read task meta: "+err.Error())
			return
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			continue // malformed TaskMeta; keep scanning other prefixes
		}
		// Ownership check: TaskMeta must name exactly the requested task —
		// task_id and project_id must both match; absent or mismatched fields
		// never serve artifacts.
		if str(raw["task_id"]) != taskID || str(raw["project_id"]) != projectID {
			continue
		}
		resultPath = str(raw["result_path"])
		specPath = str(raw["spec_path"])
		deliverables = nil
		if list, ok := raw["deliverables"].([]any); ok {
			for _, item := range list {
				if s := str(item); s != "" {
					deliverables = append(deliverables, s)
				}
			}
		}
		break // first readable TaskMeta wins (team prefix listed first)
	}

	// Decide which artifact to serve. Without ?path= we serve result_path
	// (the default published result); with ?path= the requested path must be
	// one of the task's declared artifacts — this prevents downloading files
	// that live in the task dir but were never published (defense in depth
	// beyond the prefix allowlist).
	artifactRelative := ""
	switch {
	case requestedPath == "":
		if resultPath == "" {
			httputil.WriteError(w, http.StatusNotFound, "task has no artifact")
			return
		}
		artifactRelative = resultPath
	default:
		allowed := map[string]bool{resultPath: resultPath != "", specPath: specPath != ""}
		for _, d := range deliverables {
			allowed[d] = true
		}
		if !allowed[requestedPath] {
			httputil.WriteError(w, http.StatusNotFound, "task has no such artifact")
			return
		}
		artifactRelative = requestedPath
	}

	// Path safety: the artifact path is worker-written (result_path /
	// spec_path / deliverables); validate it against the project/task
	// storage area so a bad path cannot escape to arbitrary MinIO objects.
	// Allowed: shared/tasks/{taskID}/... and shared/projects/{projectID}/...
	// (result files live in the task dir, but accept_task_result may also
	// publish a project-level result.md). Reject any path with `..` or an
	// absolute path outright.
	switch {
	case strings.HasPrefix(artifactRelative, "shared/tasks/"+taskID+"/"):
		// OK
	case strings.HasPrefix(artifactRelative, "shared/projects/"+projectID+"/"):
		// OK
	default:
		httputil.WriteError(w, http.StatusNotFound, "task has no artifact")
		return
	}
	if strings.Contains(artifactRelative, "..") || strings.HasPrefix(artifactRelative, "/") {
		httputil.WriteError(w, http.StatusNotFound, "task has no artifact")
		return
	}

	// The artifact lives under the same dual-prefix layout as TaskMeta: a
	// team member's files sync to teams/{team}/shared/..., a standalone
	// worker's to global shared/.... Try the team-scoped key first, then the
	// global one (mirrors taskMetaKeys ordering).
	var data []byte
	var readErr error
	for _, key := range artifactKeys(artifactRelative, team) {
		d, err := h.oss.GetObject(r.Context(), key)
		if err == nil {
			data = d
			break
		}
		readErr = err
	}
	if data == nil {
		if errors.Is(readErr, os.ErrNotExist) {
			httputil.WriteError(w, http.StatusNotFound, "artifact not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "read artifact: "+errStr(readErr))
		return
	}

	// Stream back with an attachment disposition so the file downloads
	// rather than rendering inline. The filename is the basename of the
	// artifact path; Content-Type is inferred from the extension.
	//
	// mime.FormatMediaType produces RFC 5987 filename*=utf-8''... for
	// non-ASCII names (Chinese etc.) and plain filename="..." for ASCII, so
	// browsers decode the download name correctly in both cases.
	fileName := path.Base(artifactRelative)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fileName}))
	w.Header().Set("Content-Type", contentTypeFor(fileName))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// artifactKeys returns candidate object keys for an artifact relative path
// under the given team ("" for global), team-scoped prefix first. Mirrors
// taskMetaKeys: team members sync to teams/{team}/shared/..., standalone
// workers to global shared/...
func artifactKeys(relative, team string) []string {
	keys := make([]string, 0, 2)
	if team != "" && strings.HasPrefix(relative, "shared/") {
		keys = append(keys, "teams/"+team+"/"+relative)
	}
	keys = append(keys, relative)
	return keys
}

// errStr returns err's message or "" for nil, avoiding a nil deref in error
// paths where the last read error may be nil when the key list is empty.
func errStr(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// taskMetaKeys returns the candidate TaskMeta keys for a task in the given
// team ("" for global), team-scoped prefix first. Mirrors readTasksDetail.
// taskMetaKeys returns the TaskMeta object keys for a task under the project's
// owning scope. Team-scoped projects read ONLY their team prefix; standalone
// projects read ONLY the global prefix. There is deliberately no cross-scope
// fallback: a team project must never mix in an unrelated global task that
// happens to share the task id (reviewer feedback — ownership must be
// verified, not guessed across scopes).
func taskMetaKeys(taskID, team string) []string {
	if team != "" {
		return []string{"teams/" + team + "/shared/tasks/" + taskID + "/meta.json"}
	}
	return []string{"shared/tasks/" + taskID + "/meta.json"}
}

// contentTypeFor maps a file extension to a Content-Type for artifact
// downloads. Unknown extensions fall back to application/octet-stream.
func contentTypeFor(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".md", ".markdown", ".txt":
		return "text/markdown; charset=utf-8"
	case ".pdf":
		return "application/pdf"
	case ".json":
		return "application/json"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".doc", ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xls", ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".ppt", ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".zip":
		return "application/zip"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// str is a small helper to coerce a JSON value to string ("" for nil).
func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// taskIDFromRaw prefers the raw task_id field, falling back to the key-derived id.
func taskIDFromRaw(raw map[string]any, fallback string) string {
	if s := str(raw["task_id"]); s != "" {
		return s
	}
	return fallback
}

// --- spawn aggregation (GET /api/v1/projects/{id}/spawns) ---
//
// Spawn subagents (spawn_subagent) are the primary workhorses of the
// AgentTeam architecture: workers schedule, spawns execute. The TeamHarness
// meta.json only tracks the team-level task state, so this endpoint reads
// each team worker's chats.json (mirrored into object storage by the
// worker's FileSync) and aggregates the spawn sessions.
//
// Parent-child linkage (meta.root_session_id / meta.spawn) is persisted by
// the QwenPaw spawn-linkage feature (2.1+). On 2.0.1 workers those fields
// are absent; the endpoint degrades gracefully (root_session_id null, sub-
// prefix detection) so the client can fall back to a flat list.

// workerChatsFile mirrors the worker-side chats.json written by QwenPaw's
// JsonChatRepository. Both 2.0.1 and 2.1 write {version, chats: [...]}.
type workerChatsFile struct {
	Version int          `json:"version"`
	Chats   []workerChat `json:"chats"`
}

// workerChat is the per-chat subset needed for spawn aggregation.
type workerChat struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	SessionID string         `json:"session_id"`
	UserID    string         `json:"user_id"`
	Channel   string         `json:"channel"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	Meta      map[string]any `json:"meta"`
	Status    string         `json:"status"`
	Source    string         `json:"source"`
}

// projectSpawnsResponse is the GET /api/v1/projects/{id}/spawns payload.
type projectSpawnsResponse struct {
	ProjectID string         `json:"project_id"`
	Workers   []workerSpawns `json:"workers"`
}

type workerSpawns struct {
	Worker string      `json:"worker"`
	Spawns []spawnInfo `json:"spawns"`
}

type spawnInfo struct {
	SessionID     string   `json:"session_id"`
	Name          string   `json:"name,omitempty"`
	Status        string   `json:"status"`
	CreatedAt     string   `json:"created_at,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
	RootSessionID *string  `json:"root_session_id"` // null on 2.0.1 workers
	Spawn         bool     `json:"spawn"`
	AllowedTools  []string `json:"subagent_allowed_tools,omitempty"`
	Skills        []string `json:"subagent_skills,omitempty"`
}

// normalizeSessionKey canonicalizes a Matrix room id or channel session id
// into the chats.json session_id form. Bare room ids (!abc:server) gain the
// canonical lowercase `matrix:` prefix; full session keys keep their channel
// prefix but fold it to lowercase (Matrix:!abc -> matrix:!abc). Empty stays
// empty. This lets room_id values from TaskMeta and session_id values from
// chats.json be compared reliably.
//
// A leading "!" disambiguates a bare Matrix room id (which itself contains
// a ":" domain separator) from a channel session key.
func normalizeSessionKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "!") {
		return "matrix:" + s
	}
	if i := strings.Index(s, ":"); i > 0 {
		return strings.ToLower(s[:i]) + s[i:]
	}
	return "matrix:" + s
}

// isSpawnChat reports whether a chat entry represents a spawn subagent
// session. 2.1 persists meta.spawn; 2.0.1 only leaves the `sub-` session id
// prefix as a signal. Both are accepted (plus a future source="spawn").
func isSpawnChat(c workerChat) bool {
	if b, ok := c.Meta["spawn"].(bool); ok && b {
		return true
	}
	if c.Source == "spawn" {
		return true
	}
	return strings.HasPrefix(c.SessionID, "sub-")
}

// spawnRootSession returns the persisted root session id (normalized) or
// nil when absent (2.0.1 workers).
func spawnRootSession(c workerChat) *string {
	if s := str(c.Meta["root_session_id"]); s != "" {
		normalized := normalizeSessionKey(s)
		return &normalized
	}
	return nil
}

// spawnAllowedTools returns the tool whitelist persisted in the chat meta
// (absent on 2.0.1). spawnSkills is the same for the skill whitelist; both
// are written by the QwenPaw spawn linkage feature from the spawn's
// request_context.
func spawnAllowedTools(c workerChat) []string {
	return spawnMetaList(c, "subagent_allowed_tools")
}

func spawnSkills(c workerChat) []string {
	return spawnMetaList(c, "subagent_skills")
}

func spawnMetaList(c workerChat, key string) []string {
	raw, ok := c.Meta[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s := str(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// teamWorkerNames resolves the Worker CRs referenced by the team's
// WorkerMembers into their MinIO storage names (WorkerSpec.WorkerName is the
// OSS path key; fall back to the CR name).
func (h *ProjectHandler) teamWorkerNames(ctx context.Context, team string) []string {
	var teams v1beta1.TeamList
	if err := h.client.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		return nil
	}
	var memberNames []string
	for i := range teams.Items {
		if teams.Items[i].Spec.EffectiveTeamName(teams.Items[i].Name) != team {
			continue
		}
		for _, m := range teams.Items[i].Spec.WorkerMembers {
			memberNames = append(memberNames, m.Name)
		}
		break
	}
	if len(memberNames) == 0 {
		return nil
	}

	byName := map[string]string{}
	var workers v1beta1.WorkerList
	if err := h.client.List(ctx, &workers, client.InNamespace(h.namespace)); err == nil {
		for i := range workers.Items {
			name := workers.Items[i].Spec.WorkerName
			if name == "" {
				name = workers.Items[i].Name
			}
			byName[workers.Items[i].Name] = name
		}
	}
	out := make([]string, 0, len(memberNames))
	for _, n := range memberNames {
		if storage, ok := byName[n]; ok {
			out = append(out, storage)
		}
	}
	sort.Strings(out)
	return out
}

// collectProjectRooms derives the set of session/room ids that belong to
// this project: the project's source_room_id plus every graph task's
// TaskMeta.room_id. Spawn association is derived through this set — a spawn's
// persisted root_session_id (QwenPaw 2.1+) is the session that called
// spawn_subagent, i.e. one of these rooms. Reads are best-effort: an
// unreadable TaskMeta just contributes nothing, never a 500.
func (h *ProjectHandler) collectProjectRooms(ctx context.Context, meta *projectMeta, team string) map[string]bool {
	rooms := map[string]bool{}
	if r := normalizeSessionKey(meta.SourceRoomID); r != "" {
		rooms[r] = true
	}
	graphTasks := meta.Tasks
	if meta.PlanType == "loop" && meta.Loop != nil {
		graphTasks = meta.Loop.Tasks
	}
	seen := map[string]bool{}
	for _, t := range graphTasks {
		if t.TaskID == "" || seen[t.TaskID] {
			continue
		}
		seen[t.TaskID] = true
		for _, key := range taskMetaKeys(t.TaskID, team) {
			data, err := h.oss.GetObject(ctx, key)
			if err != nil {
				continue
			}
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				continue
			}
			if str(raw["task_id"]) != t.TaskID || str(raw["project_id"]) != meta.ProjectID {
				continue // another task/project sharing this id
			}
			if r := normalizeSessionKey(str(raw["room_id"])); r != "" {
				rooms[r] = true
			}
			break
		}
	}
	return rooms
}

// spawnBelongsToProject reports whether a spawn chat belongs to the project's
// room set. A spawn without a persisted root_session_id (2.0.1 legacy) cannot
// be associated safely and is omitted; a root outside the project's rooms is
// another project's activity (same team, different project) and is omitted
// too. Reviewer feedback: never attach a spawn to every project of its team.
func spawnBelongsToProject(c workerChat, rooms map[string]bool) bool {
	root := spawnRootSession(c)
	if root == nil {
		return false
	}
	return rooms[*root]
}

// workerChatsPath is the workspace-relative path of a worker's chats.json.
// The worker FileSync mirrors its workspace (including .qwenpaw/workspaces/
// default/chats.json — not in the background-push exclusion list) under
// agents/{worker}/.
const workerChatsPath = ".qwenpaw/workspaces/default/chats.json"

func (h *ProjectHandler) readWorkerSpawns(ctx context.Context, worker string, rooms map[string]bool) []spawnInfo {
	out := make([]spawnInfo, 0)
	data, err := h.oss.GetObject(ctx, "agents/"+worker+"/"+workerChatsPath)
	if err != nil {
		return out // chats.json missing/unreadable: skip worker, don't 500
	}
	var file workerChatsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return out // truncated/legacy shape: skip worker
	}
	for _, c := range file.Chats {
		if !isSpawnChat(c) {
			continue
		}
		if !spawnBelongsToProject(c, rooms) {
			continue // unassociated (legacy) or another project's spawn
		}
		out = append(out, spawnInfo{
			SessionID:     c.SessionID,
			Name:          c.Name,
			Status:        c.Status,
			CreatedAt:     c.CreatedAt,
			UpdatedAt:     c.UpdatedAt,
			RootSessionID: spawnRootSession(c),
			Spawn:         true,
			AllowedTools:  spawnAllowedTools(c),
			Skills:        spawnSkills(c),
		})
	}
	return out
}

// GetProjectSpawns handles GET /api/v1/projects/{id}/spawns.
// Auth/RBAC mirrors GetProjectWorkflow: RoleHuman read + team-scope check,
// cross-team access hidden as 404.
func (h *ProjectHandler) GetProjectSpawns(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "project id is required")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	teamFilter := r.URL.Query().Get("team")

	prefixes, crToEffective, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "get project spawns: resolve prefixes", err)
		return
	}
	matches, err := h.resolveProjectMeta(r.Context(), projectID, prefixes, teamFilter, caller, crToEffective)
	if err != nil {
		writeK8sError(w, "get project spawns", err)
		return
	}
	meta, team, ok := h.resolveSingleProject(w, matches)
	if !ok {
		return
	}
	// W4: hide existence from scoped callers who do not own this project.
	if err := h.checkProjectAccess(caller, team, crToEffective); err != nil {
		if _, ok := err.(*accessDeniedError); ok {
			httputil.WriteError(w, http.StatusNotFound, "project not found")
			return
		}
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	rooms := h.collectProjectRooms(r.Context(), meta, team)
	workers := h.teamWorkerNames(r.Context(), team)
	resp := projectSpawnsResponse{ProjectID: projectID, Workers: make([]workerSpawns, 0, len(workers))}
	for _, worker := range workers {
		resp.Workers = append(resp.Workers, workerSpawns{
			Worker: worker,
			Spawns: h.readWorkerSpawns(r.Context(), worker, rooms),
		})
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// --- spawn messages (GET /api/v1/projects/{id}/spawns/{sessionId}/messages) ---
//
// The aggregation endpoint above returns per-session metadata only. This
// endpoint drills into one spawn session and returns its conversation stream
// from the worker's history.db (QwenPaw's scroll HistoryStore, mirrored into
// object storage by FileSync). The schema is stable across 2.0.1 and 2.1
// (verified against both tags): the same conversation_history table with
// seq/session_id/kind/role/name/content/tool_state/headline/created_at.
//
// Reading is strictly read-only against a temp copy: the db (plus -wal/-shm
// when present) is pulled from object storage and opened with mode=ro,
// falling back to immutable=1 when the WAL sidecars are missing or
// inconsistent (a worker-side snapshot may capture the main file between
// checkpoints; immutable reads the checkpointed portion only).
//
// The stream keeps user context messages, model turns and tool results in
// ascending order, most recent limit items; has_more reports whether older
// rows exist. task carries the first user message — the spawn task text.

// workerHistoryDBPath is the workspace-relative path of a worker's
// history.db (QwenPaw scroll HistoryStore).
const workerHistoryDBPath = ".qwenpaw/workspaces/default/history.db"

const (
	defaultSpawnMessagesLimit = 20
	maxSpawnMessagesLimit     = 50
)

type spawnMessage struct {
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`
	Role      string `json:"role,omitempty"`
	Name      string `json:"name,omitempty"`
	Content   string `json:"content,omitempty"`
	ToolState string `json:"tool_state,omitempty"`
	Headline  string `json:"headline,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type spawnMessagesResponse struct {
	SessionID string         `json:"session_id"`
	Task      string         `json:"task,omitempty"`
	Messages  []spawnMessage `json:"messages"`
	HasMore   bool           `json:"has_more"`
}

// findSpawnWorker returns the worker whose chats.json contains the spawn
// session plus the chat's raw session id (the value history.db rows are
// keyed by), or "" when no team worker owns it. Session keys are normalized
// on both sides so bare room ids and channel-prefixed keys compare reliably;
// the raw value is kept for the SQL lookup because history.db stores the
// original key.
func (h *ProjectHandler) findSpawnWorker(ctx context.Context, workers []string, sessionID string) (string, string, *string) {
	for _, worker := range workers {
		data, err := h.oss.GetObject(ctx, "agents/"+worker+"/"+workerChatsPath)
		if err != nil {
			continue
		}
		var file workerChatsFile
		if err := json.Unmarshal(data, &file); err != nil {
			continue
		}
		for _, c := range file.Chats {
			if !isSpawnChat(c) {
				continue
			}
			if normalizeSessionKey(c.SessionID) == sessionID {
				return worker, c.SessionID, spawnRootSession(c)
			}
		}
	}
	return "", "", nil
}

// openReadOnlySQLite opens a SQLite database read-only, falling back to
// immutable mode (skips WAL/lock handling) when the normal open fails —
// e.g. a worker snapshot whose -wal/-shm sidecars are missing or stale.
func openReadOnlySQLite(path string) (*sql.DB, error) {
	for _, uri := range []string{
		"file:" + path + "?mode=ro",
		"file:" + path + "?mode=ro&immutable=1",
	} {
		db, err := sql.Open("sqlite", uri)
		if err == nil {
			if err = db.Ping(); err == nil {
				return db, nil
			}
			_ = db.Close()
		}
	}
	return nil, fmt.Errorf("open %s: not a readable sqlite database", path)
}

// readSpawnMessages pulls the worker's history.db from object storage and
// returns the spawn session's conversation stream. Failures degrade to a
// non-200 status (never a 500): the db is missing/unreadable on the object
// store, or the copy cannot be opened as SQLite.
func (h *ProjectHandler) readSpawnMessages(ctx context.Context, worker, sessionID string, limit int) (spawnMessagesResponse, int) {
	resp := spawnMessagesResponse{SessionID: sessionID, Messages: make([]spawnMessage, 0)}
	unavailable := func() (spawnMessagesResponse, int) {
		return resp, http.StatusNotFound
	}

	dbData, err := h.oss.GetObject(ctx, "agents/"+worker+"/"+workerHistoryDBPath)
	if err != nil {
		return unavailable()
	}
	tmp, err := os.MkdirTemp("", "spawn-msgs-")
	if err != nil {
		return resp, http.StatusInternalServerError
	}
	defer os.RemoveAll(tmp)
	dbPath := filepath.Join(tmp, "history.db")
	if err := os.WriteFile(dbPath, dbData, 0o600); err != nil {
		return resp, http.StatusInternalServerError
	}
	// Best-effort WAL sidecars: the most recent rows live in -wal until a
	// checkpoint; without them (or when stale) the immutable fallback below
	// still serves the checkpointed portion.
	for _, suffix := range []string{"-wal", "-shm"} {
		if sidecar, err := h.oss.GetObject(ctx, "agents/"+worker+"/"+workerHistoryDBPath+suffix); err == nil {
			_ = os.WriteFile(dbPath+suffix, sidecar, 0o600)
		}
	}

	db, err := openReadOnlySQLite(dbPath)
	if err != nil {
		return unavailable()
	}
	defer db.Close()

	// task: the first user message is the spawn task text.
	_ = db.QueryRow(
		"SELECT content FROM conversation_history WHERE session_id=? AND role='user' ORDER BY seq ASC LIMIT 1",
		sessionID,
	).Scan(&resp.Task)

	rows, err := db.Query(
		"SELECT seq, kind, role, name, content, tool_state, headline, created_at "+
			"FROM conversation_history WHERE session_id=? ORDER BY seq DESC LIMIT ?",
		sessionID, limit+1,
	)
	if err != nil {
		return unavailable()
	}
	defer rows.Close()

	desc := make([]spawnMessage, 0, limit)
	for rows.Next() {
		var m spawnMessage
		var role, name, content, toolState, headline, createdAt sql.NullString
		if err := rows.Scan(&m.Seq, &m.Kind, &role, &name, &content, &toolState, &headline, &createdAt); err != nil {
			continue
		}
		m.Role, m.Name, m.Content = role.String, name.String, content.String
		m.ToolState, m.Headline, m.CreatedAt = toolState.String, headline.String, createdAt.String
		desc = append(desc, m)
	}
	if len(desc) > limit {
		resp.HasMore = true
		desc = desc[:limit]
	}
	resp.Messages = make([]spawnMessage, len(desc))
	for i := range desc {
		resp.Messages[len(desc)-1-i] = desc[i]
	}
	return resp, http.StatusOK
}

// GetProjectSpawnMessages handles GET /api/v1/projects/{id}/spawns/{sessionId}/messages.
// Auth/RBAC mirrors GetProjectSpawns: RoleHuman read + team-scope check,
// cross-team access and unknown sessions hidden as 404.
func (h *ProjectHandler) GetProjectSpawnMessages(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	sessionID := normalizeSessionKey(r.PathValue("sessionId"))
	if projectID == "" || sessionID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "project id and session id are required")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	teamFilter := r.URL.Query().Get("team")

	prefixes, crToEffective, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "get spawn messages: resolve prefixes", err)
		return
	}
	matches, err := h.resolveProjectMeta(r.Context(), projectID, prefixes, teamFilter, caller, crToEffective)
	if err != nil {
		writeK8sError(w, "get spawn messages", err)
		return
	}
	meta, team, ok := h.resolveSingleProject(w, matches)
	if !ok {
		return
	}
	// W4: hide existence from scoped callers who do not own this project.
	if err := h.checkProjectAccess(caller, team, crToEffective); err != nil {
		if _, ok := err.(*accessDeniedError); ok {
			httputil.WriteError(w, http.StatusNotFound, "project not found")
			return
		}
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	limit := defaultSpawnMessagesLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
			if limit > maxSpawnMessagesLimit {
				limit = maxSpawnMessagesLimit
			}
		}
	}

	worker, rawSessionID, root := h.findSpawnWorker(r.Context(), h.teamWorkerNames(r.Context(), team), sessionID)
	if worker == "" {
		httputil.WriteError(w, http.StatusNotFound, "spawn session not found")
		return
	}
	// Project scoping: the spawn must belong to this project (root session in
	// the project's room set). Legacy spawns without a root and other
	// projects' spawns are hidden as 404.
	rooms := h.collectProjectRooms(r.Context(), meta, team)
	spawnChat := workerChat{Meta: map[string]any{}}
	if root != nil {
		spawnChat.Meta["root_session_id"] = *root
	}
	if !spawnBelongsToProject(spawnChat, rooms) {
		httputil.WriteError(w, http.StatusNotFound, "spawn session not found")
		return
	}
	resp, status := h.readSpawnMessages(r.Context(), worker, rawSessionID, limit)
	if status != http.StatusOK {
		httputil.WriteError(w, status, "spawn messages unavailable")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}
