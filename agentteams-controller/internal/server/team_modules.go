package server

import (
	"encoding/json"
	"net/http"
	"sort"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// teamModuleMember is one member's installed state for a module.
type teamModuleMember struct {
	Worker  string `json:"worker"`
	Version string `json:"version,omitempty"`
	Loaded  bool   `json:"loaded"`
}

// teamModuleView is one module aggregated over the team's workers.
type teamModuleView struct {
	Module     string             `json:"module"`
	Name       string             `json:"name,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	Versions   map[string]int     `json:"versions"`
	Members    []teamModuleMember `json:"members"`
	Consistent bool               `json:"consistent"`
}

// teamModuleError reports a member whose registry could not be read.
type teamModuleError struct {
	Worker string `json:"worker"`
	Error  string `json:"error"`
}

type teamModulesResponse struct {
	Team    string            `json:"team"`
	Modules []teamModuleView  `json:"modules"`
	Errors  []teamModuleError `json:"errors,omitempty"`
}

// GetTeamModules returns the read-only handler for
//
//	GET /api/v1/teams/{name}/modules
//
// It expands the team's workerMembers and aggregates each member's
// qwenpaw plugin registry into one row per module id (versions by count,
// per-member installed version, consistent flag). Member failures do not
// abort the response: they are reported under errors and the remaining
// members are still aggregated. Scoping follows GetTeam (W8: out-of-scope
// callers see 404).
func (h *ResourceHandler) GetTeamModules(kubeMode, containerPrefix string) http.HandlerFunc {
	return h.getTeamModulesWithProxy(newWorkerPluginsProxy(h, kubeMode, containerPrefix), kubeMode)
}

func (h *ResourceHandler) getTeamModulesWithProxy(p *workerPluginsProxy, kubeMode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" || !workerNamePattern.MatchString(name) {
			httputil.WriteError(w, http.StatusBadRequest, "team name is required and must be a valid DNS label")
			return
		}
		if kubeMode != "embedded" {
			httputil.WriteError(w, http.StatusServiceUnavailable, "team module aggregation requires embedded mode")
			return
		}

		var team v1beta1.Team
		if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &team); err != nil {
			if apierrors.IsNotFound(err) {
				httputil.WriteError(w, http.StatusNotFound, "team not found")
				return
			}
			writeK8sError(w, "get team modules", err)
			return
		}
		if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
			(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
			!caller.TeamMatches(name) {
			httputil.WriteError(w, http.StatusNotFound, "team not found")
			return
		}

		modules, memberErrors := h.aggregateTeamModules(r, &team, p)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(teamModulesResponse{Team: name, Modules: modules, Errors: memberErrors})
	}
}

// collectTeamMemberEntries reads every member's plugin registry, keeping
// member failures separate so the aggregate can still report the rest.
func (h *ResourceHandler) collectTeamMemberEntries(r *http.Request, team *v1beta1.Team, p *workerPluginsProxy) (map[string][]map[string]any, []teamModuleError) {
	entries := make(map[string][]map[string]any, len(team.Spec.WorkerMembers))
	var memberErrors []teamModuleError
	for _, member := range team.Spec.WorkerMembers {
		var worker v1beta1.Worker
		if err := h.client.Get(r.Context(), client.ObjectKey{Name: member.Name, Namespace: h.namespace}, &worker); err != nil {
			memberErrors = append(memberErrors, teamModuleError{Worker: member.Name, Error: "worker not found"})
			continue
		}
		if rt := worker.Spec.Runtime; rt != "" && rt != "qwenpaw" {
			memberErrors = append(memberErrors, teamModuleError{Worker: member.Name, Error: "worker runtime is not qwenpaw"})
			continue
		}
		base := p.workerBaseURL(worker.Spec.EffectiveWorkerName(worker.Name), worker.Spec.Env)
		list, ferr := p.fetchProjection(r.Context(), base)
		if ferr != nil {
			memberErrors = append(memberErrors, teamModuleError{Worker: member.Name, Error: ferr.message})
			continue
		}
		entries[member.Name] = list
	}
	return entries, memberErrors
}

// aggregateTeamModules groups the collected entries by module id.
func (h *ResourceHandler) aggregateTeamModules(r *http.Request, team *v1beta1.Team, p *workerPluginsProxy) ([]teamModuleView, []teamModuleError) {
	entries, memberErrors := h.collectTeamMemberEntries(r, team, p)

	modules := make(map[string]*teamModuleView)
	for member, list := range entries {
		for _, entry := range list {
			id := pluginEntryID(entry)
			if id == "" {
				continue
			}
			view, ok := modules[id]
			if !ok {
				view = &teamModuleView{Module: id, Versions: map[string]int{}, Members: []teamModuleMember{}}
				if n, ok := entry["name"].(string); ok {
					view.Name = n
				}
				if k, ok := entry["plugin_type"].(string); ok {
					view.Kind = k
				}
				modules[id] = view
			}
			version, _ := entry["version"].(string)
			loaded, _ := entry["loaded"].(bool)
			view.Versions[version]++
			view.Members = append(view.Members, teamModuleMember{Worker: member, Version: version, Loaded: loaded})
		}
	}

	out := make([]teamModuleView, 0, len(modules))
	expected := len(entries)
	for _, view := range modules {
		sort.Slice(view.Members, func(i, j int) bool { return view.Members[i].Worker < view.Members[j].Worker })
		view.Consistent = len(view.Versions) == 1 && len(view.Members) == expected && len(memberErrors) == 0
		for _, m := range view.Members {
			if !m.Loaded {
				view.Consistent = false
			}
		}
		out = append(out, *view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out, memberErrors
}

// aggregateTeamModule isolates one module from the aggregate (used by the
// install response so the caller sees the post-install consistency).
func (h *ResourceHandler) aggregateTeamModule(r *http.Request, team *v1beta1.Team, module, pluginID string, p *workerPluginsProxy) (teamModuleView, bool, []teamModuleError) {
	modules, memberErrors := h.aggregateTeamModules(r, team, p)
	for _, m := range modules {
		if m.Module == module || (pluginID != "" && m.Module == pluginID) {
			return m, true, memberErrors
		}
	}
	return teamModuleView{Module: module, Versions: map[string]int{}, Members: []teamModuleMember{}}, false, memberErrors
}

func pluginEntryID(entry map[string]any) string {
	if id, _ := entry["id"].(string); id != "" {
		return id
	}
	if name, _ := entry["name"].(string); name != "" {
		return name
	}
	return ""
}
