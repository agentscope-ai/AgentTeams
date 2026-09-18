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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestWorkspaceFilesHandler builds a handler with a fake K8s client
// (given teams and workers) whose worker URLs point at the given httptest
// server (mimicking one worker's qwenpaw app).
func newTestWorkspaceFilesHandler(t *testing.T, kubeMode string, ts *httptest.Server, objs ...runtime.Object) *WorkspaceFilesHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewWorkspaceFilesHandler(k8s, "default", kubeMode, "agentteams-worker-")
	if ts != nil {
		h.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	}
	return h
}

// kbRequest builds a request against the workspace-files route. The URL
// always uses a safe placeholder; the actual (possibly invalid) name is
// set via PathValue so handler validation is what's tested. query is an
// already-encoded raw query string (no leading "?").
func kbRequest(name, sub, query string) *http.Request {
	target := "/api/v1/workers/placeholder/workspace-files/" + sub
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("name", name)
	req.SetPathValue("sub", sub)
	return req
}

// kbWorkerFixture builds a Team plus its Worker CRs.
func kbWorkerFixture(team string, workers ...string) []runtime.Object {
	return checkpointTeamWithWorkers(team, workers...)
}

func TestWorkspaceFilesTree_ForwardsWithRootPinned(t *testing.T) {
	const payload = `{"directory":"memory","entries":[{"kind":"directory","name":"2026-08-31","path":"memory/2026-08-31"}],"has_more":false,"next_cursor":null}`
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", "tree", "path=memory")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/workspace/tree" {
		t.Fatalf("upstream path=%q, want /workspace/tree", gotPath)
	}
	want := url.Values{"path": {"memory"}, "root": {"workspace"}}
	if gotQuery != want.Encode() {
		t.Fatalf("upstream query=%q, want %q (root must be pinned server-side)", gotQuery, want.Encode())
	}
	if strings.Contains(gotQuery, "project") {
		t.Fatalf("upstream query=%q must never expose a root=project default", gotQuery)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), payload)
	}
}

func TestWorkspaceFilesFileContent_ForwardsOffsetLimit(t *testing.T) {
	const payload = `{"content":"# Memory","encoding":"utf-8","eof":true,"etag":"W/\"1-2\"","limit":2,"next_offset":2,"offset":0,"path":"MEMORY.md","truncated":false}`
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", "file-content", "path=MEMORY.md&offset=0&limit=4096")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/workspace/file-content" {
		t.Fatalf("upstream path=%q", gotPath)
	}
	want := url.Values{"path": {"MEMORY.md"}, "offset": {"0"}, "limit": {"4096"}, "root": {"workspace"}}
	if gotQuery != want.Encode() {
		t.Fatalf("upstream query=%q, want %q", gotQuery, want.Encode())
	}
}

func TestWorkspaceFilesFileMetadata_Forwards(t *testing.T) {
	const payload = `{"etag":"W/\"1-2\"","modified_at":"2026-08-31T00:00:00Z","path":"MEMORY.md","preview_kind":"text","size":2}`
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", "file-metadata", "path=MEMORY.md")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/workspace/file-metadata" {
		t.Fatalf("upstream path=%q", gotPath)
	}
	want := url.Values{"path": {"MEMORY.md"}, "root": {"workspace"}}
	if gotQuery != want.Encode() {
		t.Fatalf("upstream query=%q, want %q", gotQuery, want.Encode())
	}
}

