package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mcpRegistryPrefix is the deployment-wide shared MCP registry maintained by
// the dashboard's MCP servers flow (one JSON document per server,
// mcp-servers/<name>.json). Reading it list-on-read gives the catalog its
// "shared" half; the per-worker half comes from Worker spec.mcpServers.
const mcpRegistryPrefix = "mcp-servers/"

// mcpRegistryEntry is the dashboard's McpServerConfig shape. Headers carry
// credentials and are read for validation only — never echoed in the
// catalog response (secret contract: URL redacted to host/path only).
type mcpRegistryEntry struct {
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Transport string            `json:"transport"`
	Timeout   int               `json:"timeout"`
	Trusted   bool              `json:"trusted"`
	Headers   map[string]string `json:"headers"`
}

// MCPWorkerRef is one worker that references a catalog entry via
// spec.mcpServers.
type MCPWorkerRef struct {
	Name string `json:"name"`
	Team string `json:"team,omitempty"`
}

// MCPInfo is one entry of the read-only deployment MCP catalog
// (GET /api/v1/mcp-servers). It carries identity/availability only — never
// URL credentials, registry headers, or tool listings.
//
// Trusted semantics (v1):
//   - worker-spec and registry+worker-spec entries are trusted — a
//     controller-managed spec reference means the server sits on the
//     deployment gateway's bearer-injected path;
//   - registry-only entries are trusted when the registry document carries
//     "trusted": true (set by the gateway-wiring flow); direct external
//     URLs stay untrusted: listed and flagged, never auto-applied.
type MCPInfo struct {
	Name      string         `json:"name"`
	Source    string         `json:"source"` // "registry" | "worker-spec" | "registry+worker-spec"
	Transport string         `json:"transport,omitempty"`
	URL       string         `json:"url,omitempty"` // redacted: host/path only
	Timeout   int            `json:"timeout,omitempty"`
	Trusted   bool           `json:"trusted"`
	Workers   []MCPWorkerRef `json:"workers,omitempty"`
}

// MCPListResponse is the payload of GET /api/v1/mcp-servers.
type MCPListResponse struct {
	Servers           []MCPInfo `json:"servers"`
	Total             int       `json:"total"`
	RegistryAvailable bool      `json:"registry_available"`
}

