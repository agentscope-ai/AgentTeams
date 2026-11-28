package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

type teamTask struct {
	projectTaskMeta
	MainTaskID   string `json:"main_task_id,omitempty"`
	TaskKind     string `json:"task_kind,omitempty"`
	RoomID       string `json:"room_id,omitempty"`
	SourceRoomID string `json:"source_room_id,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	ProjectID    string `json:"project_id"`
	TeamID       string `json:"team_id"`
}

type taskTrace struct {
	TaskID       string           `json:"task_id,omitempty"`
	Correlation  string           `json:"correlation,omitempty"`
	Trace        map[string]any   `json:"trace"`
	Observations []map[string]any `json:"observations"`
}

type traceEvidence struct {
	TraceID     string         `json:"trace_id"`
	SpanID      string         `json:"span_id"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Observation map[string]any `json:"observation,omitempty"`
}

type traceLink struct {
	From      string `json:"from_trace_id"`
	To        string `json:"to_trace_id"`
	SpanID    string `json:"upstream_span_id,omitempty"`
	Available bool   `json:"available"`
}

type taskTraceBundle struct {
	ProjectID         string            `json:"project_id,omitempty"`
	TaskID            string            `json:"task_id"`
	TeamID            string            `json:"team_id"`
	QueriedAt         string            `json:"queried_at"`
	RetrievalComplete bool              `json:"retrieval_complete"`
	Tasks             []projectTaskMeta `json:"tasks"`
	Links             []traceLink       `json:"causal_links"`
	Evidence          []traceEvidence   `json:"evidence"`
	Gaps              []string          `json:"gaps"`
	Traces            []taskTrace       `json:"traces"`
}

// projectTraceTasks reads the current DAG and Loop task nodes.
func projectTraceTasks(meta *projectMeta) []projectTaskMeta {
	if meta.PlanType == "loop" && meta.Loop != nil {
		return meta.Loop.Tasks
	}
	return meta.Tasks
}

type langfuseQuery struct {
	LangfuseURL string `json:"langfuseUrl"`
	PublicKey   string `json:"publicKey"`
	SecretKey   string `json:"secretKey"`
}

// readPages never treats a failed or missing page as a complete collection.
func (config langfuseQuery) readPages(ctx context.Context, resource string, params url.Values) ([]map[string]any, error) {
	rows := make([]map[string]any, 0)
	params.Set("limit", "100")
	type pageResult struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			TotalPages *int `json:"totalPages"`
		} `json:"meta"`
	}
	lastPage := 1
	for start := 1; start <= lastPage; {
		end := min(start+4, lastPage+1)
		pages := make([]pageResult, end-start)
		group, pageContext := errgroup.WithContext(ctx)
		for page := start; page < end; page++ {
			index := page - start
			group.Go(func() error {
				query := make(url.Values, len(params))
				for key, values := range params {
					query[key] = append([]string(nil), values...)
				}
				query.Set("page", strconv.Itoa(page))
				if err := config.get(pageContext, resource+"?"+query.Encode(), &pages[index]); err != nil {
					return err
				}
				if pages[index].Meta.TotalPages == nil || *pages[index].Meta.TotalPages < 0 {
					return fmt.Errorf("invalid Langfuse pagination response")
				}
				return nil
			})
		}
		if err := group.Wait(); err != nil {
			return nil, err
		}
		// Append by page number, not completion time. If the live collection
		// grows during retrieval, include its newly reported pages as before.
		for _, result := range pages {
			rows = append(rows, result.Data...)
			lastPage = max(lastPage, *result.Meta.TotalPages)
		}
		if lastPage > 1000 {
			return nil, fmt.Errorf("Langfuse query exceeds 1000 pages; no complete result available")
		}
		start = end
	}
	return rows, nil
}

