package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestCheckpointHandler builds a handler with a fake K8s client (given
// teams and workers) whose worker URLs point at the given httptest server
// (mimicking one worker's qwenpaw app).
func newTestCheckpointHandler(t *testing.T, kubeMode string, ts *httptest.Server, objs ...runtime.Object) *CheckpointHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewCheckpointHandler(k8s, "default", kubeMode, "agentteams-worker-")
	if ts != nil {
		h.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	}
	return h
}

// checkpointTeam builds a Team whose WorkerMembers include the given names.
func checkpointTeam(name string, workers ...string) *v1beta1.Team {
	refs := make([]v1beta1.TeamWorkerRef, 0, len(workers))
	for _, w := range workers {
		refs = append(refs, v1beta1.TeamWorkerRef{Name: w})
	}
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.TeamSpec{TeamName: name, WorkerMembers: refs},
	}
}

// checkpointWorker builds a minimal Worker CR.
func checkpointWorker(name string) *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{WorkerName: name},
	}
}

// checkpointTeamWithWorkers builds a Team plus its Worker CRs (the handler
// resolves worker existence before forwarding).
func checkpointTeamWithWorkers(name string, workers ...string) []runtime.Object {
	objs := make([]runtime.Object, 0, len(workers)+1)
	objs = append(objs, checkpointTeam(name, workers...))
	for _, w := range workers {
		objs = append(objs, checkpointWorker(w))
	}
	return objs
}

func checkpointRequest(method, name, sub, query string) *http.Request {
	// The URL always uses a safe placeholder; the actual (possibly invalid)
	// name is set via PathValue so handler validation is what's tested.
	req := httptest.NewRequest(method, "/api/v1/workers/placeholder/checkpoints/"+sub+query, nil)
	req.SetPathValue("name", name)
	req.SetPathValue("sub", sub)
	return req
}

func adminCaller(req *http.Request) *http.Request {
	return withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
}

func TestCheckpointGraph_ForwardsVerbatim(t *testing.T) {
	const payload = `{"nodes":[{"kind":"auto","commit":"abc"}],"summary":{"total":1}}`
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestCheckpointHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "daily-luo", "graph", "?limit=10")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/workspace/checkpoints/graph" {
		t.Fatalf("upstream path=%q, want /workspace/checkpoints/graph", gotPath)
	}
	if gotQuery != "limit=10" {
		t.Fatalf("upstream query=%q, want limit=10", gotQuery)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), payload)
	}
}

func TestCheckpointStatus_Forwards(t *testing.T) {
	const payload = `{"auto_enabled":false,"has_checkpoints":true}`
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestCheckpointHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "daily-luo", "status", "")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/workspace/checkpoints/status" {
		t.Fatalf("upstream path=%q", gotPath)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCheckpoint_Upstream404MeansOldQwenPaw(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	h := newTestCheckpointHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "daily-luo", "graph", "")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "requires QwenPaw 2.1") {
		t.Fatalf("body=%s, want actionable 2.1 message", rec.Body.String())
	}
}

func TestCheckpoint_UnreachableWorker(t *testing.T) {
	// A server that immediately closes — the client gets a connection error.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := upstream.URL
	upstream.Close()

	h := newTestCheckpointHandler(t, "embedded", nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	h.workerBaseURL = func(string, map[string]string) string { return url }

	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "daily-luo", "graph", "")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for unreachable worker", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unreachable") {
		t.Fatalf("body=%s, want unreachable message", rec.Body.String())
	}
}

func TestCheckpoint_KubeModeUnsupported(t *testing.T) {
	h := newTestCheckpointHandler(t, "k8s", nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "daily-luo", "graph", "")))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
}

func TestCheckpoint_RejectsUnknownSubpath(t *testing.T) {
	h := newTestCheckpointHandler(t, "embedded", nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "daily-luo", "restore", "")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for unknown subpath", rec.Code)
	}
}

func TestCheckpoint_RejectsInvalidWorkerName(t *testing.T) {
	h := newTestCheckpointHandler(t, "embedded", nil)
	for _, name := range []string{"", "Daily-Luo", "../etc", "a b"} {
		rec := httptest.NewRecorder()
		h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, name, "graph", "")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name=%q status=%d, want 400", name, rec.Code)
		}
	}
}

func TestCheckpoint_RejectsInvalidLimit(t *testing.T) {
	h := newTestCheckpointHandler(t, "embedded", nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	for _, q := range []string{"?limit=0", "?limit=1001", "?limit=abc", "?limit=5&other=1"} {
		rec := httptest.NewRecorder()
		h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "daily-luo", "graph", q)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d, want 400", q, rec.Code)
		}
	}
}

func TestCheckpoint_UnknownWorker(t *testing.T) {
	h := newTestCheckpointHandler(t, "embedded", nil)
	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "ghost-worker", "graph", "")))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for unknown worker", rec.Code)
	}
}

func TestCheckpoint_TeamLeaderCrossTeamDenied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := newTestCheckpointHandler(t, "embedded", upstream, checkpointTeamWithWorkers("beta-team", "daily-luo")...)
	req := checkpointRequest(http.MethodGet, "daily-luo", "graph", "")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team access (W8)", rec.Code)
	}
}