// ListMCPServers serves the read-only deployment MCP catalog:
//
//	GET /api/v1/mcp-servers
//
// Sources (merged and deduplicated by server name, the mcporter client
// key):
//  1. the deployment-wide shared MCP registry (object storage,
//     mcp-servers/<name>.json) — "what exists";
//  2. per-worker spec.mcpServers — the assignment view (who is wired to
//     what), reported in workers[] so the catalog × worker matrix is
//     derivable without per-worker calls.
//
// Scope: admin (L1) sees the full merged catalog; team leaders and L2
// humans see only entries referenced by workers in the teams they control
// (registry entries no in-scope worker references are invisible to them).
// Managers and workers are denied — the catalog is an admin/L2 workbench
// surface, following the skill catalog precedent. Read-only: v1 has no
// write API; registry writes go through the existing dashboard / skill
// flows, spec writes through PUT /workers.
func (h *ResourceHandler) ListMCPServers(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	switch caller.Role {
	case authpkg.RoleAdmin, authpkg.RoleHuman, authpkg.RoleTeamLeader:
	default:
		// Manager and worker roles are denied even though the authorizer
		// grants managers broad read access: the deployment MCP catalog is
		// a workbench surface (admin + L2 humans), matching the skill
		// catalog handler gate.
		httputil.WriteError(w, http.StatusForbidden, "role not allowed to read the MCP catalog")
		return
	}
	scoped := caller.Role == authpkg.RoleHuman || caller.Role == authpkg.RoleTeamLeader

	ctx := r.Context()

	// --- Per-worker half: spec.mcpServers across all workers ---
	var workers v1beta1.WorkerList
	if err := h.client.List(ctx, &workers, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list mcp servers: list workers", err)
		return
	}
	// name -> workers referencing it; name -> worker-spec entry.
	referencedBy := map[string][]MCPWorkerRef{}
	specEntries := map[string]v1beta1.MCPServer{}
	for i := range workers.Items {
		wkr := &workers.Items[i]
		team := ""
		if t, _, ok, err := findTeamMember(ctx, h.client, h.namespace, wkr.Name); err != nil {
			writeK8sError(w, "list mcp servers: lookup team member", err)
			return
		} else if ok {
			team = t.Name
		}
		// Scoped readers only see workers in the teams they control.
		if scoped && !caller.TeamMatches(team) {
			continue
		}
		for _, m := range wkr.Spec.McpServers {
			if m.Name == "" {
				continue
			}
			referencedBy[m.Name] = append(referencedBy[m.Name], MCPWorkerRef{Name: wkr.Name, Team: team})
			if _, exists := specEntries[m.Name]; !exists {
				specEntries[m.Name] = m
			}
		}
	}

	// --- Shared half: the deployment-wide registry (list-on-read) ---
	registryAvailable := h.oss != nil
	regEntries := map[string]mcpRegistryEntry{}
	if registryAvailable {
		keys, err := h.oss.ListObjects(ctx, mcpRegistryPrefix)
		if err != nil {
			writeK8sError(w, "list mcp servers: list registry", err)
			return
		}
		for _, key := range keys {
			base := strings.TrimPrefix(key, mcpRegistryPrefix)
			if !strings.HasSuffix(base, ".json") || base == "" || strings.Contains(base, "/") {
				continue
			}
			name := strings.TrimSuffix(base, ".json")
			if name == "" {
				continue
			}
			// ListObjects returns names RELATIVE to the prefix (the production
			// MinIOClient wraps `mc ls`, which prints the bare child name);
			// GetObject needs the full key, so re-attach the registry prefix.
			// Reading the listed name as-is would fetch the bucket root and
			// silently drop every registry-only entry.
			data, err := h.oss.GetObject(ctx, mcpRegistryPrefix+base)
			if err != nil {
				// A unreadable registry document must not fail the whole
				// catalog; skip it (the entry simply does not appear).
				continue
			}
			var e mcpRegistryEntry
			if err := json.Unmarshal(data, &e); err != nil {
				continue
			}
			if e.Name == "" {
				e.Name = name
			}
			regEntries[e.Name] = e
		}
	}

	// --- Merge by name ---
	names := map[string]bool{}
	for n := range regEntries {
		names[n] = true
	}
	for n := range specEntries {
		names[n] = true
	}

	servers := make([]MCPInfo, 0, len(names))
	for name := range names {
		reg, inReg := regEntries[name]
		spec, inSpec := specEntries[name]

		info := MCPInfo{Name: name, Workers: referencedBy[name]}
		switch {
		case inReg && inSpec:
			info.Source = "registry+worker-spec"
			info.Transport = reg.Transport
			info.URL = redactMCPURL(reg.URL)
			info.Timeout = reg.Timeout
			info.Trusted = true // a worker is officially wired: gateway bearer path
		case inReg:
			info.Source = "registry"
			info.Transport = reg.Transport
			info.URL = redactMCPURL(reg.URL)
			info.Timeout = reg.Timeout
			info.Trusted = reg.Trusted
		default:
			info.Source = "worker-spec"
			info.Transport = spec.Transport
			info.URL = redactMCPURL(spec.URL)
			info.Trusted = true // controller-managed spec reference
		}
		// Scoped readers: only entries referenced by an in-scope worker.
		if scoped && len(info.Workers) == 0 {
			continue
		}
		servers = append(servers, info)
	}

	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	httputil.WriteJSON(w, http.StatusOK, MCPListResponse{
		Servers:           servers,
		Total:             len(servers),
		RegistryAvailable: registryAvailable,
	})
}

// redactMCPURL applies the secret contract: only host/path are exposed.
// Scheme, userinfo, query (consumer keys) and fragment are stripped.
// Unparseable URLs are redacted to empty rather than echoed raw.
func redactMCPURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	return u.Host + p
}
