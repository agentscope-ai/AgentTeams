package resources

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

// --- fakes ---

var hTestNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type fakeStore struct {
	mu          sync.Mutex
	rows        map[string]*Worker
	createCalls int
	getCalls    int
	listCalls   int
	lastCreated *Worker
	lastAudit   *AuditMeta
	lastFilter  ListFilter
	createErr   error
	getErr      error
	listErr     error
}

func newFakeStore(seed ...*Worker) *fakeStore {
	f := &fakeStore{rows: map[string]*Worker{}}
	for _, w := range seed {
		f.rows[w.Name] = w
	}
	return f
}

func (f *fakeStore) Create(_ context.Context, w *Worker, a *AuditMeta) (*Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.lastCreated = w
	f.lastAudit = a
	if f.createErr != nil {
		return nil, f.createErr
	}
	if _, ok := f.rows[w.Name]; ok {
		return nil, ErrAlreadyExists
	}
	cp := *w
	if cp.ObservedPhase == "" {
		cp.ObservedPhase = "Pending"
	}
	if cp.Generation == 0 {
		cp.Generation = 1
	}
	if cp.Version == 0 {
		cp.Version = 1
	}
	f.rows[w.Name] = &cp
	out := cp
	return &out, nil
}

func (f *fakeStore) Get(_ context.Context, name string) (*Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	w, ok := f.rows[name]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *w
	return &cp, nil
}

func (f *fakeStore) List(_ context.Context, filter ListFilter) ([]*Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, f.listErr
	}
	names := make([]string, 0, len(f.rows))
	for name := range f.rows {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*Worker, 0, len(names))
	for _, name := range names {
		w := f.rows[name]
		if filter.TeamSlug != "" && w.TeamSlug != filter.TeamSlug {
			continue
		}
		cp := *w
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeStore) UpdateSpec(context.Context, string, Spec, int64, *AuditMeta) (*Worker, error) {
	return nil, errors.New("fake: UpdateSpec not implemented")
}

func (f *fakeStore) SyncSpec(context.Context, string, Spec, int64, *AuditMeta) (*Worker, error) {
	return nil, errors.New("fake: SyncSpec not implemented")
}

func (f *fakeStore) UpdateStatus(context.Context, string, Observed, int64) (bool, error) {
	return false, errors.New("fake: UpdateStatus not implemented")
}

func (f *fakeStore) SoftDelete(context.Context, string, int64, *AuditMeta) error {
	return errors.New("fake: SoftDelete not implemented")
}

type fakeIdem struct {
	mu       sync.Mutex
	recs     map[string]*IdemRecord
	getCalls int
	putCalls int
	getErr   error
	putErr   error
}

func newFakeIdem() *fakeIdem {
	return &fakeIdem{recs: map[string]*IdemRecord{}}
}

func fakeIdemKey(principal, key string) string { return principal + "\x00" + key }

func (f *fakeIdem) Get(_ context.Context, principal, key string) (*IdemRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	rec, ok := f.recs[fakeIdemKey(principal, key)]
	if !ok {
		return nil, ErrIdemMiss
	}
	cp := *rec
	return &cp, nil
}

func (f *fakeIdem) Put(_ context.Context, principal, key, requestHash, workerName string, status int, body []byte, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	if f.putErr != nil {
		return f.putErr
	}
	f.recs[fakeIdemKey(principal, key)] = &IdemRecord{WorkerName: workerName, RequestHash: requestHash, Status: status, Body: append([]byte(nil), body...)}
	return nil
}

type denyCall struct {
	Action string
	Actor  string
	Code   string
	Reason string
}

type fakeAuditor struct {
	mu    sync.Mutex
	calls []denyCall
	err   error
}

func (f *fakeAuditor) Deny(_ context.Context, a *AuditMeta, code, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := denyCall{Code: code, Reason: reason}
	if a != nil {
		c.Action = a.Action
		c.Actor = a.ActorID
	}
	f.calls = append(f.calls, c)
	return f.err
}

func (f *fakeAuditor) snapshot() []denyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]denyCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// --- fixtures ---