func TestWorkspaceFilesTree_CursorPassthrough(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"directory":"memory","entries":[],"has_more":true,"next_cursor":"abc123"}`))
	}))
	defer upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", "tree", "path=memory&cursor=abc123&limit=50")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := url.Values{"path": {"memory"}, "cursor": {"abc123"}, "limit": {"50"}, "root": {"workspace"}}
	if gotQuery != want.Encode() {
		t.Fatalf("upstream query=%q, want %q (cursor is an opaque passthrough)", gotQuery, want.Encode())
	}
}

// TestWorkspaceFiles_AllowlistOK locks the positive boundary of the
// knowledge base allowlist: MEMORY.md at top level, memory/** and
// digest/** at realistic depths (real worker layout:
// memory/YYYY-MM-DD/{topic}.md and digest/{personal,procedure,wiki}/*.md).
func TestWorkspaceFiles_AllowlistOK(t *testing.T) {
	filePaths := []string{
		"MEMORY.md",
		"memory/2026-08-31/topic-note.md",
		"digest/personal/user-preferences.md",
	}
	for _, p := range filePaths {
		if err := validateKbPath(p, true); err != nil {
			t.Errorf("validateKbPath(%q, file)=%v, want ok", p, err)
		}
	}
	dirPaths := []string{
		"memory",
		"memory/2026-08-31",
		"digest",
		"digest/personal",
		"digest/procedure",
		"digest/wiki",
	}
	for _, p := range dirPaths {
		if err := validateKbPath(p, false); err != nil {
			t.Errorf("validateKbPath(%q, dir)=%v, want ok", p, err)
		}
	}
}

// TestWorkspaceFiles_RejectsSensitivePaths is the security boundary of
// this PR: the worker's runtime configuration (Matrix token, MinIO
// credentials) and every other dot directory must be unreachable through
// the proxy, even though the upstream path resolver would happily serve a
// directly requested dot path.
func TestWorkspaceFiles_RejectsSensitivePaths(t *testing.T) {
	for _, p := range []string{
		".copaw/agent.json",
		".git/config",
		".qwenpaw/agent.json",
		".reme_store_v1/index.json",
		".skill.json.lock",
		"memory/.hidden-note.md",
		"digest/.keep",
	} {
		if err := validateKbPath(p, true); err == nil {
			t.Errorf("validateKbPath(%q, file)=nil, want rejection", p)
		}
		if err := validateKbPath(p, false); err == nil {
			t.Errorf("validateKbPath(%q, dir)=nil, want rejection", p)
		}
	}
}

func TestWorkspaceFiles_RejectsTraversal(t *testing.T) {
	for _, p := range []string{
		"../MEMORY.md",
		"memory/../../SOUL.md",
		"/MEMORY.md",
		"MEMORY.md\\",
		"memory/./x.md",
		"memory/x/.hidden",
		"memory//x.md",
	} {
		if err := validateKbPath(p, true); err == nil {
			t.Errorf("validateKbPath(%q, file)=nil, want rejection", p)
		}
	}
}

// TestWorkspaceFiles_RejectsNonKB locks the negative boundary: workspace
// content outside MEMORY.md / memory/** / digest/** (identity files,
// TODO, checkpoint store, skills, prefix-confusion names) must not be
// addressable.
func TestWorkspaceFiles_RejectsNonKB(t *testing.T) {
	for _, p := range []string{
		"SOUL.md",
		"PROFILE.md",
		"AGENTS.md",
		"TODO.md",
		"checkpoints/shadow.git",
		"skills/some-skill.md",
		"memories/x.md",
		"memoryX/x.md",
		"digestX/x.md",
		"MEMORY.md/foo",
		"memory/a/b/c/d",
		"",
	} {
		if err := validateKbPath(p, true); err == nil {
			t.Errorf("validateKbPath(%q, file)=nil, want rejection", p)
		}
	}
	for _, p := range []string{
		"SOUL.md",
		"checkpoints",
		"skills",
		"memories",
		"memoryX",
		"",
	} {
		if err := validateKbPath(p, false); err == nil {
			t.Errorf("validateKbPath(%q, dir)=nil, want rejection", p)
		}
	}
}

func TestWorkspaceFiles_UnknownQueryRejected(t *testing.T) {
	h := newTestWorkspaceFilesHandler(t, "embedded", nil, kbWorkerFixture("market-team", "market-writer")...)
	cases := []struct {
		sub, query string
	}{
		{"tree", "path=memory&offset=0"},
		{"tree", "path=memory&root=project"},
		{"file-metadata", "path=MEMORY.md&cursor=x"},
		{"file-metadata", "path=MEMORY.md&offset=0"},
		{"file-content", "path=MEMORY.md&cursor=x"},
		{"tree", "path=memory&path=memory"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", tc.sub, tc.query)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("sub=%s query=%q status=%d, want 400", tc.sub, tc.query, rec.Code)
		}
	}
}

func TestWorkspaceFiles_InvalidBounds(t *testing.T) {
	h := newTestWorkspaceFilesHandler(t, "embedded", nil, kbWorkerFixture("market-team", "market-writer")...)
	for _, q := range []string{
		"path=memory&limit=0",
		"path=memory&limit=1001",
		"path=memory&limit=abc",
		"path=MEMORY.md&offset=-1",
		"path=MEMORY.md&offset=1.5",
		"path=MEMORY.md&offset=abc",
		"path=MEMORY.md&limit=0",
		"path=MEMORY.md&limit=1048577", // above the upstream MAX_CHUNK_SIZE mirror
		"path=memory&limit=501",        // above the upstream MAX_PAGE_SIZE mirror
	} {
		sub := "tree"
		if strings.Contains(q, "offset") || (strings.Contains(q, "limit") && strings.Contains(q, "MEMORY.md")) {
			sub = "file-content"
		}
		rec := httptest.NewRecorder()
		h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", sub, q)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d, want 400", q, rec.Code)
		}
	}
}

func TestWorkspaceFiles_FileSubpathsRequirePath(t *testing.T) {
	h := newTestWorkspaceFilesHandler(t, "embedded", nil, kbWorkerFixture("market-team", "market-writer")...)
	for _, sub := range []string{"file-metadata", "file-content"} {
		rec := httptest.NewRecorder()
		h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", sub, "")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("sub=%s status=%d, want 400 for missing path", sub, rec.Code)
		}
	}
}

func TestWorkspaceFiles_RejectsWriteAndUnknownSubpaths(t *testing.T) {
	h := newTestWorkspaceFilesHandler(t, "embedded", nil, kbWorkerFixture("market-team", "market-writer")...)
	for _, sub := range []string{"file-upload", "download", "restore", "graph", "status", "agent.json"} {
		rec := httptest.NewRecorder()
		h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", sub, "path=MEMORY.md")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("sub=%s status=%d, want 400 (write endpoints must never be reachable)", sub, rec.Code)
		}
	}
}

func TestWorkspaceFiles_RejectsInvalidWorkerName(t *testing.T) {
	h := newTestWorkspaceFilesHandler(t, "embedded", nil)
	for _, name := range []string{"", "Market-Writer", "../etc", "a b"} {
		rec := httptest.NewRecorder()
		h.proxyWorkspaceFiles(rec, adminCaller(kbRequest(name, "tree", "path=memory")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name=%q status=%d, want 400", name, rec.Code)
		}
	}
}

func TestWorkspaceFiles_UnknownWorker(t *testing.T) {
	h := newTestWorkspaceFilesHandler(t, "embedded", nil)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("ghost-worker", "tree", "path=memory")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for unknown worker", rec.Code)
	}
}

// TestWorkspaceFiles_L2HumanInScopeAllowed is the purpose case of this
// PR: an L2 human reads a worker in one of their accessibleTeams with
// their own Matrix-token identity.
func TestWorkspaceFiles_L2HumanInScopeAllowed(t *testing.T) {
	const payload = `{"content":"# Market team memory","encoding":"utf-8","eof":true,"limit":23,"next_offset":23,"offset":0,"path":"MEMORY.md","truncated":false}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	req := withCaller(kbRequest("market-writer", "file-content", "path=MEMORY.md"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for in-scope L2 human", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != payload {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestWorkspaceFiles_L2HumanCrossTeamHidden(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	req := withCaller(kbRequest("market-writer", "tree", "path=memory"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"biz-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team access (W8: hidden, not 403)", rec.Code)
	}
}

