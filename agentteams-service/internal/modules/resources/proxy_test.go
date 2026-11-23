package resources

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

// --- fake store ---
//
// proxyStore implements the three Store calls the proxy makes (Get,
// UpdateSpec, SoftDelete) and rejects the rest: a proxy must never create,
// list or write observed state.

type updateSpecCall struct {
	name            string
	spec            Spec
	expectedVersion int64
	audit           *AuditMeta
}

type softDeleteCall struct {
	name            string
	expectedVersion int64
	audit           *AuditMeta
}

type proxyStore struct {
	rows map[string]*Worker

	getCalls int

	updateSpecCalls []updateSpecCall
	updateSpecErrs  []error // consumed one per call; ErrConflict bumps the row version

	softDeleteCalls []softDeleteCall
	softDeleteErrs  []error
}

func newProxyStore(seed ...*Worker) *proxyStore {
	s := &proxyStore{rows: map[string]*Worker{}}
	for _, w := range seed {
		s.rows[w.Name] = w
	}
	return s
}

var errProxyFakeUnused = errors.New("proxy test store: method not used by the proxy")

func (s *proxyStore) Create(context.Context, *Worker, *AuditMeta) (*Worker, error) {
	return nil, errProxyFakeUnused
}

func (s *proxyStore) Get(_ context.Context, name string) (*Worker, error) {
	s.getCalls++
	w, ok := s.rows[name]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *w
	return &cp, nil
}

func (s *proxyStore) List(context.Context, ListFilter) ([]*Worker, error) {
	return nil, errProxyFakeUnused
}

// The proxy uses SyncSpec (generation-only event); UpdateSpec is kept so the
// fake still satisfies Store for any other call site.
func (s *proxyStore) UpdateSpec(ctx context.Context, name string, spec Spec, expectedVersion int64, a *AuditMeta) (*Worker, error) {
	return s.SyncSpec(ctx, name, spec, expectedVersion, a)
}

func (s *proxyStore) SyncSpec(_ context.Context, name string, spec Spec, expectedVersion int64, a *AuditMeta) (*Worker, error) {
	s.updateSpecCalls = append(s.updateSpecCalls, updateSpecCall{name: name, spec: spec, expectedVersion: expectedVersion, audit: a})
	if len(s.updateSpecErrs) > 0 {
		err := s.updateSpecErrs[0]
		s.updateSpecErrs = s.updateSpecErrs[1:]
		if errors.Is(err, ErrConflict) {
			if w := s.rows[name]; w != nil {
				w.Version++
			}
		}
		return nil, err
	}
	w, ok := s.rows[name]
	if !ok {
		return nil, ErrNotFound
	}
	if w.Version != expectedVersion {
		return nil, ErrConflict
	}
	w.Version++
	cp := *w
	return &cp, nil
}

func (s *proxyStore) UpdateStatus(context.Context, string, Observed, int64) (bool, error) {
	return false, errProxyFakeUnused
}

func (s *proxyStore) SoftDelete(_ context.Context, name string, expectedVersion int64, a *AuditMeta) error {
	s.softDeleteCalls = append(s.softDeleteCalls, softDeleteCall{name: name, expectedVersion: expectedVersion, audit: a})
	if len(s.softDeleteErrs) > 0 {
		err := s.softDeleteErrs[0]
		s.softDeleteErrs = s.softDeleteErrs[1:]
		if errors.Is(err, ErrConflict) {
			if w := s.rows[name]; w != nil {
				w.Version++
			}
		}
		return err
	}
	w, ok := s.rows[name]
	if !ok {
		return ErrNotFound
	}
	if w.Version != expectedVersion {
		return ErrConflict
	}
	w.Version++
	return nil
}

// --- controller fixture ---

// capturedRequest records what the controller received. The handler runs in
// the httptest server goroutine, so the capture is mutex-guarded.
type capturedRequest struct {
	mu     sync.Mutex
	method string
	path   string
	query  string
	header http.Header
	body   string
}

func (c *capturedRequest) capture(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.method = r.Method
	c.path = r.URL.Path
	c.query = r.URL.RawQuery
	c.header = r.Header.Clone()
	c.body = string(body)
}

func (c *capturedRequest) snapshot() (method, path, query, body string, header http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.path, c.query, c.body, c.header.Clone()
}