func hSeedWorker(name, team string) *Worker {
	return &Worker{
		Name:                   name,
		WorkerName:             name + " display",
		TeamSlug:               team,
		Model:                  "qwen3",
		ModelProvider:          "apig-qwen",
		Runtime:                "qwenpaw",
		Skills:                 []string{"s1"},
		PackageRef:             "pkg",
		MCPServers:             json.RawMessage(`[{"name":"m","url":"https://mcp.example.com/mcp","transport":"http"}]`),
		Expose:                 json.RawMessage(`[{"port":8080,"protocol":"http"}]`),
		Env:                    map[string]string{"FOO": "bar"},
		ContainerManaged:       true,
		DesiredState:           "Running",
		ObservedPhase:          "Ready",
		ObservedContainerState: "running",
		AgentStatus:            "idle",
		Generation:             1,
		Version:                1,
		CreatedAt:              hTestNow,
		UpdatedAt:              hTestNow,
	}
}

func adminID() *identity.Identity {
	return &identity.Identity{Role: identity.RoleAdmin, Username: "admin", MatrixUserID: "@admin:mtx"}
}

func managerID() *identity.Identity {
	return &identity.Identity{Role: identity.RoleManager, Username: "manager"}
}

func humanL2ID() *identity.Identity {
	return &identity.Identity{Role: identity.RoleHuman, Username: "human-a", Teams: []string{"team-a"}}
}

func humanL3ID() *identity.Identity {
	return &identity.Identity{Role: identity.RoleHuman, Username: "human-l3", AccessibleWorkers: []string{"w1"}}
}

func leaderID() *identity.Identity {
	return &identity.Identity{Role: identity.RoleTeamLeader, Username: "leader-a", Team: "team-a"}
}

func workerID() *identity.Identity {
	return &identity.Identity{Role: identity.RoleWorker, Username: "w1", WorkerName: "w1"}
}

func newTestHandler(seed ...*Worker) (*Handler, *fakeStore, *fakeIdem, *fakeAuditor) {
	store := newFakeStore(seed...)
	idem := newFakeIdem()
	aud := &fakeAuditor{}
	h := &Handler{
		Store:   store,
		Idem:    idem,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return hTestNow },
		Auditor: aud,
	}
	return h, store, idem, aud
}

func defaultTestHandler() (*Handler, *fakeStore, *fakeIdem, *fakeAuditor) {
	return newTestHandler(
		hSeedWorker("w1", "team-a"),
		hSeedWorker("w2", "team-b"),
	)
}

func hServe(h *Handler, method, target, body string, caller *identity.Identity, headers map[string]string) *httptest.ResponseRecorder {
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
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func hDecodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) httputil.ErrorEnvelope {
	t.Helper()
	var env httputil.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %q)", err, rec.Body.String())
	}
	return env
}

func hAssertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) httputil.ErrorEnvelope {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	env := hDecodeEnvelope(t, rec)
	if env.Code != code {
		t.Fatalf("code = %q, want %q", env.Code, code)
	}
	if env.Message == "" {
		t.Fatal("error message is empty")
	}
	return env
}

// --- tests: authentication / authorization matrix ---

func TestHandlerUnauthenticated(t *testing.T) {
	h, _, _, _ := defaultTestHandler()
	cases := []struct {
		method, target, body string
	}{
		{http.MethodPost, "/api/v1/workers", `{"name":"w"}`},
		{http.MethodGet, "/api/v1/workers", ""},
		{http.MethodGet, "/api/v1/workers/w1", ""},
	}
	for _, c := range cases {
		rec := hServe(h, c.method, c.target, c.body, nil, nil)
		hAssertError(t, rec, http.StatusUnauthorized, httputil.CodeUnauthenticated)
	}
}