// readTraceObservations batches only already-authorized trace IDs. Every page
// must succeed and every returned observation must remain inside that batch.
func (config langfuseQuery) readTraceObservations(ctx context.Context, ids []string) (map[string][]map[string]any, error) {
	batches := make([][]map[string]any, (len(ids)+99)/100)
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for start := 0; start < len(ids); start += 100 {
		index, batch := start/100, ids[start:min(start+100, len(ids))]
		group.Go(func() error {
			allowed := make(map[string]bool, len(batch))
			for _, id := range batch {
				allowed[id] = true
			}
			filter, _ := json.Marshal([]map[string]any{{"type": "stringOptions", "column": "traceId", "operator": "any of", "value": batch}})
			rows, err := config.readPages(ctx, "observations", url.Values{"filter": {string(filter)}})
			if err != nil {
				return err
			}
			for _, row := range rows {
				if !allowed[str(row["traceId"])] {
					return fmt.Errorf("Langfuse returned an observation from another trace")
				}
			}
			batches[index] = rows
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	result := make(map[string][]map[string]any, len(ids))
	for _, id := range ids {
		result[id] = []map[string]any{}
	}
	for _, batch := range batches {
		for _, row := range batch {
			id := str(row["traceId"])
			result[id] = append(result[id], row)
		}
	}
	return result, nil
}

func (config langfuseQuery) get(ctx context.Context, resource string, result any) error {
	// Complete pages with captured I/O can take longer than ten seconds.
	// The caller's overall query deadline still bounds pagination.
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", config.LangfuseURL+"/api/public/"+resource, nil)
	if err != nil {
		return fmt.Errorf("invalid Langfuse query")
	}
	req.SetBasicAuth(config.PublicKey, config.SecretKey)
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Langfuse query failed or timed out")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 32*1024*1024+1))
	if err != nil || response.StatusCode != 200 || len(data) > 32*1024*1024 {
		return fmt.Errorf("Langfuse query failed (HTTP %d); no complete result available", response.StatusCode)
	}
	if json.Unmarshal(data, result) != nil {
		return fmt.Errorf("invalid Langfuse response")
	}
	return nil
}

// traceField accepts both native attributes and Langfuse's nested metadata.
func traceField(row map[string]any, key string) string {
	metadata, _ := row["metadata"].(map[string]any)
	attributes, _ := metadata["attributes"].(map[string]any)
	if value := str(metadata[key]); value != "" {
		if nested := str(attributes[key]); nested != "" && nested != value {
			return "\x00"
		}
		return value
	}
	return str(attributes[key])
}

func traceIdentity(row map[string]any, label string) string {
	value := traceField(row, "agentteams."+label+".id")
	tags, _ := row["tags"].([]any)
	for _, tag := range tags {
		if text, ok := tag.(string); ok && strings.HasPrefix(text, label+":") {
			candidate := strings.TrimPrefix(text, label+":")
			if value != "" && value != candidate {
				return "\x00"
			}
			value = candidate
		}
	}
	return value
}

func relatedTaskIDs(tasks []projectTaskMeta, target string, related bool) map[string]bool {
	selected := map[string]bool{target: true}
	if !related {
		return selected
	}
	// Ancestors and descendants are separate traversals: siblings are not
	// downstream merely because they share an ancestor.
	for _, upstream := range []bool{true, false} {
		visited := map[string]bool{target: true}
		for changed := true; changed; {
			changed = false
			for _, task := range tasks {
				for _, dep := range task.DependsOn {
					from, to := dep, task.TaskID
					if upstream {
						from, to = to, from
					}
					if visited[from] && !visited[to] {
						visited[to] = true
						selected[to] = true
						changed = true
					}
				}
			}
		}
	}
	return selected
}

// GetTaskTraces authorizes the project before making any backend request.
func (h *ProjectHandler) GetTaskTraces(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, 401, "authentication required")
		return
	}
	if caller.Role != authpkg.RoleManager && caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleHuman {
		httputil.WriteError(w, 403, "role cannot read task traces")
		return
	}
	projectID, taskID := r.PathValue("id"), r.PathValue("taskId")
	if !isSafeTaskID(projectID) || !isSafeTaskID(taskID) {
		httputil.WriteError(w, 400, "invalid project or task id")
		return
	}
	related := r.URL.Query().Get("includeRelated")
	if related != "" && related != "true" && related != "false" {
		httputil.WriteError(w, 400, "includeRelated must be true or false")
		return
	}
	prefixes, names, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "resolve trace project", err)
		return
	}
	teamFilter := r.URL.Query().Get("team")
	if effective := names[teamFilter]; effective != "" {
		teamFilter = effective
	}
	matches, err := h.resolveProjectMeta(r.Context(), projectID, prefixes, teamFilter, caller, names)
	if err != nil {
		writeK8sError(w, "resolve trace project", err)
		return
	}
	meta, team, ok := h.resolveSingleProject(w, matches)
	if !ok {
		return
	}
	if h.checkProjectAccess(caller, team, names) != nil {
		httputil.WriteError(w, 404, "project not found")
		return
	}
	tasks := projectTraceTasks(meta)
	found := false
	for _, task := range tasks {
		if task.TaskID == taskID {
			found = true
		}
	}
	if !found {
		httputil.WriteError(w, 404, "task not found")
		return
	}
	h.writeTaskTraces(w, r, projectID, taskID, team, names, tasks, related == "true", "project", projectID, "", "")
}

