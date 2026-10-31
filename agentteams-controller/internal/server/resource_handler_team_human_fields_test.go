package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// teamHumanFixtureClient is a client.Client whose List returns the
// fixtures in exactly the given slice order for TeamList and HumanList,
// and whose Get serves the human fixtures by name. listFixtureClient
// predates the Human list, so the #1475 team/human fixtures need their
// own deterministic source. No other method is functional.
type teamHumanFixtureClient struct {
	client.Client
	teams  []v1beta1.Team
	humans []v1beta1.Human
}

func (c *teamHumanFixtureClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch l := list.(type) {
	case *v1beta1.TeamList:
		l.Items = make([]v1beta1.Team, len(c.teams))
		copy(l.Items, c.teams)
	case *v1beta1.HumanList:
		l.Items = make([]v1beta1.Human, len(c.humans))
		copy(l.Items, c.humans)
	default:
		return fmt.Errorf("teamHumanFixtureClient: unsupported list type %T", list)
	}
	return nil
}

func (c *teamHumanFixtureClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	h, ok := obj.(*v1beta1.Human)
	if !ok {
		return fmt.Errorf("teamHumanFixtureClient: unsupported get type %T", obj)
	}
	for i := range c.humans {
		if c.humans[i].Name == key.Name && c.humans[i].Namespace == key.Namespace {
			*h = c.humans[i]
			return nil
		}
	}
	return fmt.Errorf("teamHumanFixtureClient: human %q not found", key.Name)
}

// teamHumanFieldsFixtures returns the shared fixture for the team/human
// ?fields= tests: "alpha-team" has every TeamResponse field non-empty
// (leader + one worker, exposed port, all LLM tuning), "bare-team"
// carries only its name, "alice" has every HumanResponse field non-empty
// including InitialPassword (the value no list response may echo), and
// "bob" carries only name/displayName/permissionLevel. Slice order is
// list order, so the golden bodies below are byte-exact.
func teamHumanFieldsFixtures() (alpha, bare *v1beta1.Team, alice, bob *v1beta1.Human) {
	alpha = &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.Description = "alpha description"
	alpha.Spec.TeamName = "Alpha"
	alpha.Spec.Admin = &v1beta1.TeamAdminSpec{Name: "admin-1", MatrixUserID: "@admin-1:example.com"}
	alpha.Spec.HumanMembers = []v1beta1.TeamMemberSpec{{Name: "alice", MatrixUserID: "@alice:example.com"}}
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
	alpha.Spec.HeartbeatEvery = "30m"
	alpha.Spec.SubagentModel = "model-s"
	alpha.Spec.LlmStreamFirstContentTimeout = "30"
	alpha.Spec.LlmStreamIdleTimeout = "60"
	alpha.Spec.LlmMaxRetries = "5"
	alpha.Spec.LlmBackoffBase = "200"
	alpha.Spec.LlmBackoffCap = "5"
	alpha.Spec.LlmMaxConcurrent = "4"
	alpha.Spec.LlmMaxQpm = "100"
	alpha.Spec.LlmRateLimitPause = "1"
	alpha.Spec.LlmRateLimitJitter = "100"
	alpha.Spec.LlmAcquireTimeout = "10"
	alpha.Status.Phase = "Active"
	alpha.Status.TeamRoomID = "!team:example.com"
	alpha.Status.LeaderDMRoomID = "!dm:example.com"
	alpha.Status.LeaderReady = true
	alpha.Status.ReadyWorkers = 1
	alpha.Status.TotalWorkers = 2
	alpha.Status.Message = "ok"
	alpha.Status.Members = []v1beta1.TeamMemberStatus{
		{Name: "alpha-dev", ExposedPorts: []v1beta1.ExposedPortStatus{{Port: 8080, Domain: "alpha-dev.example.com"}}},
	}

	bare = &v1beta1.Team{}
	bare.Name = "bare-team"
	bare.Namespace = "default"

	alice = &v1beta1.Human{}
	alice.Name = "alice"
	alice.Namespace = "default"
	alice.Spec.DisplayName = "Alice A"
	alice.Spec.Email = "alice@example.com"
	alice.Spec.PermissionLevel = 2
	alice.Spec.AccessibleTeams = []string{"alpha-team"}
	alice.Spec.AccessibleWorkers = []string{"alpha-dev"}
	alice.Spec.Capabilities = []string{"external_sources"}
	alice.Spec.Note = "ops lead"
	alice.Status.Phase = "Active"
	alice.Status.MatrixUserID = "@alice:example.com"
	alice.Status.InitialPassword = "init-pw-123"
	alice.Status.Rooms = []string{"!r1:example.com"}
	alice.Status.Message = "ok"

	bob = &v1beta1.Human{}
	bob.Name = "bob"
	bob.Namespace = "default"
	bob.Spec.DisplayName = "Bob B"
	bob.Spec.PermissionLevel = 3
	return alpha, bare, alice, bob
}