func TestWorkspaceFiles_TeamLeaderCrossTeamDenied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	req := withCaller(kbRequest("market-writer", "tree", "path=memory"),
		&authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "biz-lead", Team: "biz-team"})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team team leader (W8)", rec.Code)
	}
}

// TestWorkspaceFiles_StandaloneWorkerScopedHidden: a worker without a
// Team is hidden from scoped callers (they cannot enumerate standalone
// workers) but visible to admin.
func TestWorkspaceFiles_StandaloneWorkerScopedHidden(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"directory":"memory","entries":[],"has_more":false,"next_cursor":null}`))
	}))
	defer upstream.Close()

	// A Worker CR that no Team references (standalone worker).
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(checkpointWorker("standalone-worker")).Build()
	h := NewWorkspaceFilesHandler(k8s, "default", "embedded", "agentteams-worker-")
	h.workerBaseURL = func(string, map[string]string) string { return upstream.URL }

	scoped := withCaller(kbRequest("standalone-worker", "tree", "path=memory"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, scoped)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("scoped status=%d, want 404 for standalone worker", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("standalone-worker", "tree", "path=memory")))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status=%d, want 200 for standalone worker", rec.Code)
	}
}

// TestWorkspaceFiles_KubeModeUnsupported uses a ghost worker: the 503
// must come before any worker lookup, otherwise a 404-vs-503 split would
// leak worker existence.
func TestWorkspaceFiles_KubeModeUnsupported(t *testing.T) {
	h := newTestWorkspaceFilesHandler(t, "k8s", nil)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("ghost-worker", "tree", "path=memory")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode (before worker lookup)", rec.Code)
	}
}

func TestWorkspaceFiles_UnreachableWorker(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	urlStr := upstream.URL
	upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", nil, kbWorkerFixture("market-team", "market-writer")...)
	h.workerBaseURL = func(string, map[string]string) string { return urlStr }

	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", "tree", "path=memory")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for unreachable worker", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unreachable") {
		t.Fatalf("body=%s, want unreachable message", rec.Body.String())
	}
}

// TestWorkspaceFiles_Upstream404PassThrough: an upstream 404 means either
// "file not found" or "worker runs QwenPaw < 2.1" (no workspace router).
// It is passed through verbatim; clients use the MEMORY.md file-metadata
// probe to tell the two apart (see the API documentation).
func TestWorkspaceFiles_Upstream404PassThrough(t *testing.T) {
	const detail = `{"detail":"File not found"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(detail))
	}))
	defer upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", "file-content", "path=MEMORY.md")))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 passthrough", rec.Code)
	}
	if rec.Body.String() != detail {
		t.Fatalf("body=%s, want verbatim upstream detail", rec.Body.String())
	}
}

