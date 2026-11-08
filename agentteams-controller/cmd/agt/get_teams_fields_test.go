package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/server"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// teamFieldsCLIFixture returns the Team for the issue #1475 CLI
// regression tests: every view-relevant field is populated (and the
// values are ones a decode/re-encode round-trip through the CLI's legacy
// teamResp type would drop — workerExposedPorts, subagentModel and the
// llm* tuning are not modelled by that struct), so a type round-trip in
// the CLI would be detectable in the assertions.
func teamFieldsCLIFixture() *v1beta1.Team {
	tm := &v1beta1.Team{}
	tm.Name = "alpha-team"
	tm.Namespace = "default"
	tm.Spec.Description = "alpha description"
	tm.Spec.TeamName = "Alpha"
	tm.Spec.Admin = &v1beta1.TeamAdminSpec{Name: "admin-1", MatrixUserID: "@admin-1:example.com"}
	tm.Spec.HumanMembers = []v1beta1.TeamMemberSpec{{Name: "alice", MatrixUserID: "@alice:example.com"}}
	tm.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
	tm.Spec.HeartbeatEvery = "30m"
	tm.Spec.SubagentModel = "model-s"
	tm.Spec.LlmStreamFirstContentTimeout = "30"
	tm.Status.Phase = "Active"
	tm.Status.TeamRoomID = "!team:example.com"
	tm.Status.LeaderDMRoomID = "!dm:example.com"
	tm.Status.LeaderReady = true
	tm.Status.ReadyWorkers = 1
	tm.Status.TotalWorkers = 2
	tm.Status.Message = "ok"
	tm.Status.Members = []v1beta1.TeamMemberStatus{
		{Name: "alpha-dev", ExposedPorts: []v1beta1.ExposedPortStatus{{Port: 8080, Domain: "alpha-dev.example.com"}}},
	}
	return tm
}

// newTeamFieldsCLIServer starts the REAL controller handler (the
// regression must go CLI -> handler, not a stub) backed by a fake client
// holding the fixture team, and points AGENTTEAMS_CONTROLLER_URL at it.
func newTeamFieldsCLIServer(t *testing.T) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agentteams scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(teamFieldsCLIFixture()).
		Build()
	h := server.NewResourceHandler(k8sClient, "default", nil, "", nil)

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/teams", http.HandlerFunc(h.ListTeams))
	mux.Handle("GET /api/v1/teams/{name}", http.HandlerFunc(h.GetTeam))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
}

