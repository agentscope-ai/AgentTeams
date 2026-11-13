package resources

import (
	"encoding/json"
	"net/url"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

// This file is the table-driven authorization face of the three native
// routes (58 §3). The controller enforces the same facts in middleware
// (agentteams-controller internal/auth/authorizer.go:65-119) and in the
// list/get handlers (resource_handler.go:456-524 / :232-281); the service
// keeps them in one file so a handler never grows an inline role check.
//
// Frozen matrix (58 §3, production path):
//
//	role            create  list              get
//	admin/manager   ✅      ✅ all rows       ✅
//	human L2        ❌ 403   ✅ readable rows  ✅ in scope, else 404
//	human L3        ❌ 403   ✅ assigned only  ✅ assigned only, else 404 (MCP sanitized)
//	team-leader     ❌ 403   ✅ own team       ✅ own team, else 404
//	worker          ❌ 403   ❌ 403            ✅ self only, else 404
//
// A denial for get is rendered as 404 (never 403) so a scoped caller
// cannot probe worker existence (W8); the handler owns that status choice.

// CanCreate reports whether caller may create workers: admin/manager only.
// Every other role is denied 403 — including team-leader, whose production
// path is the authorizer 403 (resource_handler.go:212's 409 is the
// auth-disabled second line and is not part of the frozen G1 surface).
func CanCreate(c *identity.Identity) bool {
	if c == nil {
		return false
	}
	return c.Role == identity.RoleAdmin || c.Role == identity.RoleManager
}

// CanList reports whether caller may list workers. Workers have no list
// permission in the controller (authorizeWorkerSelfAction has no
// ActionList branch); every other role lists, with handler-side filtering.
func CanList(c *identity.Identity) bool {
	if c == nil {
		return false
	}
	switch c.Role {
	case identity.RoleAdmin, identity.RoleManager, identity.RoleHuman, identity.RoleTeamLeader:
		return true
	default: // identity.RoleWorker and anything unknown
		return false
	}
}

// CanRead reports whether caller may read this worker.
//
//   - admin/manager: all;
//   - worker: itself only (identity.WorkerName == row name);
//   - team-leader: own team only;
//   - human L2: WorkerReadable (accessibleTeams / explicit assignment);
//   - human L3 (worker-scoped): only the explicitly assigned workers — the
//     team fallback is deliberately NOT used, so an assignment-scoped
//     identity cannot inherit a whole team (58 §3 L3 row).
func CanRead(c *identity.Identity, w *Worker) bool {
	if c == nil || w == nil {
		return false
	}
	switch c.Role {
	case identity.RoleAdmin, identity.RoleManager:
		return true
	case identity.RoleWorker:
		return c.WorkerName == w.Name
	case identity.RoleTeamLeader:
		return c.TeamMatches(w.TeamSlug)
	case identity.RoleHuman:
		if c.IsWorkerScoped() {
			return containsWorkerName(c.AccessibleWorkers, w.Name)
		}
		return c.WorkerReadable(w.TeamSlug, w.Name)
	default:
		return false
	}
}

// FilterList keeps only the rows caller may read, preserving store order
// (name ascending). It never returns nil: an empty result encodes as [].
func FilterList(c *identity.Identity, rows []*Worker) []*Worker {
	out := make([]*Worker, 0, len(rows))
	for _, w := range rows {
		if CanRead(c, w) {
			out = append(out, w)
		}
	}
	return out
}

// SanitizeMCPForL3 scrubs credential-bearing MCP endpoint URLs from a
// response before it is served to an L3 (worker-scoped) human
// (resource_handler.go:1877-1914). Every URL is reduced to
// scheme://host[:port]/path: userinfo, query and fragment are dropped
// wholesale (MCP URLs are arbitrary external input, so a credential
// denylist cannot be complete).
//
// The service stores mcpServers as raw JSON; a value that cannot be parsed
// as []MCPServer is dropped entirely (fail closed) rather than forwarded
// unscrubbed. Untouched entries keep the stored bytes, so clean URLs remain
// byte-identical.
func SanitizeMCPForL3(resp *WorkerResponse) {
	if resp == nil || len(resp.McpServers) == 0 {
		return
	}
	var servers []mcpServerJSON
	if err := json.Unmarshal(resp.McpServers, &servers); err != nil {
		resp.McpServers = nil
		return
	}
	changed := false
	for i := range servers {
		clean := sanitizeMCPURLForL3(servers[i].URL)
		if clean != servers[i].URL {
			servers[i].URL = clean
			changed = true
		}
	}
	if !changed {
		return
	}
	out, err := json.Marshal(servers)
	if err != nil {
		resp.McpServers = nil
		return
	}
	resp.McpServers = out
}

// mcpServerJSON mirrors agentteams-controller api/v1beta1.MCPServer
// (types.go:111-115): field order is the controller's marshal order, so a
// re-marshal after sanitizing stays controller-compatible.
type mcpServerJSON struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Transport string `json:"transport,omitempty"`
}

// sanitizeMCPURLForL3 is a verbatim port of
// resource_handler.go:1898-1914: a URL without userinfo/query/fragment is
// returned byte-identical; otherwise those components are removed. A URL
// that cannot be parsed, is not absolute, or has no host is omitted ("").
func sanitizeMCPURLForL3(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return ""
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

// containsWorkerName reports whether list contains the worker name.
func containsWorkerName(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