func TestWorkspaceFiles_Upstream500Bounded(t *testing.T) {
	big := strings.Repeat("x", 8192)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(big))
	}))
	defer upstream.Close()

	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", "tree", "path=memory")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
}

// TestWorkspaceFilesWorkerBaseURL_PrefixAndPortResolution covers the
// address resolution matrix: default / non-default / empty container
// prefixes. The console port is ALWAYS the effective system port: the
// worker system env defines AGENTTEAMS_CONSOLE_PORT and the system-wins
// user-env merge discards conflicting spec.env values before the container
// is created, so any user-declared port must resolve to the same port the
// container actually listens on.
func TestWorkspaceFilesWorkerBaseURL_PrefixAndPortResolution(t *testing.T) {
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
			h := &WorkspaceFilesHandler{containerPrefix: tc.prefix}
			if got := h.defaultWorkerBaseURL("alice", tc.env); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// captureWorkspaceFilesRoundTripper records the upstream URL the handler
// dials and returns a canned 200 JSON response.
type captureWorkspaceFilesRoundTripper struct {
	gotURL *url.URL
}

func (c *captureWorkspaceFilesRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.gotURL = req.URL
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`))),
	}, nil
}

// TestWorkspaceFiles_EffectivePrefixAndPortReachUpstream is the
// end-to-end regression: a controller configured with a non-default
// container prefix and a worker whose spec.env conflicts with the system
// console port must dial exactly http://{prefix}{name}:{effective-port}
// with root=workspace pinned.
func TestWorkspaceFiles_EffectivePrefixAndPortReachUpstream(t *testing.T) {
	objs := kbWorkerFixture("market-team", "market-writer")
	objs[1].(*v1beta1.Worker).Spec.Env = map[string]string{"AGENTTEAMS_CONSOLE_PORT": "9090"}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()

	h := NewWorkspaceFilesHandler(k8s, "default", "embedded", "acme-worker-")
	rt := &captureWorkspaceFilesRoundTripper{}
	h.http = &http.Client{Transport: rt}

	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, adminCaller(kbRequest("market-writer", "file-metadata", "path=MEMORY.md")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := "http://acme-worker-market-writer:8088/workspace/file-metadata?path=MEMORY.md&root=workspace"
	if rt.gotURL.String() != want {
		t.Fatalf("upstream URL=%s, want %s", rt.gotURL.String(), want)
	}
}

// ── Knowledge base write (PUT file-content) + file-download tests ─────────

// kbWriteRequest builds a PUT request against the workspace-files route with
// a JSON body and an optional If-Match header.
func kbWriteRequest(name, sub, query, body, ifMatch string) *http.Request {
	target := "/api/v1/workers/placeholder/workspace-files/" + sub
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(body))
	req.SetPathValue("name", name)
	req.SetPathValue("sub", sub)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	return req
}

// kbHumanFixture builds a Human CR (L2 by default).
func kbHumanFixture(name string, teams []string, access string) *v1beta1.Human {
	return &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:         name,
			PermissionLevel:     2,
			AccessibleTeams:     teams,
			WorkspaceFileAccess: access,
		},
	}
}

// kbWriteUpstream mimics the worker qwenpaw app for write tests: a
// configurable file-metadata probe answer and a PUT capture that verifies
// the forwarded body / If-Match.
func kbWriteUpstream(t *testing.T, exists bool, wantIfMatch string) (*httptest.Server, *int) {
	t.Helper()
	putCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/file-metadata"):
			if exists {
				_, _ = w.Write([]byte(`{"path":"memory/t.md","size":6,"modified":1,"etag":"et-1"}`))
			} else {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail":"File not found"}`))
			}
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/workspace/file-content"):
			putCalls++
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("If-Match") != wantIfMatch {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"detail":"unexpected If-Match"}`))
				return
			}
			if !strings.Contains(string(body), `"content":"hello-kb"`) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"detail":"unexpected content"}`))
				return
			}
			_, _ = w.Write([]byte(`{"etag":"et-2","path":"memory/t.md","size":9}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return upstream, &putCalls
}

// TestWorkspaceFilesWrite_CreateNewFile: L2 human in scope creates a new
// file (no If-Match — upstream forbids an ETag on a missing file).
func TestWorkspaceFilesWrite_CreateNewFile(t *testing.T) {
	upstream, putCalls := kbWriteUpstream(t, false, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, "readwrite"))...)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for in-scope L2 human create", rec.Code, rec.Body.String())
	}
	if *putCalls != 1 {
		t.Fatalf("upstream PUT calls=%d, want 1", *putCalls)
	}
	if !strings.Contains(rec.Body.String(), `"etag":"et-2"`) {
		t.Fatalf("body=%s, want upstream etag passthrough", rec.Body.String())
	}
}

// TestWorkspaceFilesWrite_UpdateWithETag: existing file + matching If-Match
// forwards the ETag to the worker app.
func TestWorkspaceFilesWrite_UpdateWithETag(t *testing.T) {
	upstream, _ := kbWriteUpstream(t, true, "et-1")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, "readwrite"))...)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, "et-1"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for ETag-matched update", rec.Code, rec.Body.String())
	}
}

// TestWorkspaceFilesWrite_ExistingWithoutIfMatchRejected: the proxy must not
// let a client skip the optimistic-concurrency check on an existing file
// (the worker auto-appends to its memory files — a bare overwrite is a lost
// update).
func TestWorkspaceFilesWrite_ExistingWithoutIfMatchRejected(t *testing.T) {
	upstream, putCalls := kbWriteUpstream(t, true, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, "readwrite"))...)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (If-Match required for existing file)", rec.Code)
	}
	if *putCalls != 0 {
		t.Fatalf("upstream PUT calls=%d, want 0 (no write without ETag check)", *putCalls)
	}
}

// TestWorkspaceFilesWrite_NewFileWithIfMatchRejected: an ETag on a missing
// file is a client error (upstream would 409; the proxy rejects earlier).
func TestWorkspaceFilesWrite_NewFileWithIfMatchRejected(t *testing.T) {
	upstream, putCalls := kbWriteUpstream(t, false, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, "readwrite"))...)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, "et-1"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (no If-Match on new file)", rec.Code)
	}
	if *putCalls != 0 {
		t.Fatalf("upstream PUT calls=%d, want 0", *putCalls)
	}
}

// TestWorkspaceFilesWrite_ETagConflictPassthrough: the worker changed the
// file between the client's read and write — upstream 409 passes through.
func TestWorkspaceFilesWrite_ETagConflictPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/workspace/file-metadata"):
			_, _ = w.Write([]byte(`{"path":"memory/t.md","size":6,"modified":1,"etag":"et-1"}`))
		default: // the PUT
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"File changed on disk"}`))
		}
	}))
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, "readwrite"))...)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, "stale-et"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 passthrough", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "File changed on disk") {
		t.Fatalf("body=%s, want upstream conflict detail", rec.Body.String())
	}
}

