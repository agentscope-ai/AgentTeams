package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/server"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// humanFieldsCLIFixture returns the Human for the issue #1475 CLI
// regression tests: every view-relevant field is populated, including
// InitialPassword — the one-shot provisioning credential that no list
// response may carry but the detail endpoint must still return.
func humanFieldsCLIFixture() *v1beta1.Human {
	h := &v1beta1.Human{}
	h.Name = "alice"
	h.Namespace = "default"
	h.Spec.DisplayName = "Alice A"
	h.Spec.Email = "alice@example.com"
	h.Spec.PermissionLevel = 2
	h.Spec.AccessibleTeams = []string{"alpha-team"}
	h.Spec.AccessibleWorkers = []string{"alpha-dev"}
	h.Spec.Capabilities = []string{"external_sources"}
	h.Spec.Note = "ops lead"
	h.Status.Phase = "Active"
	h.Status.MatrixUserID = "@alice:example.com"
	h.Status.InitialPassword = "init-pw-123"
	h.Status.Rooms = []string{"!r1:example.com"}
	h.Status.Message = "ok"
	return h
}

// newHumanFieldsCLIServer starts the REAL controller handler (the
// regression must go CLI -> handler, not a stub) backed by a fake client
// holding the fixture human, and points AGENTTEAMS_CONTROLLER_URL at it.
func newHumanFieldsCLIServer(t *testing.T) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agentteams scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(humanFieldsCLIFixture()).
		Build()
	h := server.NewResourceHandler(k8sClient, "default", nil, "", nil)

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/humans", http.HandlerFunc(h.ListHumans))
	mux.Handle("GET /api/v1/humans/{name}", http.HandlerFunc(h.GetHuman))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
}

// runGetHumans executes the real `agt get humans` command with args and
// returns its stdout (the command writes all output to os.Stdout).
func runGetHumans(t *testing.T, args ...string) string {
	t.Helper()
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	var runErr error
	cmd := getHumansCmd()
	cmd.SetArgs(args)
	runErr = cmd.Execute()

	w.Close()
	os.Stdout = oldStdout
	if runErr != nil {
		t.Fatalf("get humans %v: %v", args, runErr)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return string(out)
}

// decodeHumansListBody decodes a captured `agt get humans -o json`
// stdout into the per-row key/value maps, failing the test if the CLI
// output is not the server envelope (humans/total).
func decodeHumansListBody(t *testing.T, out string) []map[string]any {
	t.Helper()
	var body struct {
		Humans []map[string]any `json:"humans"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("decode list output: %v\noutput:\n%s", err, out)
	}
	return body.Humans
}

// assertNoInitialPassword pins the issue #1475 credential rule on a
// LIST body: neither the key nor the credential value may appear.
func assertNoInitialPassword(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "initialPassword") {
		t.Errorf("list body carries the initialPassword key: %s", out)
	}
	if strings.Contains(out, "init-pw-123") {
		t.Errorf("list body carries the credential value: %s", out)
	}
}

// TestGetHumansJSONPassesViewThrough pins the issue #1475 contract on
// `agt get humans ... -o json`: the summary row carries the stable
// identity (name) and the list-card fields only, and no list view
// (default, all, summary) carries initialPassword.
func TestGetHumansJSONPassesViewThrough(t *testing.T) {
	newHumanFieldsCLIServer(t)

	t.Run("summary view keeps the list-card fields only", func(t *testing.T) {
		out := runGetHumans(t, "--fields=summary", "-o", "json")
		rows := decodeHumansListBody(t, out)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1: %s", len(rows), out)
		}
		row := rows[0]
		for _, key := range []string{"name", "phase", "displayName", "email", "permissionLevel", "matrixUserID", "message"} {
			if _, ok := row[key]; !ok {
				t.Errorf("summary view: missing %q: %s", key, out)
			}
		}
		if got := row["name"]; got != "alice" {
			t.Errorf("summary name = %v, want \"alice\"", got)
		}
		if got := row["permissionLevel"]; got != float64(2) {
			t.Errorf("summary permissionLevel = %v, want 2 (a zeroed value would mean the round-trip happened)", got)
		}
		// The scope lists, capabilities, note and rooms stay on the
		// detail endpoint.
		for _, key := range []string{"accessibleTeams", "accessibleWorkers", "capabilities", "note", "rooms"} {
			if _, ok := row[key]; ok {
				t.Errorf("summary view: unexpected %q: %s", key, out)
			}
		}
		assertNoInitialPassword(t, out)
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "default", args: []string{"-o", "json"}},
		{name: "fields=all", args: []string{"--fields=all", "-o", "json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runGetHumans(t, tc.args...)
			rows := decodeHumansListBody(t, out)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1: %s", len(rows), out)
			}
			row := rows[0]
			// The default is the full response, exactly as before the
			// projection existed — the scope lists, capabilities, note
			// and rooms all survive the CLI.
			for _, key := range []string{"name", "displayName", "email", "permissionLevel", "accessibleTeams", "accessibleWorkers", "capabilities", "note", "matrixUserID", "rooms", "message"} {
				if _, ok := row[key]; !ok {
					t.Errorf("full response: missing %q: %s", key, out)
				}
			}
			if got := row["displayName"]; got != "Alice A" {
				t.Errorf("displayName = %v, want \"Alice A\"", got)
			}
			// ...but the credential never rides along on a list row.
			assertNoInitialPassword(t, out)
		})
	}
}

// TestGetHumanJSONPassesDetailThrough pins the other side of the issue
// #1475 credential rule on `agt get humans <name> -o json`: the detail
// endpoint still returns initialPassword (one-shot provisioning), and
// the detail response is re-emitted verbatim, so the fields the CLI's
// humanResp type does not model (email, capabilities, note) survive
// the CLI.
func TestGetHumanJSONPassesDetailThrough(t *testing.T) {
	newHumanFieldsCLIServer(t)

	out := runGetHumans(t, "alice", "-o", "json")
	var row map[string]any
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("decode detail output: %v\noutput:\n%s", err, out)
	}
	for _, wc := range []struct {
		key  string
		want any
	}{
		{key: "name", want: "alice"},
		{key: "displayName", want: "Alice A"},
		{key: "email", want: "alice@example.com"},
		{key: "permissionLevel", want: float64(2)},
		{key: "matrixUserID", want: "@alice:example.com"},
		{key: "initialPassword", want: "init-pw-123"},
	} {
		if got := row[wc.key]; got != wc.want {
			t.Errorf("detail %s = %v, want %v", wc.key, got, wc.want)
		}
	}
	for _, key := range []string{"accessibleTeams", "capabilities", "note", "rooms"} {
		if _, ok := row[key]; !ok {
			t.Errorf("detail missing %q: %s", key, out)
		}
	}
}