func TestCheckpoint_UpstreamErrorBodyBounded(t *testing.T) {
	big := strings.Repeat("x", 8192)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(big))
	}))
	defer upstream.Close()

	h := newTestCheckpointHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "daily-luo", "graph", "")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
}

// TestCheckpointWorkerBaseURL_PrefixAndPortResolution covers the address
// resolution matrix: default / non-default / empty container prefixes. The
// console port is ALWAYS the effective system port: the worker system env
// defines AGENTTEAMS_CONSOLE_PORT and the system-wins user-env merge
// discards conflicting spec.env values before the container is created, so
// any user-declared port (valid, padded, invalid, out-of-range) must
// resolve to the same port the container actually listens on.
func TestCheckpointWorkerBaseURL_PrefixAndPortResolution(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		env    map[string]string
		want   string
	}{
		{"default prefix and port", "agentteams-worker-", nil, "http://agentteams-worker-alice:8088"},
		{"non-default prefix", "acme-worker-", nil, "http://acme-worker-alice:8088"},
		{"empty prefix (auto-prefix disabled)", "", nil, "http://alice:8088"},
		{"user port discarded (system wins)", "agentteams-worker-", map[string]string{"AGENTTEAMS_CONSOLE_PORT": "9090"}, "http://agentteams-worker-alice:8088"},
		{"custom prefix, user port discarded", "acme-worker-", map[string]string{"AGENTTEAMS_CONSOLE_PORT": " 7000 "}, "http://acme-worker-alice:8088"},
		{"invalid user port discarded", "agentteams-worker-", map[string]string{"AGENTTEAMS_CONSOLE_PORT": "not-a-port"}, "http://agentteams-worker-alice:8088"},
		{"out-of-range user port discarded", "agentteams-worker-", map[string]string{"AGENTTEAMS_CONSOLE_PORT": "99999"}, "http://agentteams-worker-alice:8088"},
		{"zero user port discarded", "agentteams-worker-", map[string]string{"AGENTTEAMS_CONSOLE_PORT": "0"}, "http://agentteams-worker-alice:8088"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &CheckpointHandler{containerPrefix: tc.prefix}
			if got := h.defaultWorkerBaseURL("alice", tc.env); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// captureRoundTripper records the upstream URL the handler dials and returns
// a canned 200 JSON response, so tests can assert on effective address
// resolution without a live worker container.
type captureRoundTripper struct {
	gotURL *url.URL
}

func (c *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.gotURL = req.URL
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`))),
	}, nil
}

// TestCheckpoint_EffectivePrefixAndPortReachUpstream is the end-to-end
// regression: a controller configured with a non-default container prefix
// and a worker whose spec.env conflicts with the system console port must
// dial exactly http://{prefix}{name}:{effective-port} — the conflicting
// spec.env value is discarded by the same system-wins merge the container
// creation applies, so the proxy targets the port the container actually
// listens on. A pre-fix handler reading the raw spec.env would dial 9090
// and 502, because the container listens on 8088.
func TestCheckpoint_EffectivePrefixAndPortReachUpstream(t *testing.T) {
	objs := checkpointTeamWithWorkers("team-a", "daily-luo")
	objs[1].(*v1beta1.Worker).Spec.Env = map[string]string{"AGENTTEAMS_CONSOLE_PORT": "9090"}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()

	h := NewCheckpointHandler(k8s, "default", "embedded", "acme-worker-")
	rt := &captureRoundTripper{}
	h.http = &http.Client{Transport: rt}

	rec := httptest.NewRecorder()
	h.proxyCheckpoint(rec, adminCaller(checkpointRequest(http.MethodGet, "daily-luo", "status", "")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := "http://acme-worker-daily-luo:8088/workspace/checkpoints/status"
	if rt.gotURL.String() != want {
		t.Fatalf("upstream URL=%s, want %s", rt.gotURL.String(), want)
	}
}

// TestCheckpointPort_MatchesContainerCreationEnvChain is the cross-component
// regression: the port the proxy resolves must equal the port the docker
// backend derives, computed through the real container-creation env chain
// (WorkerEnvBuilder.Build + the shared system-wins merge). If the system
// default or the merge semantics ever change, this test fails instead of
// the proxy silently 502-ing on every worker.
func TestCheckpointPort_MatchesContainerCreationEnvChain(t *testing.T) {
	userEnv := map[string]string{"AGENTTEAMS_CONSOLE_PORT": "9090"}

	// The docker backend reads the port from the merged CreateRequest env.
	builder := service.NewWorkerEnvBuilder(config.WorkerEnvDefaults{})
	sysEnv := builder.Build("daily-luo", &service.WorkerProvisionResult{
		GatewayKey:    "gk",
		MatrixToken:   "mt",
		RoomID:        "!room",
		MinIOPassword: "mp",
	})
	service.MergeUserEnv(sysEnv, userEnv) // same semantics the reconciler applies
	dockerSide := sysEnv[service.WorkerConsolePortEnv]

	// The proxy resolves the port through the shared effective-env helper.
	proxySide := service.EffectiveWorkerConsolePort(userEnv)

	if dockerSide != proxySide {
		t.Fatalf("docker backend port %q != checkpoint proxy port %q — proxy would 502", dockerSide, proxySide)
	}
	if dockerSide != "8088" {
		t.Fatalf("effective console port=%q, want %q (system default, user value must be discarded)", dockerSide, "8088")
	}
}