func TestHandlerAuthorizationMatrix(t *testing.T) {
	// 5 roles × create/list/get, asserted cell by cell against the frozen
	// matrix (production path).
	tests := []struct {
		name       string
		caller     *identity.Identity
		method     string
		target     string
		body       string
		wantStatus int
	}{
		{"admin create", adminID(), http.MethodPost, "/api/v1/workers", `{"name":"new-admin"}`, http.StatusCreated},
		{"manager create", managerID(), http.MethodPost, "/api/v1/workers", `{"name":"new-manager"}`, http.StatusCreated},
		{"human create", humanL2ID(), http.MethodPost, "/api/v1/workers", `{"name":"new-human"}`, http.StatusForbidden},
		{"leader create", leaderID(), http.MethodPost, "/api/v1/workers", `{"name":"new-leader"}`, http.StatusForbidden},
		{"worker create", workerID(), http.MethodPost, "/api/v1/workers", `{"name":"new-worker"}`, http.StatusForbidden},

		{"admin list", adminID(), http.MethodGet, "/api/v1/workers", "", http.StatusOK},
		{"manager list", managerID(), http.MethodGet, "/api/v1/workers", "", http.StatusOK},
		{"human list", humanL2ID(), http.MethodGet, "/api/v1/workers", "", http.StatusOK},
		{"leader list", leaderID(), http.MethodGet, "/api/v1/workers", "", http.StatusOK},
		{"worker list", workerID(), http.MethodGet, "/api/v1/workers", "", http.StatusForbidden},

		{"admin get", adminID(), http.MethodGet, "/api/v1/workers/w1", "", http.StatusOK},
		{"manager get", managerID(), http.MethodGet, "/api/v1/workers/w1", "", http.StatusOK},
		{"human L2 get own team", humanL2ID(), http.MethodGet, "/api/v1/workers/w1", "", http.StatusOK},
		{"human L2 get other team", humanL2ID(), http.MethodGet, "/api/v1/workers/w2", "", http.StatusNotFound},
		{"human L3 get assigned", humanL3ID(), http.MethodGet, "/api/v1/workers/w1", "", http.StatusOK},
		{"human L3 get unassigned", humanL3ID(), http.MethodGet, "/api/v1/workers/w2", "", http.StatusNotFound},
		{"leader get own team", leaderID(), http.MethodGet, "/api/v1/workers/w1", "", http.StatusOK},
		{"leader get other team", leaderID(), http.MethodGet, "/api/v1/workers/w2", "", http.StatusNotFound},
		{"worker get self", workerID(), http.MethodGet, "/api/v1/workers/w1", "", http.StatusOK},
		{"worker get other", workerID(), http.MethodGet, "/api/v1/workers/w2", "", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _, _ := defaultTestHandler()
			rec := hServe(h, tt.method, tt.target, tt.body, tt.caller, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusForbidden {
				env := hDecodeEnvelope(t, rec)
				if env.Code != httputil.CodeForbidden {
					t.Fatalf("code = %q, want forbidden", env.Code)
				}
			}
			if tt.wantStatus == http.StatusNotFound {
				env := hDecodeEnvelope(t, rec)
				if env.Code != httputil.CodeNotFound {
					t.Fatalf("code = %q, want not_found", env.Code)
				}
			}
		})
	}
}

// --- tests: create ---