// runGetTeams executes the real `agt get teams` command with args and
// returns its stdout (the command writes all output to os.Stdout).
func runGetTeams(t *testing.T, args ...string) string {
	t.Helper()
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	var runErr error
	cmd := getTeamsCmd()
	cmd.SetArgs(args)
	runErr = cmd.Execute()

	w.Close()
	os.Stdout = oldStdout
	if runErr != nil {
		t.Fatalf("get teams %v: %v", args, runErr)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return string(out)
}

// decodeTeamsListBody decodes a captured `agt get teams -o json` stdout
// into the per-row key/value maps, failing the test if the CLI output is
// not the server envelope (teams/total).
func decodeTeamsListBody(t *testing.T, out string) []map[string]any {
	t.Helper()
	var body struct {
		Teams []map[string]any `json:"teams"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("decode list output: %v\noutput:\n%s", err, out)
	}
	return body.Teams
}

// TestGetTeamsJSONPassesViewThrough pins the issue #1475 contract on
// `agt get teams ... -o json`: the selected view must come back exactly
// as the server sent it — the summary row carries the stable identity
// (name) and the list-card fields only, with no member/port references,
// and a decode/re-encode round-trip that drops workerExposedPorts,
// subagentModel or the llm* tuning is detectable in the assertions.
func TestGetTeamsJSONPassesViewThrough(t *testing.T) {
	newTeamFieldsCLIServer(t)

	t.Run("summary view keeps the list-card fields only", func(t *testing.T) {
		out := runGetTeams(t, "--fields=summary", "-o", "json")
		rows := decodeTeamsListBody(t, out)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1: %s", len(rows), out)
		}
		row := rows[0]
		for _, key := range []string{"name", "teamName", "phase", "description", "leaderName", "leaderReady", "readyWorkers", "totalWorkers", "message", "workerNames"} {
			if _, ok := row[key]; !ok {
				t.Errorf("summary view: missing %q: %s", key, out)
			}
		}
		if got := row["name"]; got != "alpha-team" {
			t.Errorf("summary name = %v, want \"alpha-team\"", got)
		}
		if got := row["leaderName"]; got != "alpha-lead" {
			t.Errorf("summary leaderName = %v, want \"alpha-lead\"", got)
		}
		if got := row["totalWorkers"]; got != float64(2) {
			t.Errorf("summary totalWorkers = %v, want 2 (a zeroed value would mean the round-trip happened)", got)
		}
		// The heavy fields never ride along on the summary view: the
		// member and port references, the room IDs, heartbeat and LLM
		// tuning stay on the detail endpoint.
		for _, key := range []string{"admin", "humanMembers", "workerMembers", "workerExposedPorts", "teamRoomID", "leaderDMRoomID", "heartbeatEvery", "subagentModel", "llmStreamFirstContentTimeout"} {
			if _, ok := row[key]; ok {
				t.Errorf("summary view: unexpected %q: %s", key, out)
			}
		}
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "default", args: []string{"-o", "json"}},
		{name: "fields=all", args: []string{"--fields=all", "-o", "json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runGetTeams(t, tc.args...)
			rows := decodeTeamsListBody(t, out)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1: %s", len(rows), out)
			}
			row := rows[0]
			// The default is the full response, exactly as before the
			// projection existed: the member references, the port
			// mapping and the LLM tuning all survive the CLI.
			for _, key := range []string{"name", "admin", "humanMembers", "workerMembers", "workerExposedPorts", "teamRoomID", "leaderDMRoomID", "heartbeatEvery", "subagentModel", "llmStreamFirstContentTimeout"} {
				if _, ok := row[key]; !ok {
					t.Errorf("full response: missing %q: %s", key, out)
				}
			}
			if got := row["subagentModel"]; got != "model-s" {
				t.Errorf("subagentModel = %v, want \"model-s\"", got)
			}
			if got := row["heartbeatEvery"]; got != "30m" {
				t.Errorf("heartbeatEvery = %v, want \"30m\"", got)
			}
			admin, ok := row["admin"].(map[string]any)
			if !ok || admin["name"] != "admin-1" {
				t.Errorf("admin = %v, want the admin-1 reference", row["admin"])
			}
			ports, ok := row["workerExposedPorts"].(map[string]any)
			if !ok {
				t.Fatalf("workerExposedPorts = %v, want the port mapping", row["workerExposedPorts"])
			}
			devPorts, ok := ports["alpha-dev"].([]any)
			if !ok || len(devPorts) != 1 {
				t.Fatalf("workerExposedPorts[alpha-dev] = %v, want one port", ports["alpha-dev"])
			}
			if p, _ := devPorts[0].(map[string]any); p["port"] != float64(8080) {
				t.Errorf("exposed port = %v, want 8080", p["port"])
			}
		})
	}
}

// TestGetTeamJSONPassesDetailThrough pins the same pass-through on the
// single-team form: the detail endpoint response is re-emitted verbatim,
// so the fields the CLI's teamResp type does not model (workerExposedPorts,
// subagentModel) survive the CLI.
func TestGetTeamJSONPassesDetailThrough(t *testing.T) {
	newTeamFieldsCLIServer(t)

	out := runGetTeams(t, "alpha-team", "-o", "json")
	var row map[string]any
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("decode detail output: %v\noutput:\n%s", err, out)
	}
	for _, wc := range []struct {
		key  string
		want any
	}{
		{key: "name", want: "alpha-team"},
		{key: "leaderName", want: "alpha-lead"},
		{key: "phase", want: "Active"},
		{key: "subagentModel", want: "model-s"},
		{key: "teamRoomID", want: "!team:example.com"},
		{key: "readyWorkers", want: float64(1)},
		{key: "totalWorkers", want: float64(2)},
	} {
		if got := row[wc.key]; got != wc.want {
			t.Errorf("detail %s = %v, want %v", wc.key, got, wc.want)
		}
	}
	for _, key := range []string{"admin", "humanMembers", "workerMembers", "workerExposedPorts"} {
		if _, ok := row[key]; !ok {
			t.Errorf("detail missing %q: %s", key, out)
		}
	}
}