// TestWorkspaceFilesWrite_CrossTeamHidden: W8 — cross-team writes hide the
// worker as 404 (existence must not be probeable).
func TestWorkspaceFilesWrite_CrossTeamHidden(t *testing.T) {
	upstream, putCalls := kbWriteUpstream(t, false, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("bob", []string{"biz-team"}, ""))...)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"biz-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team write (W8)", rec.Code)
	}
	if *putCalls != 0 {
		t.Fatalf("upstream PUT calls=%d, want 0", *putCalls)
	}
}

// TestWorkspaceFilesWrite_ReadOnlyHumanDenied: L1 locked the user to
// read-only via workspaceFileAccess="read" — in-scope writes get 403.
func TestWorkspaceFilesWrite_ReadOnlyHumanDenied(t *testing.T) {
	upstream, putCalls := kbWriteUpstream(t, false, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, "read"))...)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for read-only human", rec.Code)
	}
	if *putCalls != 0 {
		t.Fatalf("upstream PUT calls=%d, want 0", *putCalls)
	}
}

// TestWorkspaceFilesWrite_DefaultAccessDenied is the upgrade regression the
// review required: a Human CR with no workspaceFileAccess field at all (the
// shape of every pre-existing L2 human right after a controller upgrade)
// must NOT be able to write — empty means "read", write is an explicit
// opt-in.
func TestWorkspaceFilesWrite_DefaultAccessDenied(t *testing.T) {
	upstream, putCalls := kbWriteUpstream(t, false, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, ""))...)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403 for empty workspaceFileAccess (upgrade default = read)", rec.Code, rec.Body.String())
	}
	if *putCalls != 0 {
		t.Fatalf("upstream PUT calls=%d, want 0", *putCalls)
	}
}