func TestHandlerCreate(t *testing.T) {
	t.Run("201 defaults", func(t *testing.T) {
		h, store, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-new"}`, adminID(),
			map[string]string{"X-Request-Id": "req-123"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content-type = %q", ct)
		}
		if !strings.HasSuffix(rec.Body.String(), "\n") {
			t.Fatalf("body must end with a newline: %q", rec.Body.String())
		}
		var resp WorkerResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Name != "w-new" || resp.Runtime != "openclaw" {
			t.Fatalf("resp = %+v", resp)
		}
		if resp.Phase != "Pending" || resp.State != "Running" || !resp.ContainerManaged {
			t.Fatalf("defaults = phase:%q state:%q managed:%v", resp.Phase, resp.State, resp.ContainerManaged)
		}
		if store.createCalls != 1 || store.lastAudit == nil || store.lastAudit.Action != "worker.create" {
			t.Fatalf("store create calls=%d audit=%+v", store.createCalls, store.lastAudit)
		}
		if store.lastAudit.ActorID != "admin" || store.lastAudit.ActorType != "admin" {
			t.Fatalf("audit actor = %+v", store.lastAudit)
		}
		if store.lastAudit.RequestID != "req-123" {
			t.Fatalf("audit request id = %q", store.lastAudit.RequestID)
		}
	})

	t.Run("default runtime and model", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		h.DefaultRuntime = "qwenpaw"
		h.DefaultModel = "qwen3-max"
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-def"}`, managerID(), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		var resp WorkerResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Runtime != "qwenpaw" || resp.Model != "qwen3-max" {
			t.Fatalf("runtime/model = %q/%q", resp.Runtime, resp.Model)
		}
	})

	t.Run("containerManaged false and state carried", func(t *testing.T) {
		h, store, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers",
			`{"name":"w-cm","containerManaged":false,"state":"Sleeping"}`, adminID(), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		var resp WorkerResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ContainerManaged || resp.State != "Sleeping" {
			t.Fatalf("resp = %+v", resp)
		}
		if store.lastCreated.ContainerManaged || store.lastCreated.DesiredState != "Sleeping" {
			t.Fatalf("stored = %+v", store.lastCreated)
		}
	})

	t.Run("duplicate name 409", func(t *testing.T) {
		h, store, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w1"}`, adminID(), nil)
		hAssertError(t, rec, http.StatusConflict, httputil.CodeAlreadyExists)
		if store.createCalls != 1 {
			t.Fatalf("create calls = %d", store.createCalls)
		}
	})

	t.Run("copaw 400", func(t *testing.T) {
		h, store, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-copaw","runtime":"CoPaw"}`, adminID(), nil)
		env := hAssertError(t, rec, http.StatusBadRequest, httputil.CodeDeprecatedRuntime)
		if !strings.Contains(env.Message, "migrate to qwenpaw") {
			t.Fatalf("message = %q", env.Message)
		}
		if store.createCalls != 0 {
			t.Fatalf("create calls = %d", store.createCalls)
		}
	})

	t.Run("invalid name 400", func(t *testing.T) {
		h, store, _, aud := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"Bad_Name"}`, adminID(), nil)
		env := hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidName)
		if !strings.Contains(env.Message, "invalid worker name") {
			t.Fatalf("message = %q", env.Message)
		}
		if store.createCalls != 0 {
			t.Fatalf("create calls = %d", store.createCalls)
		}
		calls := aud.snapshot()
		if len(calls) != 1 || calls[0].Code != httputil.CodeInvalidName || calls[0].Action != "worker.create" {
			t.Fatalf("audit denies = %+v", calls)
		}
	})

	t.Run("missing name 400", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{}`, adminID(), nil)
		env := hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidName)
		if env.Message != "worker name is required" {
			t.Fatalf("message = %q", env.Message)
		}
	})

	t.Run("invalid JSON 400", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":`, adminID(), nil)
		env := hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidRequest)
		if !strings.HasPrefix(env.Message, "invalid JSON:") {
			t.Fatalf("message = %q", env.Message)
		}
	})

	t.Run("reserved env key 400", func(t *testing.T) {
		h, store, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers",
			`{"name":"w-env","env":{"AGENTTEAMS_WORKER_NAME":"hack"}}`, adminID(), nil)
		env := hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidEnv)
		if env.Message != "environment variable is controlled by the system: AGENTTEAMS_WORKER_NAME" {
			t.Fatalf("message = %q", env.Message)
		}
		if store.createCalls != 0 {
			t.Fatalf("create calls = %d", store.createCalls)
		}
	})

	t.Run("invalid env key 400", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers",
			`{"name":"w-env2","env":{"1BAD":"v"}}`, adminID(), nil)
		hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidEnv)
	})

	t.Run("admin env round-trip", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers",
			`{"name":"w-env3","env":{"FOO":"bar"}}`, adminID(), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		var resp WorkerResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !resp.EnvEditable || resp.Env["FOO"] != "bar" {
			t.Fatalf("env = %+v editable=%v", resp.Env, resp.EnvEditable)
		}
	})

	t.Run("store failure 500", func(t *testing.T) {
		h, store, _, _ := defaultTestHandler()
		store.createErr = errors.New("db down")
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-err"}`, adminID(), nil)
		hAssertError(t, rec, http.StatusInternalServerError, httputil.CodeInternal)
	})
}

// --- tests: idempotency ---

