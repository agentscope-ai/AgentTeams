package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

// listFieldsFixtures returns the shared fixture for the ?fields= tests:
// "full" has every WorkerResponse field non-empty (team_leader of
// alpha-team), "solo" carries only its name. The listFixtureClient source
// serves workers in slice order [full, solo], so the golden bodies below
// are byte-exact.
func listFieldsFixtures() (full, solo *v1beta1.Worker, alpha *v1beta1.Team) {
	boolPtr := func(b bool) *bool { return &b }
	intPtr := func(i int) *int { return &i }
	strPtr := func(s string) *string { return &s }

	full = &v1beta1.Worker{}
	full.Name = "full"
	full.Namespace = "default"
	full.Spec.WorkerName = "full-display"
	full.Spec.Model = "model-a"
	full.Spec.SubagentModel = "model-s"
	full.Spec.LlmStreamFirstContentTimeout = "30"
	full.Spec.LlmStreamIdleTimeout = "60"
	full.Spec.LlmMaxRetries = "5"
	full.Spec.LlmBackoffBase = "200"
	full.Spec.LlmBackoffCap = "5"
	full.Spec.LlmMaxConcurrent = "4"
	full.Spec.LlmMaxQpm = "100"
	full.Spec.LlmRateLimitPause = "1"
	full.Spec.LlmRateLimitJitter = "100"
	full.Spec.LlmAcquireTimeout = "10"
	full.Spec.Runtime = "copaw" // -> runtimeDeprecated true in the response
	full.Spec.Image = "image-a"
	full.Spec.Identity = "identity-a"
	full.Spec.Soul = "soul-body"
	full.Spec.Agents = "agents-body"
	full.Spec.Skills = []string{"skill-a"}
	full.Spec.McpServers = []v1beta1.MCPServer{{Name: "mcp-a", URL: "https://mcp.example.com", Transport: "http"}}
	full.Spec.Package = "package-a"
	full.Spec.ChannelPolicy = &v1beta1.ChannelPolicySpec{GroupDenyExtra: []string{"grp-deny"}}
	full.Spec.ContainerManaged = boolPtr(true)
	full.Spec.State = strPtr("Sleeping")
	full.Spec.Env = map[string]string{"K": "V"}
	full.Spec.BackendRuntime = strPtr("pod")
	full.Status.Phase = "Running"
	full.Status.MatrixUserID = "@full:example.com"
	full.Status.RoomID = "room-full"
	full.Status.ContainerState = "running"
	full.Status.LastActiveAt = "2026-10-31T00:00:00Z"
	full.Status.AgentStatus = "running"
	full.Status.RunningTaskCount = intPtr(3)
	full.Status.LastRunAt = "2026-10-30T00:00:00Z"
	full.Status.LastFinishAt = "2026-10-29T00:00:00Z"
	full.Status.Message = "ok"
	full.Status.ExposedPorts = []v1beta1.ExposedPortStatus{{Port: 8080, Domain: "full.example.com"}}

	solo = &v1beta1.Worker{}
	solo.Name = "solo"
	solo.Namespace = "default"

	alpha = &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "full", Role: "team_leader"}}
	return full, solo, alpha
}