// Golden bodies for the team list (json.Encoder output, trailing newline
// included). The "all" body is the pre-projection implementation's
// output: the default path must stay byte-identical to it (issue #1475
// red line).
const (
	goldenTeamsAll = `{"teams":[{"name":"alpha-team","teamName":"Alpha","phase":"Active","description":"alpha description","admin":{"name":"admin-1","matrixUserId":"@admin-1:example.com"},"humanMembers":[{"name":"alice","matrixUserId":"@alice:example.com"}],"workerMembers":[{"name":"alpha-lead","role":"team_leader"},{"name":"alpha-dev","role":"worker"}],"leaderName":"alpha-lead","heartbeatEvery":"30m","teamRoomID":"!team:example.com","leaderDMRoomID":"!dm:example.com","leaderReady":true,"readyWorkers":1,"totalWorkers":2,"message":"ok","workerNames":["alpha-dev"],"workerExposedPorts":{"alpha-dev":[{"port":8080,"domain":"alpha-dev.example.com"}]},"subagentModel":"model-s","llmStreamFirstContentTimeout":"30","llmStreamIdleTimeout":"60","llmMaxRetries":"5","llmBackoffBase":"200","llmBackoffCap":"5","llmMaxConcurrent":"4","llmMaxQpm":"100","llmRateLimitPause":"1","llmRateLimitJitter":"100","llmAcquireTimeout":"10"},{"name":"bare-team","phase":"Pending","workerMembers":null,"leaderName":"","leaderReady":false,"readyWorkers":0,"totalWorkers":0}],"total":2}
`
	goldenTeamsSummary = `{"teams":[{"name":"alpha-team","teamName":"Alpha","phase":"Active","description":"alpha description","leaderName":"alpha-lead","leaderReady":true,"readyWorkers":1,"totalWorkers":2,"message":"ok","workerNames":["alpha-dev"]},{"name":"bare-team","phase":"Pending","leaderName":"","leaderReady":false,"readyWorkers":0,"totalWorkers":0}],"total":2}
`
)

// teamHumanFixtureHandler builds a handler over the shared fixtures.
func teamHumanFixtureHandler() (*ResourceHandler, *teamHumanFixtureClient) {
	alpha, bare, alice, bob := teamHumanFieldsFixtures()
	counting := &teamHumanFixtureClient{
		teams:  []v1beta1.Team{*alpha, *bare},
		humans: []v1beta1.Human{*alice, *bob},
	}
	return NewResourceHandler(counting, "default", nil, "", nil), counting
}

// TestListTeamsFieldsProjection pins the ?fields= contract on
// GET /api/v1/teams (issue #1475): the default (fields absent or "all")
// body is byte-identical to the pre-projection implementation, summary
// returns exactly the list-card fields, token order, trimming and
// duplicates do not matter, and the teams/total envelope is unchanged.
func TestListTeamsFieldsProjection(t *testing.T) {
	handler, _ := teamHumanFixtureHandler()

	for _, tc := range []struct {
		name   string
		query  string
		golden string
	}{
		{name: "default", query: "", golden: goldenTeamsAll},
		{name: "all", query: "fields=all", golden: goldenTeamsAll},
		{name: "summary", query: "fields=summary", golden: goldenTeamsSummary},
		{name: "summary-duplicate", query: "fields=summary,summary", golden: goldenTeamsSummary},
		{name: "trimmed", query: "fields=%20summary%20", golden: goldenTeamsSummary},
		{name: "trailing-comma", query: "fields=summary,", golden: goldenTeamsSummary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/teams?"+tc.query, nil)
			rec := httptest.NewRecorder()
			handler.ListTeams(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.golden {
				t.Fatalf("body mismatch\ngot:  %s\nwant: %s", got, tc.golden)
			}
		})
	}
}