func TestHandlerIdempotency(t *testing.T) {
	h, store, idem, _ := defaultTestHandler()
	headers := map[string]string{"Idempotency-Key": "key-1"}

	first := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-idem"}`, adminID(), headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body %q", first.Code, first.Body.String())
	}
	if store.createCalls != 1 || idem.putCalls != 1 {
		t.Fatalf("after first: create=%d put=%d", store.createCalls, idem.putCalls)
	}

	// Same key + same body: replay the first outcome without touching the
	// store.
	second := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-idem"}`, adminID(), headers)
	if second.Code != http.StatusCreated || second.Body.String() != first.Body.String() {
		t.Fatalf("replay = %d %q, first %q", second.Code, second.Body.String(), first.Body.String())
	}
	if store.createCalls != 1 || idem.putCalls != 1 {
		t.Fatalf("after replay: create=%d put=%d", store.createCalls, idem.putCalls)
	}
	// Same key + different body: conflict, never a replay.
	third := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-other"}`, adminID(), headers)
	if third.Code != http.StatusConflict {
		t.Fatalf("mismatched body = %d, want 409: %s", third.Code, third.Body.String())
	}
	if store.createCalls != 1 || idem.putCalls != 1 {
		t.Fatalf("after mismatch: create=%d put=%d", store.createCalls, idem.putCalls)
	}
}