// newProxyController starts a fake controller that answers with status,
// headers and body, and records the request it received.
func newProxyController(t *testing.T, status int, headers map[string]string, body string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	got := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.capture(r)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// newTestProxy builds a proxy with a discard logger and a fixed clock.
func newTestProxy(store Store, controllerURL string) *Proxy {
	return &Proxy{
		Store:         store,
		ControllerURL: controllerURL,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           func() time.Time { return hTestNow },
	}
}

// proxyServe runs one request through the proxy mux.
func proxyServe(p *Proxy, method, target, body string, caller *identity.Identity, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if caller != nil {
		r = r.WithContext(identity.WithIdentity(r.Context(), caller))
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	mux := http.NewServeMux()
	p.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

// proxyPutResponse is the 2xx PUT body: a full WorkerResponse (43 fields,
// controller types.go:92-146) carrying every spec field the proxy syncs.
const proxyPutResponse = `{"envEditable":true,"env":{"FOO":"new","BAR":"baz"},"name":"w1","workerName":"Worker One","phase":"Pending","containerManaged":true,"state":"Running","model":"qwen3-max","modelProvider":"apig-prod","subagentModel":"qwen3-mini","llmStreamFirstContentTimeout":"300s","llmStreamIdleTimeout":"300s","llmMaxRetries":"3","llmBackoffBase":"1s","llmBackoffCap":"30s","llmMaxConcurrent":"4","llmMaxQpm":"60","llmRateLimitPause":"5s","llmRateLimitJitter":"0.5s","llmAcquireTimeout":"10s","runtime":"qwenpaw","image":"img:v2","identity":"ident-2","soul":"new soul","agents":"new agents","skills":["s2"],"mcpServers":[{"name":"m2"}],"package":"pkg2","backendRuntime":"","channelPolicy":{"dm":"allow"},"containerState":"","matrixUserID":"","roomID":"","message":"","lastActiveAt":"","agentStatus":"","lastRunAt":"","lastFinishAt":"","exposedPorts":[{"port":8080,"domain":"w1.example.com"}],"team":"team-a","role":""}`

// sealedProxyWorker seeds a worker with DB-owned spec fields the sync must
// preserve: team_slug, expose, resources, env, remote skills. package /
// containerManaged / state are echoed by the controller and are synced.
func sealedProxyWorker(name string) *Worker {
	w := hSeedWorker(name, "team-a")
	w.Version = 7
	w.RemoteSkills = json.RawMessage(`[{"url":"https://skills.example.com"}]`)
	w.Resources = json.RawMessage(`{"cpu":"2","memory":"4Gi"}`)
	return w
}

// --- tests: PUT ---

func TestProxyPutForwardsAndSyncsSpec(t *testing.T) {
	store := newProxyStore(sealedProxyWorker("w1"))
	responseHeaders := map[string]string{
		"Content-Type":  "application/json",
		"ETag":          `"etag-1"`,
		"Cache-Control": httputil.ETagCacheControl,
		"Vary":          "Authorization",
	}
	srv, got := newProxyController(t, http.StatusOK, responseHeaders, proxyPutResponse)
	p := newTestProxy(store, srv.URL)

	rec := proxyServe(p, http.MethodPut, "/api/v1/workers/w1?force=true", `{"model":"qwen3-max"}`, managerID(), map[string]string{
		"Authorization":   "Bearer tok-1",
		"Content-Type":    "application/json",
		"Accept":          "application/json",
		"If-Match":        `"etag-0"`,
		"Idempotency-Key": "idem-1",
		"X-Request-Id":    "req-1",
	})

	// The client sees the controller response byte for byte.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != proxyPutResponse {
		t.Fatalf("body = %q, want the controller bytes verbatim", rec.Body.String())
	}
	for _, h := range []string{"Content-Type", "ETag", "Cache-Control", "Vary"} {
		if want := responseHeaders[h]; rec.Header().Get(h) != want {
			t.Fatalf("response header %s = %q, want %q", h, rec.Header().Get(h), want)
		}
	}

	// The controller received the inbound method, path, query, headers and
	// body unchanged.
	method, path, query, body, header := got.snapshot()
	if method != http.MethodPut {
		t.Fatalf("method = %q, want PUT", method)
	}
	if path != "/api/v1/workers/w1" {
		t.Fatalf("path = %q, want /api/v1/workers/w1", path)
	}
	if query != "force=true" {
		t.Fatalf("query = %q, want force=true", query)
	}
	if body != `{"model":"qwen3-max"}` {
		t.Fatalf("body = %q, want the inbound bytes", body)
	}
	for k, want := range map[string]string{
		"Authorization":   "Bearer tok-1",
		"Content-Type":    "application/json",
		"Accept":          "application/json",
		"If-Match":        `"etag-0"`,
		"Idempotency-Key": "idem-1",
		"X-Request-Id":    "req-1",
	} {
		if header.Get(k) != want {
			t.Fatalf("forwarded header %s = %q, want %q", k, header.Get(k), want)
		}
	}

	// Write-through sync: exactly one UpdateSpec against the read version.
	if len(store.updateSpecCalls) != 1 {
		t.Fatalf("UpdateSpec calls = %d, want 1", len(store.updateSpecCalls))
	}
	call := store.updateSpecCalls[0]
	if call.name != "w1" || call.expectedVersion != 7 {
		t.Fatalf("UpdateSpec(%q, version %d), want (w1, 7)", call.name, call.expectedVersion)
	}
	spec := call.spec
	if spec.WorkerName != "Worker One" || spec.Model != "qwen3-max" || spec.ModelProvider != "apig-prod" || spec.SubagentModel != "qwen3-mini" {
		t.Fatalf("model fields not synced: %+v", spec)
	}
	for field, pair := range map[string][2]string{
		"llmStreamFirstContentTimeout": {spec.LLMStreamFirstContentTimeout, "300s"},
		"llmStreamIdleTimeout":         {spec.LLMStreamIdleTimeout, "300s"},
		"llmMaxRetries":                {spec.LLMMaxRetries, "3"},
		"llmBackoffBase":               {spec.LLMBackoffBase, "1s"},
		"llmBackoffCap":                {spec.LLMBackoffCap, "30s"},
		"llmMaxConcurrent":             {spec.LLMMaxConcurrent, "4"},
		"llmMaxQpm":                    {spec.LLMMaxQPM, "60"},
		"llmRateLimitPause":            {spec.LLMRateLimitPause, "5s"},
		"llmRateLimitJitter":           {spec.LLMRateLimitJitter, "0.5s"},
		"llmAcquireTimeout":            {spec.LLMAcquireTimeout, "10s"},
		"runtime":                      {spec.Runtime, "qwenpaw"},
		"image":                        {spec.Image, "img:v2"},
		"identity":                     {spec.Identity, "ident-2"},
		"soul":                         {spec.Soul, "new soul"},
		"agents":                       {spec.Agents, "new agents"},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("spec.%s = %q, want %q", field, pair[0], pair[1])
		}
	}
	if len(spec.Skills) != 1 || spec.Skills[0] != "s2" {
		t.Fatalf("spec.Skills = %v, want [s2]", spec.Skills)
	}
	if string(spec.MCPServers) != `[{"name":"m2"}]` {
		t.Fatalf("spec.MCPServers = %s, want the response bytes", spec.MCPServers)
	}
	if string(spec.ChannelPolicy) != `{"dm":"allow"}` {
		t.Fatalf("spec.ChannelPolicy = %s, want the response bytes", spec.ChannelPolicy)
	}

	// DB-owned fields survive the overlay.
	if spec.TeamSlug != "team-a" {
		t.Fatalf("spec.TeamSlug = %q, want team-a (team API owns it)", spec.TeamSlug)
	}
	if string(spec.Expose) != `[{"port":8080,"protocol":"http"}]` {
		t.Fatalf("spec.Expose = %s, want the stored value", spec.Expose)
	}
	if string(spec.Resources) != `{"cpu":"2","memory":"4Gi"}` {
		t.Fatalf("spec.Resources = %s, want the stored value", spec.Resources)
	}
	if string(spec.RemoteSkills) != `[{"url":"https://skills.example.com"}]` {
		t.Fatalf("spec.RemoteSkills = %s, want the stored value", spec.RemoteSkills)
	}
	// package / containerManaged / state ARE echoed by the controller and
	// are part of the write-through subset (they were a stale-row gap).
	if spec.PackageRef != "pkg2" || !spec.ContainerManaged || spec.DesiredState != "Running" {
		t.Fatalf("synced scalar fields not applied: %+v", spec)
	}
	// An admin PUT echoes env; the write-through must adopt it (the old row
	// value would be stale, not authoritative).
	if spec.Env["FOO"] != "new" || spec.Env["BAR"] != "baz" {
		t.Fatalf("spec.Env = %v, want the controller-echoed env", spec.Env)
	}

	// Audit: who, which worker, which action.
	if call.audit == nil {
		t.Fatal("UpdateSpec got a nil audit meta")
	}
	if call.audit.Action != actionProxiedUpdate {
		t.Fatalf("audit action = %q, want %q", call.audit.Action, actionProxiedUpdate)
	}
	if call.audit.ActorType != string(identity.RoleManager) || call.audit.ActorID != "manager" {
		t.Fatalf("audit actor = %q/%q, want manager/manager", call.audit.ActorType, call.audit.ActorID)
	}
	if call.audit.RequestID != "req-1" {
		t.Fatalf("audit request id = %q, want req-1", call.audit.RequestID)
	}
	if call.audit.TokenJTI != identity.TokenDigest("tok-1") {
		t.Fatalf("audit token digest = %q, want the bearer digest", call.audit.TokenJTI)
	}
}

func TestProxyPutPassesErrorsThrough(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			store := newProxyStore(hSeedWorker("w1", "team-a"))
			body := `{"message":"controller old envelope"}`
			srv, _ := newProxyController(t, status, map[string]string{"Content-Type": "application/json"}, body)
			p := newTestProxy(store, srv.URL)

			rec := proxyServe(p, http.MethodPut, "/api/v1/workers/w1", `{"model":"x"}`, managerID(), nil)
			if rec.Code != status {
				t.Fatalf("status = %d, want %d", rec.Code, status)
			}
			if rec.Body.String() != body {
				t.Fatalf("body = %q, want the controller envelope verbatim", rec.Body.String())
			}
			if len(store.updateSpecCalls) != 0 || store.getCalls != 0 {
				t.Fatalf("store touched on %d: updateSpec=%d get=%d", status, len(store.updateSpecCalls), store.getCalls)
			}
		})
	}
}