// Golden bodies for the golden worker list, in the WorkerResponse field
// order (json.Encoder output, trailing newline included). The "all" body
// is the pre-projection implementation's output: the default path must
// stay byte-identical to it (issue #1475 first red line).
const (
	goldenListAll = `{"workers":[{"envEditable":true,"env":{"K":"V"},"name":"full","workerName":"full-display","phase":"Running","containerManaged":true,"state":"Sleeping","model":"model-a","subagentModel":"model-s","llmStreamFirstContentTimeout":"30","llmStreamIdleTimeout":"60","llmMaxRetries":"5","llmBackoffBase":"200","llmBackoffCap":"5","llmMaxConcurrent":"4","llmMaxQpm":"100","llmRateLimitPause":"1","llmRateLimitJitter":"100","llmAcquireTimeout":"10","runtime":"copaw","runtimeDeprecated":true,"image":"image-a","identity":"identity-a","soul":"soul-body","agents":"agents-body","skills":["skill-a"],"mcpServers":[{"name":"mcp-a","url":"https://mcp.example.com","transport":"http"}],"package":"package-a","backendRuntime":"pod","channelPolicy":{"groupDenyExtra":["grp-deny"]},"containerState":"running","matrixUserID":"@full:example.com","roomID":"room-full","message":"ok","lastActiveAt":"2026-10-31T00:00:00Z","agentStatus":"running","runningTaskCount":3,"lastRunAt":"2026-10-30T00:00:00Z","lastFinishAt":"2026-10-29T00:00:00Z","exposedPorts":[{"port":8080,"domain":"full.example.com"}],"team":"alpha-team","role":"team_leader"},{"envEditable":true,"name":"solo","phase":"Pending","containerManaged":true,"state":"Running"}],"total":2}
`
	goldenListSpec = `{"workers":[{"name":"full","workerName":"full-display","containerManaged":true,"state":"Sleeping","model":"model-a","subagentModel":"model-s","llmStreamFirstContentTimeout":"30","llmStreamIdleTimeout":"60","llmMaxRetries":"5","llmBackoffBase":"200","llmBackoffCap":"5","llmMaxConcurrent":"4","llmMaxQpm":"100","llmRateLimitPause":"1","llmRateLimitJitter":"100","llmAcquireTimeout":"10","runtime":"copaw","runtimeDeprecated":true,"image":"image-a","identity":"identity-a","package":"package-a","backendRuntime":"pod","matrixUserID":"@full:example.com","roomID":"room-full","exposedPorts":[{"port":8080,"domain":"full.example.com"}],"team":"alpha-team","role":"team_leader"},{"name":"solo","containerManaged":true,"state":"Running"}],"total":2}
`
	goldenListStatus = `{"workers":[{"phase":"Running","containerState":"running","message":"ok","lastActiveAt":"2026-10-31T00:00:00Z","agentStatus":"running","runningTaskCount":3,"lastRunAt":"2026-10-30T00:00:00Z","lastFinishAt":"2026-10-29T00:00:00Z","team":"alpha-team","role":"team_leader"},{"phase":"Pending"}],"total":2}
`
	goldenListSpecStatus = `{"workers":[{"name":"full","workerName":"full-display","phase":"Running","containerManaged":true,"state":"Sleeping","model":"model-a","subagentModel":"model-s","llmStreamFirstContentTimeout":"30","llmStreamIdleTimeout":"60","llmMaxRetries":"5","llmBackoffBase":"200","llmBackoffCap":"5","llmMaxConcurrent":"4","llmMaxQpm":"100","llmRateLimitPause":"1","llmRateLimitJitter":"100","llmAcquireTimeout":"10","runtime":"copaw","runtimeDeprecated":true,"image":"image-a","identity":"identity-a","package":"package-a","backendRuntime":"pod","containerState":"running","matrixUserID":"@full:example.com","roomID":"room-full","message":"ok","lastActiveAt":"2026-10-31T00:00:00Z","agentStatus":"running","runningTaskCount":3,"lastRunAt":"2026-10-30T00:00:00Z","lastFinishAt":"2026-10-29T00:00:00Z","exposedPorts":[{"port":8080,"domain":"full.example.com"}],"team":"alpha-team","role":"team_leader"},{"name":"solo","phase":"Pending","containerManaged":true,"state":"Running"}],"total":2}
`
)