// TestListTeamsFieldsKeySets asserts the view field sets on the fully-
// populated row: the summary carries exactly the list-card set (nothing
// more, nothing less), and in particular the member and port references
// (admin / humanMembers / workerMembers / workerExposedPorts) never
// appear in it.
func TestListTeamsFieldsKeySets(t *testing.T) {
	handler, _ := teamHumanFixtureHandler()

	summaryKeys := []string{
		"name", "teamName", "phase", "description", "leaderName",
		"leaderReady", "readyWorkers", "totalWorkers", "message",
		"workerNames",
	}
	allKeys := []string{
		"name", "teamName", "phase", "description", "admin",
		"humanMembers", "workerMembers", "leaderName", "heartbeatEvery",
		"teamRoomID", "leaderDMRoomID", "leaderReady", "readyWorkers",
		"totalWorkers", "message", "workerNames", "workerExposedPorts",
		"subagentModel", "llmStreamFirstContentTimeout", "llmStreamIdleTimeout",
		"llmMaxRetries", "llmBackoffBase", "llmBackoffCap", "llmMaxConcurrent",
		"llmMaxQpm", "llmRateLimitPause", "llmRateLimitJitter", "llmAcquireTimeout",
	}

	keySet := func(keys ...string) map[string]bool {
		set := make(map[string]bool, len(keys))
		for _, k := range keys {
			set[k] = true
		}
		return set
	}
	summarySet := keySet(summaryKeys...)
	allSet := keySet(allKeys...)

	fetchRows := func(t *testing.T, fields string) []map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams?fields="+fields, nil)
		rec := httptest.NewRecorder()
		handler.ListTeams(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("fields=%s: status = %d: %s", fields, rec.Code, rec.Body.String())
		}
		var body struct {
			Teams []map[string]any `json:"teams"`
			Total int              `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("fields=%s: decode: %v", fields, err)
		}
		if body.Total != 2 || len(body.Teams) != 2 {
			t.Fatalf("fields=%s: total=%d teams=%d, want 2/2", fields, body.Total, len(body.Teams))
		}
		return body.Teams
	}

	assertExactKeys := func(t *testing.T, label string, row map[string]any, want map[string]bool) {
		t.Helper()
		if len(row) != len(want) {
			t.Errorf("%s: %d keys, want %d: %v", label, len(row), len(want), row)
		}
		for k := range row {
			if !want[k] {
				t.Errorf("%s: unexpected key %q", label, k)
			}
		}
		for k := range want {
			if !rowHas(row, k) {
				t.Errorf("%s: missing key %q", label, k)
			}
		}
	}

	// Row 0 is the fully-populated team: the summary carries exactly
	// its field set.
	summaryRows := fetchRows(t, "summary")
	assertExactKeys(t, "summary row", summaryRows[0], summarySet)
	// The bare team's summary omits only the empty omitempty fields.
	if _, has := summaryRows[1]["workerNames"]; has {
		t.Errorf("bare summary row: workerNames should be omitted when empty")
	}
	if _, has := summaryRows[1]["message"]; has {
		t.Errorf("bare summary row: message should be omitted when empty")
	}

	allRows := fetchRows(t, "")
	assertExactKeys(t, "default row", allRows[0], allSet)
}

// rowHas reports whether row carries key (helper kept local so the
// key-set loop reads like the worker fields tests).
func rowHas(row map[string]any, k string) bool {
	_, ok := row[k]
	return ok
}

// TestListTeamsFieldsInvalid pins the fail-closed parsing of the team
// ?fields= (issue #1475): unknown tokens are a 400 whose message lists
// the valid values, "all" cannot be combined with a subset, and a worker
// token (spec) is not a team token.
func TestListTeamsFieldsInvalid(t *testing.T) {
	handler, _ := teamHumanFixtureHandler()

	for _, fields := range []string{"bogus", "spec", "ALL", "all,summary", "summary,bogus"} {
		t.Run("fields="+fields, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/teams?fields="+fields, nil)
			rec := httptest.NewRecorder()
			handler.ListTeams(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(body.Message, "summary, all") {
				t.Errorf("error message %q does not list the valid values", body.Message)
			}
		})
	}
}

// TestListTeamsFieldsWithScope pins that team projection composes with
// the existing visibility scoping: an L2 human sees only its teams, and
// fields=summary reduces each surviving row (projection applies after
// filtering, issue #1475).
func TestListTeamsFieldsWithScope(t *testing.T) {
	handler, _ := teamHumanFixtureHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/teams?fields=summary", nil)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{
		Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"},
	}))
	rec := httptest.NewRecorder()
	handler.ListTeams(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	golden := `{"teams":[{"name":"alpha-team","teamName":"Alpha","phase":"Active","description":"alpha description","leaderName":"alpha-lead","leaderReady":true,"readyWorkers":1,"totalWorkers":2,"message":"ok","workerNames":["alpha-dev"]}],"total":1}
`
	if got := rec.Body.String(); got != golden {
		t.Fatalf("body mismatch\ngot:  %s\nwant: %s", got, golden)
	}
}

// TestListHumansFieldsProjection pins the ?fields=summary contract on
// GET /api/v1/humans (issue #1475): exactly the list-card fields, token
// trimming and duplicates do not matter, humans/total envelope kept.
func TestListHumansFieldsProjection(t *testing.T) {
	handler, _ := teamHumanFixtureHandler()

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "summary", query: "fields=summary"},
		{name: "summary-duplicate", query: "fields=summary,summary"},
		{name: "trimmed", query: "fields=%20summary%20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/humans?"+tc.query, nil)
			rec := httptest.NewRecorder()
			handler.ListHumans(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			golden := `{"humans":[{"name":"alice","phase":"Active","displayName":"Alice A","email":"alice@example.com","permissionLevel":2,"matrixUserID":"@alice:example.com","message":"ok"},{"name":"bob","phase":"Pending","displayName":"Bob B","permissionLevel":3}],"total":2}
`
			if got := rec.Body.String(); got != golden {
				t.Fatalf("body mismatch\ngot:  %s\nwant: %s", got, golden)
			}
		})
	}
}

// TestListHumansFieldsKeySets asserts the human summary field set on
// the fully-populated row: exactly the list-card set, and the scope
// lists, note, rooms and initialPassword never appear in it.
func TestListHumansFieldsKeySets(t *testing.T) {
	handler, _ := teamHumanFixtureHandler()

	keySet := func(keys ...string) map[string]bool {
		set := make(map[string]bool, len(keys))
		for _, k := range keys {
			set[k] = true
		}
		return set
	}
	summaryKeys := keySet(
		"name", "phase", "displayName", "email", "permissionLevel",
		"matrixUserID", "message",
	)
	excludedKeys := keySet(
		"accessibleTeams", "accessibleWorkers", "capabilities", "note",
		"rooms", "initialPassword",
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/humans?fields=summary", nil)
	rec := httptest.NewRecorder()
	handler.ListHumans(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Humans []map[string]any `json:"humans"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 2 || len(body.Humans) != 2 {
		t.Fatalf("total=%d humans=%d, want 2/2", body.Total, len(body.Humans))
	}
	if len(body.Humans[0]) != len(summaryKeys) {
		t.Errorf("alice row: %d keys, want %d: %v", len(body.Humans[0]), len(summaryKeys), body.Humans[0])
	}
	for k := range body.Humans[0] {
		if !summaryKeys[k] {
			t.Errorf("alice row: unexpected key %q", k)
		}
	}
	for k := range body.Humans[0] {
		if excludedKeys[k] {
			t.Errorf("alice row: excluded key %q present", k)
		}
	}
	for k := range body.Humans[1] {
		if !summaryKeys[k] {
			t.Errorf("bob row: key %q outside the summary view", k)
		}
	}
}

// TestListHumansFieldsInvalid pins the fail-closed parsing of the human
// ?fields= (issue #1475): unknown tokens and the "all"+subset
// combination are 400s whose message lists the valid values.
func TestListHumansFieldsInvalid(t *testing.T) {
	handler, _ := teamHumanFixtureHandler()

	for _, fields := range []string{"bogus", "ALL", "all,summary", "spec,status"} {
		t.Run("fields="+fields, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/humans?fields="+fields, nil)
			rec := httptest.NewRecorder()
			handler.ListHumans(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(body.Message, "summary, all") {
				t.Errorf("error message %q does not list the valid values", body.Message)
			}
		})
	}
}