// TestWorkspaceFilesWrite_LeaderDenied: team leaders stay read-only on the
// write API (their KB management surface is the chat/tools path, not REST).
func TestWorkspaceFilesWrite_LeaderDenied(t *testing.T) {
	upstream, putCalls := kbWriteUpstream(t, false, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "market-writer", Team: "market-team"})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for in-scope team leader", rec.Code)
	}
	if *putCalls != 0 {
		t.Fatalf("upstream PUT calls=%d, want 0", *putCalls)
	}
}

// TestWorkspaceFilesWrite_AdminAllowed: L1 (admin) may write any team's
// knowledge base — no Human CR needed, no team scope.
func TestWorkspaceFilesWrite_AdminAllowed(t *testing.T) {
	upstream, _ := kbWriteUpstream(t, false, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, adminCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, "")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for admin", rec.Code, rec.Body.String())
	}
}

// TestWorkspaceFilesWrite_OverSizeRejected: the 1 MiB write cap is enforced
// before the worker app is touched.
func TestWorkspaceFilesWrite_OverSizeRejected(t *testing.T) {
	upstream, putCalls := kbWriteUpstream(t, false, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, "readwrite"))...)
	big := strings.Repeat("x", 1024*1024+1)
	req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"`+big+`"}`, ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for over-size body", rec.Code)
	}
	if *putCalls != 0 {
		t.Fatalf("upstream PUT calls=%d, want 0", *putCalls)
	}
}

// TestWorkspaceFilesWrite_SensitivePathRejected: the knowledge base
// allowlist applies to writes exactly as to reads (SOUL.md is the team
// owner's domain — never writable through this API).
func TestWorkspaceFilesWrite_SensitivePathRejected(t *testing.T) {
	upstream, putCalls := kbWriteUpstream(t, true, "")
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, "readwrite"))...)
	for _, path := range []string{"SOUL.md", "PROFILE.md", "skills/x/SKILL.md", ".copaw/agent.json", "memory/../../SOUL.md", "MEMORY.md/foo"} {
		req := withCaller(kbWriteRequest("market-writer", "file-content", "path="+url.QueryEscape(path), `{"content":"hello-kb"}`, ""),
			&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
		rec := httptest.NewRecorder()
		h.proxyWorkspaceFileWrite(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("path=%s status=%d, want 400 (allowlist)", path, rec.Code)
		}
	}
	if *putCalls != 0 {
		t.Fatalf("upstream PUT calls=%d, want 0", *putCalls)
	}
}

// TestWorkspaceFilesWrite_KubeModeUnavailable: uniform 503 in kube mode.
func TestWorkspaceFilesWrite_KubeModeUnavailable(t *testing.T) {
	h := newTestWorkspaceFilesHandler(t, "kube", nil, kbWorkerFixture("market-team", "market-writer")...)
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFileWrite(rec, adminCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", `{"content":"hello-kb"}`, "")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
}

// TestWorkspaceFilesWrite_UnknownSubpathAndBody: only file-content is a
// write subpath; the body must be {"content": string}.
func TestWorkspaceFilesWrite_UnknownSubpathAndBody(t *testing.T) {
	h := newTestWorkspaceFilesHandler(t, "embedded", nil, kbWorkerFixture("market-team", "market-writer")...)
	for sub, body := range map[string]string{"file-metadata": `{}`, "file-download": `{}`, "tree": `{}`} {
		rec := httptest.NewRecorder()
		h.proxyWorkspaceFileWrite(rec, adminCaller(kbWriteRequest("market-writer", sub, "path=memory", body, "")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("sub=%s status=%d, want 400 (not a write subpath)", sub, rec.Code)
		}
	}
	upstream, _ := kbWriteUpstream(t, false, "")
	defer upstream.Close()
	h2 := newTestWorkspaceFilesHandler(t, "embedded", upstream,
		append(kbWorkerFixture("market-team", "market-writer"),
			kbHumanFixture("alice", []string{"market-team"}, "readwrite"))...)
	for _, body := range []string{`[]`, `{"noContent":true}`, `not-json`} {
		req := withCaller(kbWriteRequest("market-writer", "file-content", "path=memory/t.md", body, ""),
			&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
		rec := httptest.NewRecorder()
		h2.proxyWorkspaceFileWrite(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%q status=%d, want 400 (invalid body)", body, rec.Code)
		}
	}
}

// TestWorkspaceFilesDownload_InScopeAllowed: file-download streams with the
// attachment headers forwarded verbatim.
func TestWorkspaceFilesDownload_InScopeAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/workspace/file-download") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown")
		w.Header().Set("Content-Disposition", `attachment; filename="MEMORY.md"`)
		w.Header().Set("Content-Length", "11")
		w.Header().Set("ETag", `"dl-1"`)
		_, _ = w.Write([]byte("memory-body"))
	}))
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	req := withCaller(kbRequest("market-writer", "file-download", "path=MEMORY.md"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 for in-scope download", rec.Code)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="MEMORY.md"` {
		t.Fatalf("Content-Disposition=%q, want attachment header forwarded", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/markdown" {
		t.Fatalf("Content-Type=%q, want upstream media type (not forced JSON)", got)
	}
	if rec.Body.String() != "memory-body" {
		t.Fatalf("body=%q", rec.Body.String())
	}
}

