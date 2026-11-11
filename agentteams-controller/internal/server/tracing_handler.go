package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var traceIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)

type traceWorkerAccess struct {
	RuntimeName string
	Teams       map[string]bool
}

// traceWorkers verifies current Worker/Team membership before returning data.
func (h *ProjectHandler) traceWorkers(ctx context.Context, caller *authpkg.CallerIdentity) (map[string]traceWorkerAccess, error) {
	var workers v1beta1.WorkerList
	if err := h.client.List(ctx, &workers, client.InNamespace(h.namespace)); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(workers.Items))
	for _, worker := range workers.Items {
		names = append(names, worker.Name)
	}
	index, err := teamMemberIndex(ctx, h.client, h.namespace, names)
	if err != nil {
		return nil, err
	}
	allowed := map[string]traceWorkerAccess{}
	explicitWorkers := map[string]bool{}
	for _, name := range caller.AccessibleWorkers {
		explicitWorkers[name] = true
	}
	explicitScope := caller.Role == authpkg.RoleHuman && len(explicitWorkers) > 0
	for _, worker := range workers.Items {
		if explicitScope && !explicitWorkers[worker.Name] {
			continue
		}
		teams := map[string]bool{"": true}
		if hit, ok := index[worker.Name]; ok {
			teams = map[string]bool{hit.team.Name: true, hit.team.Spec.EffectiveTeamName(hit.team.Name): true}
			if caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleManager && !explicitScope && !caller.TeamMatches(hit.team.Name) {
				continue
			}
		} else if caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleManager && !explicitScope {
			continue
		}
		identity := worker.Spec.EffectiveWorkerName(worker.Name)
		access := traceWorkerAccess{RuntimeName: identity, Teams: teams}
		allowed[worker.Name], allowed[identity] = access, access
	}
	return allowed, nil
}

func traceIsVisible(row map[string]any, workers map[string]traceWorkerAccess) bool {
	worker := str(row["userId"])
	if identity := traceField(row, "agentteams.worker.name"); identity != "" && identity != worker {
		return false
	}
	return workers[worker].Teams[traceIdentity(row, "team")]
}

func traceWorkerFilter(workers map[string]traceWorkerAccess) string {
	seen := map[string]bool{}
	names := []string{}
	for _, worker := range workers {
		if !seen[worker.RuntimeName] {
			names = append(names, worker.RuntimeName)
			seen[worker.RuntimeName] = true
		}
	}
	filter, _ := json.Marshal([]map[string]any{{"type": "stringOptions", "column": "userId", "operator": "any of", "value": names}})
	return string(filter)
}

func traceReader(w http.ResponseWriter, r *http.Request) *authpkg.CallerIdentity {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, 401, "authentication required")
		return nil
	}
	if caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleManager && caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleHuman {
		httputil.WriteError(w, 403, "role cannot read traces")
		return nil
	}
	return caller
}

