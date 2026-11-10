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

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

type teamTask struct {
	projectTaskMeta
	ProjectID string `json:"project_id"`
	TeamID    string `json:"team_id"`
}

type taskTrace struct {
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
	ProjectID         string            `json:"project_id"`
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
	for page := 1; ; page++ {
		params.Set("page", strconv.Itoa(page))
		var result struct {
			Data []map[string]any `json:"data"`
			Meta struct {
				TotalPages *int `json:"totalPages"`
			} `json:"meta"`
		}
		if err := config.get(ctx, resource+"?"+params.Encode(), &result); err != nil {
			return nil, err
		}
		if result.Meta.TotalPages == nil || *result.Meta.TotalPages < 0 {
			return nil, fmt.Errorf("invalid Langfuse pagination response")
		}
		rows = append(rows, result.Data...)
		if page >= *result.Meta.TotalPages {
			return rows, nil
		}
		if page >= 1000 {
			return nil, fmt.Errorf("Langfuse query exceeds 1000 pages; no complete result available")
		}
	}
}

func (config langfuseQuery) get(ctx context.Context, resource string, result any) error {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	config, err := h.langfuseConfiguration(r.Context())
	if err != nil {
		httputil.WriteError(w, 503, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	workers, err := h.traceWorkers(ctx, caller)
	if err != nil {
		httputil.WriteError(w, 503, "cannot verify trace resource scope")
		return
	}
	// Query the whole project, without a time cutoff, so late spans, retries,
	// old rounds and reverse causal references are discoverable.
	rows, err := config.readPages(ctx, "traces", url.Values{"tags": {"agentteams", "project:" + projectID}, "filter": {traceWorkerFilter(workers)}})
	if err != nil {
		httputil.WriteError(w, 502, err.Error())
		return
	}
	selected := relatedTaskIDs(tasks, taskID, related == "true")
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
	bundle := taskTraceBundle{ProjectID: projectID, TaskID: taskID, TeamID: team, QueriedAt: time.Now().UTC().Format(time.RFC3339Nano), Tasks: []projectTaskMeta{}, Traces: []taskTrace{}, Links: []traceLink{}, Evidence: []traceEvidence{}, Gaps: []string{}}
	visible := map[string]map[string]any{}
	selectedTraces := map[string]bool{}
	for _, row := range rows {
		id := str(row["id"])
		owner := traceIdentity(row, "task")
		if !traceIsVisible(row, workers) || traceIdentity(row, "project") != projectID || !allowedTeams[traceIdentity(row, "team")] || !traceIDPattern.MatchString(id) || (owner != "" && !validTasks[owner]) {
			continue
		}
		visible[id] = row
		if selected[owner] {
			selectedTraces[id] = true
		}
	}
	// Traverse only real causal references, separately from the task DAG.
	if related == "true" {
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
	for _, id := range ids {
		row := visible[id]
		observations, err := config.readPages(ctx, "observations", url.Values{"traceId": {id}})
		if err != nil {
			httputil.WriteError(w, 502, err.Error())
			return
		}
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
		bundle.Traces = append(bundle.Traces, taskTrace{Trace: row, Observations: observations})
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