func TestProxyPutConflictRetriesOnce(t *testing.T) {
	store := newProxyStore(sealedProxyWorker("w1"))
	store.updateSpecErrs = []error{ErrConflict}
	srv, _ := newProxyController(t, http.StatusOK, nil, proxyPutResponse)
	p := newTestProxy(store, srv.URL)

	rec := proxyServe(p, http.MethodPut, "/api/v1/workers/w1", `{"model":"m"}`, managerID(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the forwarded response is unaffected)", rec.Code)
	}
	if rec.Body.String() != proxyPutResponse {
		t.Fatal("conflict retry changed the forwarded bytes")
	}
	if len(store.updateSpecCalls) != 2 {
		t.Fatalf("UpdateSpec calls = %d, want 2 (initial + one conflict retry)", len(store.updateSpecCalls))
	}
	if store.updateSpecCalls[0].expectedVersion != 7 || store.updateSpecCalls[1].expectedVersion != 8 {
		t.Fatalf("expected versions = %d, %d; want 7, 8 (re-read after the conflict bump)",
			store.updateSpecCalls[0].expectedVersion, store.updateSpecCalls[1].expectedVersion)
	}
}

func TestProxyPutMissingRowIsNotCreated(t *testing.T) {
	store := newProxyStore() // the row was created out of band
	srv, _ := newProxyController(t, http.StatusOK, nil, proxyPutResponse)
	p := newTestProxy(store, srv.URL)

	// A caller-less request also covers the anonymous audit path.
	rec := proxyServe(p, http.MethodPut, "/api/v1/workers/w1", `{"model":"m"}`, nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != proxyPutResponse {
		t.Fatalf("status/body = %d/%q, want the controller response", rec.Code, rec.Body.String())
	}
	if len(store.updateSpecCalls) != 0 {
		t.Fatalf("UpdateSpec calls = %d, want 0: the proxy must not create rows", len(store.updateSpecCalls))
	}
	if store.getCalls != 1 {
		t.Fatalf("Get calls = %d, want 1", store.getCalls)
	}
}

func TestProxyPutControllerUnavailable(t *testing.T) {
	store := newProxyStore(hSeedWorker("w1", "team-a"))
	srv, _ := newProxyController(t, http.StatusOK, nil, proxyPutResponse)
	url := srv.URL
	srv.Close() // the controller is unreachable
	p := newTestProxy(store, url)

	rec := proxyServe(p, http.MethodPut, "/api/v1/workers/w1", `{"model":"m"}`, managerID(), nil)
	hAssertError(t, rec, http.StatusBadGateway, httputil.CodeUpstreamUnavailable)
	if len(store.updateSpecCalls) != 0 || store.getCalls != 0 {
		t.Fatal("the store was touched although the write never reached the controller")
	}
}

func TestProxyPutResponseTooLarge(t *testing.T) {
	store := newProxyStore(hSeedWorker("w1", "team-a"))
	srv, _ := newProxyController(t, http.StatusOK, nil, strings.Repeat("x", proxyMaxResponseBytes+1))
	p := newTestProxy(store, srv.URL)

	rec := proxyServe(p, http.MethodPut, "/api/v1/workers/w1", `{"model":"m"}`, managerID(), nil)
	hAssertError(t, rec, http.StatusBadGateway, httputil.CodeUpstreamUnavailable)
	if len(store.updateSpecCalls) != 0 {
		t.Fatal("the proxy must not sync from an over-limit body")
	}
}

// --- tests: DELETE ---

func TestProxyDeleteMirrorsStatus(t *testing.T) {
	const conflictBody = `{"message":"worker is a member of team team-a; remove via PUT/DELETE /api/v1/teams/team-a"}`
	const notFoundBody = `{"message":"delete worker: not found"}`
	cases := []struct {
		name        string
		status      int
		body        string
		wantDeleted bool
	}{
		{"deleted", http.StatusNoContent, "", true},
		{"already gone", http.StatusNotFound, notFoundBody, true},
		{"team member", http.StatusConflict, conflictBody, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newProxyStore(sealedProxyWorker("w1"))
			srv, _ := newProxyController(t, tc.status, nil, tc.body)
			p := newTestProxy(store, srv.URL)

			rec := proxyServe(p, http.MethodDelete, "/api/v1/workers/w1", "", managerID(), nil)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if rec.Body.String() != tc.body {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.body)
			}
			if !tc.wantDeleted {
				if len(store.softDeleteCalls) != 0 {
					t.Fatalf("SoftDelete calls = %d, want 0 for a %d", len(store.softDeleteCalls), tc.status)
				}
				if store.getCalls != 0 {
					t.Fatalf("Get calls = %d, want 0 (DB untouched)", store.getCalls)
				}
				return
			}
			if len(store.softDeleteCalls) != 1 {
				t.Fatalf("SoftDelete calls = %d, want 1", len(store.softDeleteCalls))
			}
			call := store.softDeleteCalls[0]
			if call.name != "w1" || call.expectedVersion != 7 {
				t.Fatalf("SoftDelete(%q, %d), want (w1, 7)", call.name, call.expectedVersion)
			}
			if call.audit == nil || call.audit.Action != actionProxiedDelete {
				t.Fatalf("audit = %+v, want action %q", call.audit, actionProxiedDelete)
			}
			if call.audit.ActorType != string(identity.RoleManager) || call.audit.ActorID != "manager" {
				t.Fatalf("audit actor = %q/%q, want manager/manager", call.audit.ActorType, call.audit.ActorID)
			}
		})
	}
}