// GetMainTaskTraces uses the same authorized Task storage scope as Task reads.
func (h *ProjectHandler) GetMainTaskTraces(w http.ResponseWriter, r *http.Request) {
	caller := traceReader(w, r)
	if caller == nil {
		return
	}
	id := r.PathValue("id")
	if !isSafeTaskID(id) {
		httputil.WriteError(w, 400, "invalid task id")
		return
	}
	key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	body, err := h.oss.GetObject(r.Context(), key)
	var record struct {
		ID        string            `json:"task_id"`
		Kind      string            `json:"task_kind"`
		ProjectID string            `json:"project_id"`
		Status    string            `json:"status"`
		RoomID    string            `json:"room_id"`
		Title     string            `json:"title"`
		Tasks     []projectTaskMeta `json:"tasks"`
	}
	if err != nil || json.Unmarshal(body, &record) != nil || record.ID != id {
		httputil.WriteError(w, 503, "task state unavailable")
		return
	}
	if record.Kind != "main" {
		httputil.WriteError(w, 400, "main task required")
		return
	}
	_, names, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "resolve trace team", err)
		return
	}
	tasks := append([]projectTaskMeta{{TaskID: id, Title: record.Title, Status: record.Status}}, record.Tasks...)
	h.writeTaskTraces(w, r, record.ProjectID, id, team, names, tasks, true, "main_task", id, record.RoomID, strings.TrimSuffix(key, id+"/meta.json"))
}

