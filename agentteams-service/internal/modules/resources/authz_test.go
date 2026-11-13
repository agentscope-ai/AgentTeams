package resources

import (
	"encoding/json"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

func hAuthzWorker(name, team string) *Worker {
	return &Worker{Name: name, WorkerName: name + "-display", TeamSlug: team}
}

func TestCanCreate(t *testing.T) {
	tests := []struct {
		id   *identity.Identity
		want bool
	}{
		{&identity.Identity{Role: identity.RoleAdmin}, true},
		{&identity.Identity{Role: identity.RoleManager}, true},
		{&identity.Identity{Role: identity.RoleHuman}, false},
		{&identity.Identity{Role: identity.RoleTeamLeader, Team: "t"}, false},
		{&identity.Identity{Role: identity.RoleWorker}, false},
		{&identity.Identity{Role: "unknown"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := CanCreate(tt.id); got != tt.want {
			t.Fatalf("CanCreate(%+v) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestCanList(t *testing.T) {
	tests := []struct {
		id   *identity.Identity
		want bool
	}{
		{&identity.Identity{Role: identity.RoleAdmin}, true},
		{&identity.Identity{Role: identity.RoleManager}, true},
		{&identity.Identity{Role: identity.RoleHuman}, true},
		{&identity.Identity{Role: identity.RoleTeamLeader, Team: "t"}, true},
		{&identity.Identity{Role: identity.RoleWorker}, false},
		{&identity.Identity{Role: "unknown"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := CanList(tt.id); got != tt.want {
			t.Fatalf("CanList(%+v) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestCanRead(t *testing.T) {
	w1 := hAuthzWorker("w1", "team-a")
	w2 := hAuthzWorker("w2", "team-b")

	tests := []struct {
		name string
		id   *identity.Identity
		w    *Worker
		want bool
	}{
		{"admin", &identity.Identity{Role: identity.RoleAdmin}, w2, true},
		{"manager", &identity.Identity{Role: identity.RoleManager}, w2, true},
		{"leader own team", &identity.Identity{Role: identity.RoleTeamLeader, Team: "team-a"}, w1, true},
		{"leader other team", &identity.Identity{Role: identity.RoleTeamLeader, Team: "team-a"}, w2, false},
		{"leader Teams set", &identity.Identity{Role: identity.RoleTeamLeader, Teams: []string{"team-b"}}, w2, true},
		{"leader no team", &identity.Identity{Role: identity.RoleTeamLeader}, w1, false},
		{"human L2 in team", &identity.Identity{Role: identity.RoleHuman, Teams: []string{"team-a"}}, w1, true},
		{"human L2 other team", &identity.Identity{Role: identity.RoleHuman, Teams: []string{"team-a"}}, w2, false},
		{"human L3 assigned", &identity.Identity{Role: identity.RoleHuman, AccessibleWorkers: []string{"w2"}}, w2, true},
		{"human L3 not assigned", &identity.Identity{Role: identity.RoleHuman, AccessibleWorkers: []string{"w2"}}, w1, false},
		// L3 assignment scope wins over a matching team: an assignment-scoped
		// human must not inherit a whole team (58 §3 L3 row).
		{"human L3 team fallback suppressed", &identity.Identity{Role: identity.RoleHuman, Teams: []string{"team-a"}, AccessibleWorkers: []string{"w2"}}, w1, false},
		{"worker self", &identity.Identity{Role: identity.RoleWorker, WorkerName: "w1"}, w1, true},
		{"worker other", &identity.Identity{Role: identity.RoleWorker, WorkerName: "w1"}, w2, false},
		{"unknown role", &identity.Identity{Role: "robot"}, w1, false},
		{"nil identity", nil, w1, false},
		{"nil worker", &identity.Identity{Role: identity.RoleAdmin}, nil, false},
	}
	for _, tt := range tests {
		if got := CanRead(tt.id, tt.w); got != tt.want {
			t.Fatalf("%s: CanRead = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestFilterList(t *testing.T) {
	rows := []*Worker{
		hAuthzWorker("w1", "team-a"),
		hAuthzWorker("w2", "team-b"),
		hAuthzWorker("w3", "team-a"),
	}

	human := &identity.Identity{Role: identity.RoleHuman, Teams: []string{"team-a"}}
	got := FilterList(human, rows)
	if len(got) != 2 || got[0].Name != "w1" || got[1].Name != "w3" {
		t.Fatalf("FilterList(human) = %v", hWorkerNames(got))
	}

	admin := &identity.Identity{Role: identity.RoleAdmin}
	if got := FilterList(admin, rows); len(got) != 3 {
		t.Fatalf("FilterList(admin) = %v", hWorkerNames(got))
	}

	// Empty result stays non-nil (the list envelope must encode []).
	worker := &identity.Identity{Role: identity.RoleWorker, WorkerName: "nope"}
	empty := FilterList(worker, rows)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("FilterList(worker) = %#v, want non-nil empty slice", empty)
	}
}

func hWorkerNames(rows []*Worker) []string {
	out := make([]string, 0, len(rows))
	for _, w := range rows {
		out = append(out, w.Name)
	}
	return out
}

func TestSanitizeMCPURLForL3(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"https://mcp.example.com/mcp", "https://mcp.example.com/mcp"},
		{"https://user:pass@mcp.example.com/mcp", "https://mcp.example.com/mcp"},
		{"https://mcp.example.com/mcp?api_key=secret", "https://mcp.example.com/mcp"},
		{"https://user:pass@mcp.example.com/mcp?api_key=secret#fragment", "https://mcp.example.com/mcp"},
		{"https://mcp.example.com/mcp#fragment", "https://mcp.example.com/mcp"},
		{"not-a-url", ""},
		{"/relative/path?x=1", ""},
		{"http://", ""},
	}
	for _, tt := range tests {
		if got := sanitizeMCPURLForL3(tt.in); got != tt.want {
			t.Fatalf("sanitizeMCPURLForL3(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeMCPForL3(t *testing.T) {
	clean := json.RawMessage(`[{"name":"clean","url":"https://clean.example.com/mcp"}]`)
	resp := &WorkerResponse{McpServers: clean}
	SanitizeMCPForL3(resp)
	if string(resp.McpServers) != string(clean) {
		t.Fatalf("clean MCP bytes must stay identical, got %s", resp.McpServers)
	}

	dirty := json.RawMessage(`[{"name":"a","url":"https://user:pw@mcp.example.com/x?api_key=secret#frag","transport":"http"},{"name":"b","url":"https://clean.example.com/mcp"},{"name":"c","url":"/relative/path"}]`)
	resp = &WorkerResponse{McpServers: dirty}
	SanitizeMCPForL3(resp)

	var servers []mcpServerJSON
	if err := json.Unmarshal(resp.McpServers, &servers); err != nil {
		t.Fatalf("sanitized MCP is not JSON: %v (%s)", err, resp.McpServers)
	}
	if len(servers) != 3 {
		t.Fatalf("sanitized servers = %+v", servers)
	}
	if servers[0].URL != "https://mcp.example.com/x" {
		t.Fatalf("query/userinfo/fragment not stripped: %q", servers[0].URL)
	}
	if servers[1].URL != "https://clean.example.com/mcp" {
		t.Fatalf("clean URL changed: %q", servers[1].URL)
	}
	if servers[2].URL != "" {
		t.Fatalf("relative URL must fail closed to empty, got %q", servers[2].URL)
	}
	if servers[0].Name != "a" || servers[0].Transport != "http" {
		t.Fatalf("non-URL fields damaged: %+v", servers[0])
	}

	// Malformed raw JSON fails closed instead of leaking unparsed bytes.
	bad := &WorkerResponse{McpServers: json.RawMessage(`{not-json`)}
	SanitizeMCPForL3(bad)
	if bad.McpServers != nil {
		t.Fatalf("malformed MCP JSON = %s, want nil", bad.McpServers)
	}

	// Defensive nil handling.
	SanitizeMCPForL3(nil)
	empty := &WorkerResponse{}
	SanitizeMCPForL3(empty)
	if empty.McpServers != nil {
		t.Fatalf("empty MCP = %s", empty.McpServers)
	}
}