// TestListWorkersFieldsProjection pins the ?fields= contract on
// GET /api/v1/workers (issue #1475): the default (fields absent or "all")
// body is byte-identical to the pre-projection implementation, spec /
// status / spec,status return exactly their view, token order, trimming
// and duplicates do not matter, and the workers/total envelope is
// unchanged.
func TestListWorkersFieldsProjection(t *testing.T) {
	full, solo, alpha := listFieldsFixtures()

	for _, tc := range []struct {
		name   string
		query  string
		golden string
	}{
		{name: "default", query: "", golden: goldenListAll},
		{name: "all", query: "fields=all", golden: goldenListAll},
		{name: "spec", query: "fields=spec", golden: goldenListSpec},
		{name: "status", query: "fields=status", golden: goldenListStatus},
		{name: "spec-status", query: "fields=spec,status", golden: goldenListSpecStatus},
		{name: "status-spec-same", query: "fields=status,spec", golden: goldenListSpecStatus},
		{name: "trimmed", query: "fields=%20spec%20", golden: goldenListSpec},
		{name: "duplicate-tokens", query: "fields=spec,spec", golden: goldenListSpec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counting := &listFixtureClient{
				workers: []v1beta1.Worker{*full, *solo},
				teams:   []v1beta1.Team{*alpha},
			}
			handler := NewResourceHandler(counting, "default", nil, "", nil)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/workers?"+tc.query, nil)
			rec := httptest.NewRecorder()
			handler.ListWorkers(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.golden {
				t.Fatalf("body mismatch\ngot:  %s\nwant: %s", got, tc.golden)
			}
		})
	}
}