func TestProxyDelete404MissingRow(t *testing.T) {
	store := newProxyStore()
	srv, _ := newProxyController(t, http.StatusNotFound, nil, `{"message":"delete worker: not found"}`)
	p := newTestProxy(store, srv.URL)

	rec := proxyServe(p, http.MethodDelete, "/api/v1/workers/w1", "", managerID(), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(store.softDeleteCalls) != 0 {
		t.Fatal("SoftDelete called for a row that does not exist")
	}
	if store.getCalls != 1 {
		t.Fatalf("Get calls = %d, want 1", store.getCalls)
	}
}

func TestProxyDeleteConflictRetriesOnce(t *testing.T) {
	store := newProxyStore(sealedProxyWorker("w1"))
	store.softDeleteErrs = []error{ErrConflict}
	srv, _ := newProxyController(t, http.StatusNoContent, nil, "")
	p := newTestProxy(store, srv.URL)

	rec := proxyServe(p, http.MethodDelete, "/api/v1/workers/w1", "", managerID(), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(store.softDeleteCalls) != 2 {
		t.Fatalf("SoftDelete calls = %d, want 2 (initial + one conflict retry)", len(store.softDeleteCalls))
	}
	if store.softDeleteCalls[0].expectedVersion != 7 || store.softDeleteCalls[1].expectedVersion != 8 {
		t.Fatalf("expected versions = %d, %d; want 7, 8",
			store.softDeleteCalls[0].expectedVersion, store.softDeleteCalls[1].expectedVersion)
	}
}