func (h *ProjectHandler) ListTraces(w http.ResponseWriter, r *http.Request) {
	caller := traceReader(w, r)
	if caller == nil {
		return
	}
	query := r.URL.Query()
	if agent := query.Get("agent"); agent != "" {
		if worker := query.Get("worker"); worker != "" && worker != agent {
			httputil.WriteError(w, 400, "agent and worker filters disagree")
			return
		}
		query.Set("worker", agent)
	}
	limit := 20
	if query.Has("limit") {
		value, err := strconv.Atoi(query.Get("limit"))
		if err != nil || value < 1 || value > 100 {
			httputil.WriteError(w, 400, "limit must be 1..100")
			return
		}
		limit = value
	}
	var since, until time.Time
	for key, target := range map[string]*time.Time{"since": &since, "until": &until} {
		if query.Has(key) {
			value, err := time.Parse(time.RFC3339Nano, query.Get(key))
			if err != nil {
				httputil.WriteError(w, 400, key+" must be RFC3339 with a timezone")
				return
			}
			*target = value
		}
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		httputil.WriteError(w, 400, "since must precede until")
		return
	}
	page := 1
	if query.Has("page") {
		value, err := strconv.Atoi(query.Get("page"))
		if err != nil || value < 1 || value > 99999 {
			httputil.WriteError(w, 400, "invalid page")
			return
		}
		page = value
	}
	hours := 24
	if query.Has("hours") {
		value, err := strconv.Atoi(query.Get("hours"))
		if err != nil || (value != 1 && value != 24 && value != 168) {
			httputil.WriteError(w, 400, "invalid time range")
			return
		}
		hours = value
	}
	workers, err := h.traceWorkers(r.Context(), caller)
	if err != nil {
		httputil.WriteError(w, 503, "cannot verify trace resource scope")
		return
	}
	if worker := query.Get("worker"); worker != "" && workers[worker].Teams == nil {
		httputil.WriteError(w, 404, "resource not found")
		return
	}
	params := url.Values{"tags": {"agentteams", "worker-turn"}, "orderBy": {"timestamp.desc"}}
	params.Set("filter", traceWorkerFilter(workers))
	if session := query.Get("session"); session != "" {
		params.Set("sessionId", session)
	} else if since.IsZero() && until.IsZero() {
		params.Set("fromTimestamp", time.Now().Add(-time.Duration(hours)*time.Hour).UTC().Format(time.RFC3339Nano))
	}
	if !since.IsZero() {
		params.Set("fromTimestamp", since.UTC().Format(time.RFC3339Nano))
	}
	if !until.IsZero() {
		params.Set("toTimestamp", until.UTC().Format(time.RFC3339Nano))
	}
	if worker := query.Get("worker"); worker != "" {
		params.Set("userId", workers[worker].RuntimeName)
	}
	for _, label := range []string{"task", "team", "project", "room"} {
		if value := query.Get(label); value != "" {
			params.Add("tags", label+":"+value)
		}
	}
	if runtime := query.Get("runtime"); runtime != "" {
		params.Add("tags", runtime)
	}
	visible := make([]map[string]any, 0)
	if len(workers) > 0 {
		config, err := h.langfuseConfiguration(r.Context())
		if err != nil {
			httputil.WriteError(w, 503, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		rows, err := config.readPages(ctx, "traces", params)
		if err != nil {
			httputil.WriteError(w, 502, err.Error())
			return
		}
		for _, row := range rows {
			if !traceIsVisible(row, workers) {
				continue
			}
			if !since.IsZero() || !until.IsZero() {
				at, err := time.Parse(time.RFC3339Nano, str(row["timestamp"]))
				if err != nil || (!since.IsZero() && at.Before(since)) || (!until.IsZero() && !at.Before(until)) {
					continue
				}
			}
			if runtime := query.Get("runtime"); runtime != "" {
				actual := traceField(row, "agentteams.runtime")
				if actual == "" {
					tags, _ := row["tags"].([]any)
					for _, tag := range tags {
						if str(tag) == runtime {
							actual = runtime
						}
					}
				}
				if actual != runtime {
					continue
				}
			}
			if selected := query.Get("worker"); selected != "" && str(row["userId"]) != workers[selected].RuntimeName {
				continue
			}
			match := true
			for _, label := range []string{"task", "team", "project", "room"} {
				if value := query.Get(label); value != "" && traceIdentity(row, label) != value {
					match = false
				}
			}
			if query.Get("session") != "" && str(row["sessionId"]) != query.Get("session") {
				match = false
			}
			if !match {
				continue
			}
			item := map[string]any{}
			for _, key := range []string{"id", "name", "timestamp", "userId", "sessionId", "tags", "metadata", "latency", "totalCost"} {
				item[key] = row[key]
			}
			visible = append(visible, item)
		}
	}
	start := (page - 1) * limit
	if start > len(visible) {
		start = len(visible)
	}
	end := start + limit
	if end > len(visible) {
		end = len(visible)
	}
	httputil.WriteJSON(w, 200, map[string]any{"data": visible[start:end], "meta": map[string]int{"page": page, "totalItems": len(visible), "totalPages": (len(visible) + limit - 1) / limit}})
}

func (h *ProjectHandler) GetTrace(w http.ResponseWriter, r *http.Request) {
	caller := traceReader(w, r)
	if caller == nil {
		return
	}
	id := r.PathValue("traceId")
	if !traceIDPattern.MatchString(id) {
		httputil.WriteError(w, 400, "invalid trace id")
		return
	}
	workers, err := h.traceWorkers(r.Context(), caller)
	if err != nil {
		httputil.WriteError(w, 503, "cannot verify trace resource scope")
		return
	}
	config, err := h.langfuseConfiguration(r.Context())
	if err != nil {
		httputil.WriteError(w, 503, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	row := map[string]any{}
	if err := config.get(ctx, "traces/"+id, &row); err != nil {
		httputil.WriteError(w, 502, err.Error())
		return
	}
	if str(row["id"]) != id || !traceIsVisible(row, workers) {
		httputil.WriteError(w, 404, "trace not found or not authorized")
		return
	}
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
		metadata, _ := observation["metadata"].(map[string]any)
		attributes, _ := metadata["attributes"].(map[string]any)
		flattened := map[string]any{}
		for key, value := range attributes {
			flattened[key] = value
		}
		for key, value := range metadata {
			flattened[key] = value
		}
		observation["metadata"] = flattened
	}
	row["observations"], row["backend"] = observations, "langfuse"
	row["backendUrl"], row["langfuseUrl"] = nil, nil
	if caller.Role == authpkg.RoleAdmin {
		settings, _, err := h.readObservabilityConfig(ctx)
		if err != nil {
			httputil.WriteError(w, 503, "cannot read observability configuration")
			return
		}
		base := str(settings["publicUrl"])
		if base == "" {
			base = config.LangfuseURL
		}
		if project := str(settings["projectId"]); project != "" {
			link := fmt.Sprintf("%s/project/%s/traces/%s", base, url.PathEscape(project), id)
			row["backendUrl"], row["langfuseUrl"] = link, link
		}
	}
	httputil.WriteJSON(w, 200, row)
}