// TestListWorkersFieldsKeySets asserts the view field sets from issue
// #1475 on the fully-populated row: each view carries exactly its field
// set (nothing more, nothing less), and prompt bodies / full
// configuration (soul/agents/env/envEditable/skills/mcpServers/
// channelPolicy) never appear in any projection.
func TestListWorkersFieldsKeySets(t *testing.T) {
	full, solo, alpha := listFieldsFixtures()

	specKeys := []string{
		"name", "workerName", "model", "subagentModel",
		"llmStreamFirstContentTimeout", "llmStreamIdleTimeout",
		"llmMaxRetries", "llmBackoffBase", "llmBackoffCap",
		"llmMaxConcurrent", "llmMaxQpm", "llmRateLimitPause",
		"llmRateLimitJitter", "llmAcquireTimeout",
		"runtime", "runtimeDeprecated", "image", "identity",
		"package", "backendRuntime", "containerManaged", "state",
		"exposedPorts", "matrixUserID", "roomID", "team", "role",
	}
	statusKeys := []string{
		"phase", "containerState", "message", "lastActiveAt",
		"agentStatus", "runningTaskCount", "lastRunAt", "lastFinishAt",
		"team", "role",
	}
	// Excluded from every projection.
	excludedKeys := []string{
		"soul", "agents", "env", "envEditable", "skills", "mcpServers", "channelPolicy",
	}

	keySet := func(keys ...string) map[string]bool {
		set := make(map[string]bool, len(keys))
		for _, k := range keys {
			set[k] = true
		}
		return set
	}
	specSet := keySet(specKeys...)
	statusSet := keySet(statusKeys...)
	unionSet := keySet(specKeys...)
	for _, k := range statusKeys {
		unionSet[k] = true
	}
	excludedSet := keySet(excludedKeys...)

	// runListFields calls GET /api/v1/workers?fields=<fields> on the
	// fixture and returns each row's key set (row order = list order).
	runListFields := func(t *testing.T, fields string) []map[string]bool {
		t.Helper()
		counting := &listFixtureClient{
			workers: []v1beta1.Worker{*full, *solo},
			teams:   []v1beta1.Team{*alpha},
		}
		handler := NewResourceHandler(counting, "default", nil, "", nil)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/workers?fields="+fields, nil)
		rec := httptest.NewRecorder()
		handler.ListWorkers(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("fields=%s: status = %d: %s", fields, rec.Code, rec.Body.String())
		}
		var body struct {
			Workers []map[string]any `json:"workers"`
			Total   int              `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("fields=%s: decode: %v", fields, err)
		}
		if body.Total != 2 || len(body.Workers) != 2 {
			t.Fatalf("fields=%s: total=%d workers=%d, want 2/2", fields, body.Total, len(body.Workers))
		}
		rows := make([]map[string]bool, len(body.Workers))
		for i, row := range body.Workers {
			rows[i] = keySet()
			for k := range row {
				rows[i][k] = true
			}
		}
		return rows
	}

	assertExactKeys := func(t *testing.T, label string, got map[string]bool, want map[string]bool) {
		t.Helper()
		for k, present := range want {
			if !present && !got[k] {
				t.Errorf("%s: missing key %q", label, k)
			}
		}
		for k := range got {
			if !want[k] {
				t.Errorf("%s: unexpected key %q", label, k)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d keys, want %d", label, len(got), len(want))
		}
	}

	for _, tc := range []struct {
		fields   string
		wantFull map[string]bool
	}{
		{fields: "spec", wantFull: specSet},
		{fields: "status", wantFull: statusSet},
		{fields: "spec,status", wantFull: unionSet},
	} {
		t.Run(tc.fields, func(t *testing.T) {
			rows := runListFields(t, tc.fields)
			// Row 0 is the fully-populated worker: every view field is
			// non-empty, so the row carries exactly the view's field set.
			assertExactKeys(t, "full row", rows[0], tc.wantFull)
			// Row 1 (bare worker): no excluded keys, and no key outside
			// the view's set.
			for k := range rows[1] {
				if excludedSet[k] {
					t.Errorf("solo row: excluded key %q present", k)
				}
				if !tc.wantFull[k] {
					t.Errorf("solo row: key %q outside the view", k)
				}
			}
		})
	}
}

// TestListWorkersFieldsInvalid pins the fail-closed parsing of ?fields=
// (issue #1475): unknown tokens are a 400 whose message lists the valid
// values, and "all" cannot be combined with a subset.
func TestListWorkersFieldsInvalid(t *testing.T) {
	full, solo, alpha := listFieldsFixtures()

	for _, fields := range []string{"bogus", "spec,BOGUS", "ALL", "all,spec", "spec,status,bogus"} {
		t.Run("fields="+fields, func(t *testing.T) {
			counting := &listFixtureClient{
				workers: []v1beta1.Worker{*full, *solo},
				teams:   []v1beta1.Team{*alpha},
			}
			handler := NewResourceHandler(counting, "default", nil, "", nil)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/workers?fields="+fields, nil)
			rec := httptest.NewRecorder()
			handler.ListWorkers(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(body.Message, "spec, status, all") {
				t.Errorf("error message %q does not list the valid values", body.Message)
			}
		})
	}
}

// TestListWorkersFieldsWithFilters pins that projection composes with the
// existing filtering: L2 visibility scoping and the ?team= filter apply
// before projection, so fields=spec returns the same rows the default
// path would, each reduced to the spec view (prompt bodies stay off the
// wire even though the fixture carries a soul).
func TestListWorkersFieldsWithFilters(t *testing.T) {
	lead := &v1beta1.Worker{}
	lead.Name = "alpha-lead"
	lead.Namespace = "default"
	lead.Spec.WorkerName = "alpha-lead-display"
	lead.Spec.Model = "model-a"
	lead.Spec.Runtime = "qwenpaw"
	lead.Spec.Soul = "lead-soul"
	lead.Status.Phase = "Running"

	betaDev := &v1beta1.Worker{}
	betaDev.Name = "beta-dev"
	betaDev.Namespace = "default"
	betaDev.Spec.Model = "model-b"

	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "alpha-lead", Role: "team_leader"}}

	beta := &v1beta1.Team{}
	beta.Name = "beta-team"
	beta.Namespace = "default"
	beta.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "beta-dev"}}

	counting := &listFixtureClient{
		workers: []v1beta1.Worker{*lead, *betaDev},
		teams:   []v1beta1.Team{*alpha, *beta},
	}
	handler := NewResourceHandler(counting, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers?fields=spec&team=alpha-team", nil)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{
		Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"},
	}))
	rec := httptest.NewRecorder()
	handler.ListWorkers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	golden := `{"workers":[{"name":"alpha-lead","workerName":"alpha-lead-display","containerManaged":true,"state":"Running","model":"model-a","runtime":"qwenpaw","team":"alpha-team","role":"team_leader"}],"total":1}
`
	if got := rec.Body.String(); got != golden {
		t.Fatalf("body mismatch\ngot:  %s\nwant: %s", got, golden)
	}
}
