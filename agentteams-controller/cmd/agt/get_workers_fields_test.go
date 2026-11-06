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

// workerFieldsCLIFixture returns the Worker for the issue #1477 CLI
// regression tests: every view-relevant field is populated (and the values
// are ones a decode/re-encode round-trip would zero or drop), so a
// type round-trip in the CLI would be detectable in the assertions.
func workerFieldsCLIFixture() *v1beta1.Worker {
	boolPtr := func(b bool) *bool { return &b }
	intPtr := func(i int) *int { return &i }

	w := &v1beta1.Worker{}
	w.Name = "alice"
	w.Namespace = "default"
	w.Spec.WorkerName = "Alice"
	w.Spec.Model = "model-a"
	w.Spec.Runtime = "qwenpaw"
	w.Spec.ContainerManaged = boolPtr(true)
	w.Spec.Skills = []string{"skill-a"}
	w.Spec.Env = map[string]string{"K": "V"}
	w.Status.Phase = "Running"
	w.Status.AgentStatus = "busy"
	w.Status.RunningTaskCount = intPtr(3)
	w.Status.LastActiveAt = "2026-11-01T08:00:00Z"
	return w
}

// newWorkerFieldsCLIServer starts the REAL controller handler (the
// regression must go CLI -> handler, not a stub) backed by a fake client
// holding the fixture worker, and points AGENTTEAMS_CONTROLLER_URL at it.
func newWorkerFieldsCLIServer(t *testing.T) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agentteams scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workerFieldsCLIFixture()).
		Build()
	h := server.NewResourceHandler(k8sClient, "default", nil, "", nil)

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/workers", http.HandlerFunc(h.ListWorkers))
	mux.Handle("GET /api/v1/workers/{name}", http.HandlerFunc(h.GetWorker))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
}

// runGetWorkers executes the real `agt get workers` command with args and
// returns its stdout (the command writes all output to os.Stdout).
func runGetWorkers(t *testing.T, args ...string) string {
	t.Helper()
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	var runErr error
	cmd := getWorkersCmd()
	cmd.SetArgs(args)
	runErr = cmd.Execute()

	w.Close()
	os.Stdout = oldStdout
	if runErr != nil {
		t.Fatalf("get workers %v: %v", args, runErr)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return string(out)
}

// decodeListBody decodes a captured `agt get workers -o json` stdout into
// the per-row key/value maps, failing the test if the CLI output is not
// the server envelope (workers/total).
func decodeListBody(t *testing.T, out string) []map[string]any {
	t.Helper()
	var body struct {
		Workers []map[string]any `json:"workers"`
		Total   int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("decode list output: %v\noutput:\n%s", err, out)
	}
	return body.Workers
}

// TestGetWorkersJSONPassesViewThrough pins the issue #1477 fix on
// `agt get workers ... -o json`: the selected view must come back exactly
// as the server sent it — no decode/re-encode round-trip that drops
// agentStatus/runningTaskCount/lastActiveAt and invents
// containerManaged / phase:"".
func TestGetWorkersJSONPassesViewThrough(t *testing.T) {
	newWorkerFieldsCLIServer(t)

	t.Run("status view keeps the runtime fields", func(t *testing.T) {
		out := runGetWorkers(t, "--fields=status", "-o", "json")
		rows := decodeListBody(t, out)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1: %s", len(rows), out)
		}
		row := rows[0]
		for _, key := range []string{"name", "agentStatus", "runningTaskCount", "lastActiveAt"} {
			if _, ok := row[key]; !ok {
				t.Errorf("status view: missing %q: %s", key, out)
			}
		}
		for _, key := range []string{"containerManaged", "model", "skills"} {
			if _, ok := row[key]; ok {
				t.Errorf("status view: unexpected %q: %s", key, out)
			}
		}
		if got := row["agentStatus"]; got != "busy" {
			t.Errorf("agentStatus = %v, want \"busy\"", got)
		}
		if got := row["runningTaskCount"]; got != float64(3) {
			t.Errorf("runningTaskCount = %v, want 3 (a zeroed value would mean the round-trip happened)", got)
		}
	})

	t.Run("spec view has no phase", func(t *testing.T) {
		out := runGetWorkers(t, "--fields=spec", "-o", "json")
		rows := decodeListBody(t, out)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1: %s", len(rows), out)
		}
		row := rows[0]
		if _, ok := row["phase"]; ok {
			t.Errorf("spec view: must not carry phase (even phase:\"\"): %s", out)
		}
		if got := row["runtime"]; got != "qwenpaw" {
			t.Errorf("runtime = %v, want qwenpaw", got)
		}
		if got := row["model"]; got != "model-a" {
			t.Errorf("model = %v, want model-a", got)
		}
	})

	t.Run("spec,status carries both views", func(t *testing.T) {
		out := runGetWorkers(t, "--fields=spec,status", "-o", "json")
		rows := decodeListBody(t, out)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1: %s", len(rows), out)
		}
		row := rows[0]
		for _, key := range []string{"model", "runtime", "phase", "agentStatus", "runningTaskCount"} {
			if _, ok := row[key]; !ok {
				t.Errorf("spec,status view: missing %q: %s", key, out)
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
			out := runGetWorkers(t, tc.args...)
			rows := decodeListBody(t, out)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1: %s", len(rows), out)
			}
			row := rows[0]
			for _, key := range []string{"skills", "env"} {
				if _, ok := row[key]; !ok {
					t.Errorf("full response: missing %q: %s", key, out)
				}
			}
		})
	}
}

// TestGetWorkersTableShowsDashForMissingFields pins that the table output
// no longer invents values for fields the selected view does not carry
// (issue #1477): a status-view row has no runtime, so the RUNTIME column
// renders "-" — not the assumed "openclaw" — and a missing phase renders
// "-" — not the assumed "Pending".
func TestGetWorkersTableShowsDashForMissingFields(t *testing.T) {
	newWorkerFieldsCLIServer(t)

	out := runGetWorkers(t, "--fields=status")
	if strings.Contains(out, "openclaw") {
		t.Errorf("table must not assume a runtime for a view without it:\n%s", out)
	}
	if strings.Contains(out, "Pending") {
		t.Errorf("table must not assume a phase for a view without it:\n%s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("table missing the worker name:\n%s", out)
	}
	if !strings.Contains(out, "-") {
		t.Errorf("table must render the missing runtime as \"-\":\n%s", out)
	}
}

// TestGetWorkerJSONPassesDetailThrough pins the same pass-through on the
// single-worker form: the detail endpoint response is re-emitted verbatim,
// so the runtime-reported fields survive the CLI.
func TestGetWorkerJSONPassesDetailThrough(t *testing.T) {
	newWorkerFieldsCLIServer(t)

	out := runGetWorkers(t, "alice", "-o", "json")
	var row map[string]any
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("decode detail output: %v\noutput:\n%s", err, out)
	}
	for _, wc := range []struct {
		key  string
		want any
	}{
		{key: "name", want: "alice"},
		{key: "agentStatus", want: "busy"},
		{key: "runningTaskCount", want: float64(3)},
		{key: "lastActiveAt", want: "2026-11-01T08:00:00Z"},
		{key: "phase", want: "Running"},
		{key: "runtime", want: "qwenpaw"},
		{key: "containerManaged", want: true},
	} {
		if got := row[wc.key]; got != wc.want {
			t.Errorf("detail %s = %v, want %v", wc.key, got, wc.want)
		}
	}
	for _, key := range []string{"skills", "env"} {
		if _, ok := row[key]; !ok {
			t.Errorf("detail missing %q: %s", key, out)
		}
	}
}