func TestHandlerIdempotencyReplaysStoredOutcome(t *testing.T) {
	h, store, idem, _ := defaultTestHandler()
	body := []byte(`{"code":"already_exists","message":"stored"}\n`)
	idem.recs[fakeIdemKey(adminID().MatrixUserID, "cached")] = &IdemRecord{WorkerName: "w1", Status: http.StatusConflict, Body: body}

	rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-x"}`, adminID(),
		map[string]string{"Idempotency-Key": "cached"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(body) {
		t.Fatalf("replay body = %q, want %q", rec.Body.String(), body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if store.createCalls != 0 {
		t.Fatalf("create calls = %d", store.createCalls)
	}
}

func TestHandlerIdempotencyBestEffort(t *testing.T) {
	t.Run("put failure does not change the response", func(t *testing.T) {
		h, _, idem, _ := defaultTestHandler()
		idem.putErr = errors.New("idem db down")
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-put"}`, adminID(),
			map[string]string{"Idempotency-Key": "put-fail"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		if idem.putCalls != 1 {
			t.Fatalf("put calls = %d", idem.putCalls)
		}
	})

	t.Run("lookup failure falls open", func(t *testing.T) {
		h, store, idem, _ := defaultTestHandler()
		idem.getErr = errors.New("idem db down")
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-get"}`, adminID(),
			map[string]string{"Idempotency-Key": "get-fail"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		if store.createCalls != 1 {
			t.Fatalf("create calls = %d", store.createCalls)
		}
	})

	t.Run("empty key skips the idem store", func(t *testing.T) {
		h, _, idem, _ := defaultTestHandler()
		rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-nokey"}`, adminID(), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		if idem.getCalls != 0 || idem.putCalls != 0 {
			t.Fatalf("idem calls get=%d put=%d", idem.getCalls, idem.putCalls)
		}
	})
}

// --- tests: list fields / filtering ---

func hDecodeList(t *testing.T, rec *httptest.ResponseRecorder) (rows []map[string]json.RawMessage, total int) {
	t.Helper()
	var got struct {
		Workers []map[string]json.RawMessage `json:"workers"`
		Total   int                          `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v (body %q)", err, rec.Body.String())
	}
	return got.Workers, got.Total
}

func hHasKey(row map[string]json.RawMessage, key string) bool {
	_, ok := row[key]
	return ok
}

func TestHandlerListFields(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		wantKeys []string
		banKeys  []string
	}{
		{name: "all", query: "", wantKeys: []string{"name", "phase", "containerManaged", "model", "env"}, banKeys: nil},
		{name: "spec", query: "?fields=spec", wantKeys: []string{"name", "containerManaged", "model", "team"}, banKeys: []string{"phase", "modelProvider", "env", "soul", "exposedPorts"}},
		{name: "status", query: "?fields=status", wantKeys: []string{"name", "phase", "containerState"}, banKeys: []string{"containerManaged", "model", "env"}},
		{name: "spec,status", query: "?fields=spec,status", wantKeys: []string{"name", "phase", "containerManaged", "model", "containerState"}, banKeys: []string{"env", "soul"}},
		{name: "all token", query: "?fields=all", wantKeys: []string{"name", "phase", "containerManaged"}, banKeys: nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _, _ := defaultTestHandler()
			rec := hServe(h, http.MethodGet, "/api/v1/workers"+tt.query, "", adminID(), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
			}
			rows, total := hDecodeList(t, rec)
			if total != 2 || len(rows) != 2 {
				t.Fatalf("total = %d, rows = %d", total, len(rows))
			}
			for _, key := range tt.wantKeys {
				if !hHasKey(rows[0], key) {
					t.Fatalf("row missing %q: %v", key, hRowKeys(rows[0]))
				}
			}
			for _, key := range tt.banKeys {
				if hHasKey(rows[0], key) {
					t.Fatalf("row must not carry %q in %s view: %v", key, tt.name, hRowKeys(rows[0]))
				}
			}
		})
	}

	t.Run("invalid fields 400", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodGet, "/api/v1/workers?fields=bogus", "", adminID(), nil)
		env := hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidRequest)
		if !strings.Contains(env.Message, `invalid fields value "bogus"`) {
			t.Fatalf("message = %q", env.Message)
		}
	})

	t.Run("all combined with subset 400", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodGet, "/api/v1/workers?fields=all,spec", "", adminID(), nil)
		hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidRequest)
	})

	t.Run("duplicate and reordered tokens", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodGet, "/api/v1/workers?fields=status,%20spec,spec", "", adminID(), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		rows, _ := hDecodeList(t, rec)
		if !hHasKey(rows[0], "phase") || !hHasKey(rows[0], "containerManaged") {
			t.Fatalf("row keys = %v", hRowKeys(rows[0]))
		}
	})

	t.Run("empty list encodes []", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		rec := hServe(h, http.MethodGet, "/api/v1/workers", "", adminID(), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"workers":[]`) {
			t.Fatalf("body = %q, want workers:[]", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"total":0`) {
			t.Fatalf("body = %q, want total:0", rec.Body.String())
		}
	})
}

func hRowKeys(row map[string]json.RawMessage) []string {
	out := make([]string, 0, len(row))
	for k := range row {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestHandlerListFiltering(t *testing.T) {
	t.Run("human L2 sees own team only", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodGet, "/api/v1/workers", "", humanL2ID(), nil)
		rows, total := hDecodeList(t, rec)
		if total != 1 || len(rows) != 1 {
			t.Fatalf("human list total = %d rows = %d", total, len(rows))
		}
		if name := string(rows[0]["name"]); name != `"w1"` {
			t.Fatalf("human saw %s", name)
		}
	})

	t.Run("L3 sees assigned only", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodGet, "/api/v1/workers", "", humanL3ID(), nil)
		rows, total := hDecodeList(t, rec)
		if total != 1 || len(rows) != 1 {
			t.Fatalf("L3 list total = %d rows = %d", total, len(rows))
		}
	})

	t.Run("leader sees own team only", func(t *testing.T) {
		h, _, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodGet, "/api/v1/workers", "", leaderID(), nil)
		_, total := hDecodeList(t, rec)
		if total != 1 {
			t.Fatalf("leader total = %d", total)
		}
	})

	t.Run("team query is passed to the store and applied", func(t *testing.T) {
		h, store, _, _ := defaultTestHandler()
		rec := hServe(h, http.MethodGet, "/api/v1/workers?team=team-b", "", adminID(), nil)
		_, total := hDecodeList(t, rec)
		if total != 1 {
			t.Fatalf("team-b total = %d", total)
		}
		if store.lastFilter.TeamSlug != "team-b" {
			t.Fatalf("store filter = %+v", store.lastFilter)
		}
	})
}

// --- tests: ETag ---

func TestHandlerListETag(t *testing.T) {
	h, _, _, _ := defaultTestHandler()

	first := hServe(h, http.MethodGet, "/api/v1/workers?fields=spec", "", adminID(), nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag = %q, want quoted", etag)
	}
	if got := first.Header().Get("Cache-Control"); got != httputil.ETagCacheControl {
		t.Fatalf("Cache-Control = %q, want %q", got, httputil.ETagCacheControl)
	}
	if got := first.Header().Get("Vary"); got != "Authorization" {
		t.Fatalf("Vary = %q", got)
	}

	second := hServe(h, http.MethodGet, "/api/v1/workers?fields=spec", "", adminID(),
		map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("second status = %d, want 304 (body %q)", second.Code, second.Body.String())
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 body = %q, want empty", second.Body.String())
	}
	if got := second.Header().Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want %q", got, etag)
	}

	// Non-spec projections are poll-volatile: never ETagged.
	for _, query := range []string{"?fields=status", "?fields=spec,status", ""} {
		rec := hServe(h, http.MethodGet, "/api/v1/workers"+query, "", adminID(), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", query, rec.Code)
		}
		if got := rec.Header().Get("ETag"); got != "" {
			t.Fatalf("%s ETag = %q, want none", query, got)
		}
	}

	// Invalid fields must fall through to its own 400, not 304.
	bad := hServe(h, http.MethodGet, "/api/v1/workers?fields=nope", "", adminID(),
		map[string]string{"If-None-Match": "*"})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid fields status = %d, want 400 (body %q)", bad.Code, bad.Body.String())
	}
}

// --- tests: get ---

func TestHandlerGet(t *testing.T) {
	h, _, _, aud := defaultTestHandler()
	rec := hServe(h, http.MethodGet, "/api/v1/workers/w1", "", adminID(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Fatalf("get must not carry ETag, got %q", got)
	}
	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "w1" || resp.Team != "team-a" || resp.Env["FOO"] != "bar" {
		t.Fatalf("resp = %+v", resp)
	}

	missing := hServe(h, http.MethodGet, "/api/v1/workers/w404", "", adminID(), nil)
	hAssertError(t, missing, http.StatusNotFound, httputil.CodeNotFound)

	hidden := hServe(h, http.MethodGet, "/api/v1/workers/w2", "", humanL2ID(), nil)
	hAssertError(t, hidden, http.StatusNotFound, httputil.CodeNotFound)
	calls := aud.snapshot()
	found := false
	for _, c := range calls {
		if c.Action == "worker.get" && c.Code == httputil.CodeNotFound {
			found = true
		}
	}
	if !found {
		t.Fatalf("hidden get must be audited as deny: %+v", calls)
	}
}

func TestHandlerGetStoreFailure(t *testing.T) {
	h, store, _, _ := defaultTestHandler()
	store.getErr = errors.New("db down")
	rec := hServe(h, http.MethodGet, "/api/v1/workers/w1", "", adminID(), nil)
	hAssertError(t, rec, http.StatusInternalServerError, httputil.CodeInternal)
}

func TestHandlerListStoreFailure(t *testing.T) {
	h, store, _, _ := defaultTestHandler()
	store.listErr = errors.New("db down")
	rec := hServe(h, http.MethodGet, "/api/v1/workers", "", adminID(), nil)
	hAssertError(t, rec, http.StatusInternalServerError, httputil.CodeInternal)
}

// --- tests: L3 MCP sanitization through the handlers ---

func TestHandlerL3SanitizesMCP(t *testing.T) {
	dirty := json.RawMessage(`[{"name":"m","url":"https://user:pass@mcp.example.com/mcp?api_key=secret#frag","transport":"http"}]`)

	h, store, _, _ := defaultTestHandler()
	store.rows["w1"].MCPServers = dirty

	// L3 get: query/userinfo/fragment stripped.
	rec := hServe(h, http.MethodGet, "/api/v1/workers/w1", "", humanL3ID(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("L3 get status = %d, body %q", rec.Code, rec.Body.String())
	}
	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var servers []mcpServerJSON
	if err := json.Unmarshal(resp.McpServers, &servers); err != nil {
		t.Fatalf("decode mcpServers: %v", err)
	}
	if len(servers) != 1 || servers[0].URL != "https://mcp.example.com/mcp" {
		t.Fatalf("L3 get mcpServers = %s", resp.McpServers)
	}

	// L3 list: same reduction.
	rec = hServe(h, http.MethodGet, "/api/v1/workers", "", humanL3ID(), nil)
	rows, _ := hDecodeList(t, rec)
	if len(rows) != 1 {
		t.Fatalf("L3 list rows = %d", len(rows))
	}
	if strings.Contains(rec.Body.String(), "api_key=secret") || strings.Contains(rec.Body.String(), "user:pass") {
		t.Fatalf("L3 list leaked the MCP credentials: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"url":"https://mcp.example.com/mcp"`) {
		t.Fatalf("L3 list did not carry the sanitized URL: %s", rec.Body.String())
	}

	// Admin keeps the verbatim stored bytes.
	rec = hServe(h, http.MethodGet, "/api/v1/workers/w1", "", adminID(), nil)
	if !strings.Contains(rec.Body.String(), "api_key=secret") {
		t.Fatalf("admin must see the verbatim MCP URL: %s", rec.Body.String())
	}
}

// --- tests: no identity -> 401 for all routes is covered above ---

func TestHandlerNilAuditor(t *testing.T) {
	h, _, _, _ := defaultTestHandler()
	h.Auditor = nil
	rec := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"Bad_Name"}`, adminID(), nil)
	hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidName)

	h.Log = nil
	rec = hServe(h, http.MethodGet, "/api/v1/workers", "", nil, nil)
	hAssertError(t, rec, http.StatusUnauthorized, httputil.CodeUnauthenticated)
}

func TestHandlerMethodRouting(t *testing.T) {
	h, _, _, _ := defaultTestHandler()
	// Only the three frozen routes exist; PUT stays with 2c's proxy.
	rec := hServe(h, http.MethodPut, "/api/v1/workers/w1", `{}`, adminID(), nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status = %d, want 405", rec.Code)
	}
}

// --- tests: get-by-name cache-aside refresh ---

type refreshStore struct {
	*fakeStore
	updated int
	deleted int
}

func (r *refreshStore) UpdateStatus(context.Context, string, Observed, int64) (bool, error) {
	r.updated++
	return true, nil
}

func (r *refreshStore) SoftDelete(context.Context, string, int64, *AuditMeta) error {
	r.deleted++
	return nil
}

func newRefreshHandler(t *testing.T, resolve ExecutionResolver) (*Handler, *refreshStore) {
	t.Helper()
	st := &refreshStore{fakeStore: newFakeStore(hSeedWorker("w1", "team-a"))}
	return &Handler{
		Store:            st,
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		ResolveExecution: resolve,
	}, st
}

func TestHandlerGetRefreshesFromController(t *testing.T) {
	t.Run("newer observed state is applied", func(t *testing.T) {
		h, st := newRefreshHandler(t, func(context.Context, string) (Observed, int64, bool, error) {
			return Observed{Phase: "Running", ContainerState: "running", AgentStatus: "idle"}, 2, true, nil
		})
		rec := hServe(h, http.MethodGet, "/api/v1/workers/w1", "", adminID(), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"phase":"Running"`) {
			t.Fatalf("refreshed phase not visible: %s", rec.Body.String())
		}
		if st.updated != 1 {
			t.Fatalf("UpdateStatus calls = %d, want 1", st.updated)
		}
	})

	t.Run("controller 404 soft-deletes and returns 404", func(t *testing.T) {
		h, st := newRefreshHandler(t, func(context.Context, string) (Observed, int64, bool, error) {
			return Observed{}, 0, false, nil
		})
		rec := hServe(h, http.MethodGet, "/api/v1/workers/w1", "", adminID(), nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not_found") {
			t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
		}
		if st.deleted != 1 {
			t.Fatalf("SoftDelete calls = %d, want 1", st.deleted)
		}
	})

	t.Run("controller unavailable falls back to the row", func(t *testing.T) {
		h, st := newRefreshHandler(t, func(context.Context, string) (Observed, int64, bool, error) {
			return Observed{}, 0, false, errors.New("controller down")
		})
		rec := hServe(h, http.MethodGet, "/api/v1/workers/w1", "", adminID(), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
		}
		if st.updated != 0 || st.deleted != 0 {
			t.Fatalf("unexpected store mutation: updated=%d deleted=%d", st.updated, st.deleted)
		}
	})
}

func TestHandlerIdempotencyScopedToPrincipal(t *testing.T) {
	h, store, idem, _ := defaultTestHandler()
	headers := map[string]string{"Idempotency-Key": "shared-key"}
	first := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-admin"}`, adminID(), headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("admin create = %d %s", first.Code, first.Body.String())
	}
	second := hServe(h, http.MethodPost, "/api/v1/workers", `{"name":"w-manager"}`, managerID(), headers)
	if second.Code != http.StatusCreated {
		t.Fatalf("manager same key must not replay admin: %d %s", second.Code, second.Body.String())
	}
	if store.createCalls != 2 || idem.putCalls != 2 {
		t.Fatalf("create=%d put=%d, want 2/2", store.createCalls, idem.putCalls)
	}
}