func (h *ProjectHandler) writeTaskTraces(w http.ResponseWriter, r *http.Request, projectID, taskID, team string, names map[string]string, tasks []projectTaskMeta, related bool, scopeLabel, scopeID, scopeRoom, taskPrefix string) {
	caller := authpkg.CallerFromContext(r.Context())
	config, err := h.langfuseConfiguration(r.Context())
	if err != nil {
		httputil.WriteError(w, 503, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	workers, err := h.traceWorkers(ctx, caller)
	if err != nil {
		httputil.WriteError(w, 503, "cannot verify trace resource scope")
		return
	}
	// Query the authoritative task scope without a lower time cutoff, so late spans, retries,
	// old rounds and reverse causal references are discoverable.
	queriedAt := time.Now().UTC().Format(time.RFC3339Nano)
	// Observations are read below in authorized batches. Avoid asking trace
	// discovery to aggregate the same observations and metrics again.
	params := url.Values{"tags": {"agentteams", scopeLabel + ":" + scopeID}, "fields": {"core,io"}, "toTimestamp": {queriedAt}}
	if scopeLabel == "main_task" && scopeRoom != "" {
		params["tags"] = []string{"agentteams"}
		var filters []map[string]any
		_ = json.Unmarshal([]byte(traceWorkerFilter(workers, params)), &filters)
		filters = append(filters, map[string]any{"type": "arrayOptions", "column": "tags", "operator": "any of", "value": []string{"main_task:" + scopeID, "room:" + scopeRoom}})
		encoded, _ := json.Marshal(filters)
		params.Set("filter", string(encoded))
	} else {
		params.Set("filter", traceWorkerFilter(workers, params))
	}
	rows, err := config.readPages(ctx, "traces", params)
	if err != nil {
		httputil.WriteError(w, 502, err.Error())
		return
	}
	selected := relatedTaskIDs(tasks, taskID, related)
	if scopeLabel == "main_task" {
		for _, task := range tasks {
			selected[task.TaskID] = true
		}
	}
	allowedTeams := map[string]bool{team: true}
	for cr, effective := range names {
		if effective == team {
			allowedTeams[cr] = true
		}
	}
	validTasks := map[string]bool{}
	for _, task := range tasks {
		validTasks[task.TaskID] = true
	}
	bundle := taskTraceBundle{ProjectID: projectID, TaskID: taskID, TeamID: team, QueriedAt: queriedAt, Tasks: []projectTaskMeta{}, Traces: []taskTrace{}, Links: []traceLink{}, Evidence: []traceEvidence{}, Gaps: []string{}}
	visible := map[string]map[string]any{}
	selectedTraces := map[string]bool{}
	checkedOwners := map[string]bool{}
	for _, row := range rows {
		id := str(row["id"])
		owner := traceIdentity(row, "task")
		scopeMatches := traceIdentity(row, scopeLabel) == scopeID
		if scopeLabel == "main_task" && scopeRoom != "" && traceIdentity(row, "main_task") == "" && traceIdentity(row, "room") == scopeRoom {
			scopeMatches = true
		}
		if !traceIsVisible(row, workers) || !scopeMatches || !allowedTeams[traceIdentity(row, "team")] || !traceIDPattern.MatchString(id) {
			continue
		}
		// Replaced nodes leave the active DAG but retain their execution records.
		// Verify those records in the authorized storage scope, not from trace tags alone.
		if scopeLabel == "main_task" && owner != "" && !validTasks[owner] && !checkedOwners[owner] && isSafeTaskID(owner) {
			checkedOwners[owner] = true
			body, err := h.oss.GetObject(ctx, taskPrefix+owner+"/meta.json")
			var child teamTask
			if err == nil && json.Unmarshal(body, &child) == nil && child.TaskID == owner && child.TaskKind == "execution" && child.MainTaskID == scopeID && child.ProjectID == projectID && allowedTeams[child.TeamID] {
				validTasks[owner] = true
				selected[owner] = true
				tasks = append(tasks, child.projectTaskMeta)
			}
		}
		if owner != "" && !validTasks[owner] {
			continue
		}
		visible[id] = row
		if selected[owner] || (scopeLabel == "main_task" && owner == "") {
			selectedTraces[id] = true
		}
	}
	// Traverse only real causal references, separately from the task DAG.
	if related {
		for changed := true; changed; {
			changed = false
			for id, row := range visible {
				parent := traceField(row, "agentteams.upstream.trace_id")
				if parent == "" {
					continue
				}
				if selectedTraces[id] && visible[parent] != nil && !selectedTraces[parent] {
					selectedTraces[parent] = true
					changed = true
				}
				if selectedTraces[parent] && !selectedTraces[id] {
					selectedTraces[id] = true
					changed = true
				}
			}
		}
	}
	seenTasks := map[string]bool{}
	ids := make([]string, 0, len(selectedTraces))
	for id := range selectedTraces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	observationsByTrace, err := config.readTraceObservations(ctx, ids)
	if err != nil {
		httputil.WriteError(w, 502, err.Error())
		return
	}
	for _, id := range ids {
		row := visible[id]
		observations := observationsByTrace[id]
		for _, observation := range observations {
			if str(observation["traceId"]) != id {
				httputil.WriteError(w, 502, "Langfuse returned an observation from another trace")
				return
			}
			if str(observation["level"]) == "ERROR" || traceField(observation, "agentteams.outcome") == "error" || traceField(observation, "agentteams.outcome") == "cancelled" {
				bundle.Evidence = append(bundle.Evidence, traceEvidence{TraceID: id, SpanID: str(observation["id"]), Name: str(observation["name"]), Kind: "error", Observation: observation})
			} else if code, _ := strconv.Atoi(traceField(observation, "http.response.status_code")); code >= 400 {
				bundle.Evidence = append(bundle.Evidence, traceEvidence{TraceID: id, SpanID: str(observation["id"]), Name: str(observation["name"]), Kind: "http_error", Observation: observation})
			}
			if observation["endTime"] == nil {
				bundle.Gaps = append(bundle.Gaps, "unfinished_span:"+id+"/"+str(observation["id"]))
			}
			if traceField(observation, "agentteams.telemetry.capture_content") == "false" {
				bundle.Gaps = append(bundle.Gaps, "content_capture_disabled:"+id+"/"+str(observation["id"]))
			}
			for _, side := range []string{"input", "output"} {
				if traceField(observation, "agentteams.telemetry."+side+".capture_error") != "" {
					bundle.Gaps = append(bundle.Gaps, side+"_capture_failed:"+id+"/"+str(observation["id"]))
				}
				if content, ok := observation[side].(map[string]any); ok && content["truncated"] == true {
					bundle.Gaps = append(bundle.Gaps, side+"_truncated:"+id+"/"+str(observation["id"]))
				}
			}
		}
		if len(observations) == 0 {
			bundle.Gaps = append(bundle.Gaps, "no_observations:"+id)
		}
		owner := traceIdentity(row, "task")
		correlation := "task_metadata"
		if scopeLabel == "main_task" && owner == "" {
			owner = taskID
			correlation = "task_room"
			bundle.Gaps = append(bundle.Gaps, "task_identity_missing:"+id)
		}
		if owner == "" {
			bundle.Gaps = append(bundle.Gaps, "uncorrelated_trace:"+id)
		} else {
			seenTasks[owner] = true
			selected[owner] = true
		}
		if parent := traceField(row, "agentteams.upstream.trace_id"); parent != "" {
			bundle.Links = append(bundle.Links, traceLink{From: parent, To: id, SpanID: traceField(row, "agentteams.upstream.span_id"), Available: selectedTraces[parent]})
			if !selectedTraces[parent] {
				bundle.Gaps = append(bundle.Gaps, "upstream_trace_unavailable_or_not_requested:"+id)
			}
		}
		bundle.Traces = append(bundle.Traces, taskTrace{TaskID: owner, Correlation: correlation, Trace: row, Observations: observations})
	}
	for _, task := range tasks {
		if selected[task.TaskID] {
			bundle.Tasks = append(bundle.Tasks, task)
			if !seenTasks[task.TaskID] {
				bundle.Gaps = append(bundle.Gaps, "no_trace_for_task:"+task.TaskID)
			}
		}
	}
	bundle.Gaps = append(bundle.Gaps, "collection_completeness_not_proven:backend_snapshot_only")
	bundle.RetrievalComplete = true
	httputil.WriteJSON(w, http.StatusOK, bundle)
}