// TestWorkspaceFilesDownload_CrossTeamHidden: W8 applies to downloads.
func TestWorkspaceFilesDownload_CrossTeamHidden(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	req := withCaller(kbRequest("market-writer", "file-download", "path=MEMORY.md"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"biz-team"}})
	rec := httptest.NewRecorder()
	h.proxyWorkspaceFiles(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team download (W8)", rec.Code)
	}
}

// TestWorkspaceFilesDownload_SensitivePathRejected: the allowlist covers
// downloads — agent.json (Matrix token) must never stream out.
func TestWorkspaceFilesDownload_SensitivePathRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"leak"}`))
	}))
	defer upstream.Close()
	h := newTestWorkspaceFilesHandler(t, "embedded", upstream, kbWorkerFixture("market-team", "market-writer")...)
	for _, path := range []string{".copaw/agent.json", "MEMORY.md/foo"} {
		req := adminCaller(kbRequest("market-writer", "file-download", "path="+url.QueryEscape(path)))
		rec := httptest.NewRecorder()
		h.proxyWorkspaceFiles(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("path=%s status=%d, want 400 (allowlist) even for admin", path, rec.Code)
		}
	}
}

// TestWorkspaceFilesWrite_RouteAcceptsAuthorizedWrite is the regression test
// for the P1 review finding on this PR: the registered PUT route is the fixed
// literal /api/v1/workers/{name}/workspace-files/file-content (http.go) with
// no {sub} capture, but the handler used to read r.PathValue("sub") — always
// "" on the registered route — and rejected every authorized write with 400
// "unsupported workspace file write subpath" before any worker lookup. The
// requests below go through the actual NewHTTPServer(...).Mux registration,
// with no SetPathValue shims.
func TestWorkspaceFilesWrite_RouteAcceptsAuthorizedWrite(t *testing.T) {
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(kbWorkerFixture("market-team", "market-writer")...).Build()
	enricher := authpkg.NewCREnricher(k8s, "default")
	mw := authpkg.NewMiddleware(&alwaysAdminAuth{}, enricher, authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{
		Client:    k8s,
		Namespace: "default",
		KubeMode:  "embedded",
		AuthMw:    mw,
	})

	const body = `{"content":"# market memory"}`

	// 1) Existing worker (admin): must pass routing + authz + validation and
	//    reach the upstream probe — 502 against the dead worker pod URL in
	//    the test environment, never the phantom-sub 400.
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workers/market-writer/workspace-files/file-content?path=MEMORY.md",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sa-token")
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "unsupported workspace file write subpath") {
		t.Fatalf("P1 regression: authorized write rejected by the phantom sub check: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502 (upstream probe against the dead pod URL) — the request must reach the worker/upstream stage", rec.Code, rec.Body.String())
	}

	// 2) Unknown worker: routing + authz pass and the worker lookup runs —
	//    404 from the lookup proves the request went past the route level.
	req2 := httptest.NewRequest(http.MethodPut,
		"/api/v1/workers/ghost-writer/workspace-files/file-content?path=MEMORY.md",
		strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer sa-token")
	rec2 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unknown worker status=%d body=%s, want 404 from the worker lookup", rec2.Code, rec2.Body.String())
	}
}
