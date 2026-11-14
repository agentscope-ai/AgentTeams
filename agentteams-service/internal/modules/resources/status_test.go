package resources

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
)

// --- fake store ---

// statusStore records UpdateStatus calls and reports the configured
// updated/discarded outcome. A status callback must never touch any other
// Store method, so the rest fail the test.

type statusCall struct {
	name       string
	observed   Observed
	generation int64
}

type statusStore struct {
	calls   []statusCall
	updated map[string]bool
	failOn  string
	failure error
}

func newStatusStore(updated map[string]bool) *statusStore {
	return &statusStore{updated: updated}
}

var errStatusFakeUnused = errors.New("status test store: method not used by the writeback")

func (s *statusStore) Create(context.Context, *Worker, *AuditMeta) (*Worker, error) {
	return nil, errStatusFakeUnused
}

func (s *statusStore) Get(context.Context, string) (*Worker, error) {
	return nil, errStatusFakeUnused
}

func (s *statusStore) List(context.Context, ListFilter) ([]*Worker, error) {
	return nil, errStatusFakeUnused
}

func (s *statusStore) SyncSpec(context.Context, string, Spec, int64, *AuditMeta) (*Worker, error) {
	return nil, errors.New("status test store: SyncSpec not implemented")
}

func (s *statusStore) UpdateSpec(context.Context, string, Spec, int64, *AuditMeta) (*Worker, error) {
	return nil, errStatusFakeUnused
}

func (s *statusStore) UpdateStatus(_ context.Context, name string, o Observed, generation int64) (bool, error) {
	s.calls = append(s.calls, statusCall{name: name, observed: o, generation: generation})
	if s.failure != nil && name == s.failOn {
		return false, s.failure
	}
	return s.updated[name], nil
}

func (s *statusStore) SoftDelete(context.Context, string, int64, *AuditMeta) error {
	return errStatusFakeUnused
}

// --- helpers ---

func newTestStatusHandler(store Store) *StatusHandler {
	return &StatusHandler{
		Store: store,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:   func() time.Time { return hTestNow },
	}
}

func statusServe(h *StatusHandler, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/workers/status", strings.NewReader(body)))
	return rec
}

// --- tests ---

func TestStatusHandlerBatch(t *testing.T) {
	store := newStatusStore(map[string]bool{"w1": true, "w2": false})
	h := newTestStatusHandler(store)

	body := `{"workers":[` +
		`{"name":"w1","generation":3,"phase":"Running","containerState":"running","matrixUserID":"@w1:x","roomID":"!r:x","message":"","lastActiveAt":"2026-11-13T00:00:00Z","agentStatus":"idle","runningTaskCount":2,"lastRunAt":"2026-11-13T00:01:00.5Z","lastFinishAt":""},` +
		`{"name":"w2","generation":9,"phase":"Pending"}]}`
	rec := statusServe(h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if want := `{"updated":1,"discarded":1}` + "\n"; rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
	if len(store.calls) != 2 {
		t.Fatalf("UpdateStatus calls = %d, want 2", len(store.calls))
	}

	first := store.calls[0]
	if first.name != "w1" || first.generation != 3 {
		t.Fatalf("call = (%q, %d), want (w1, 3)", first.name, first.generation)
	}
	o := first.observed
	if o.Phase != "Running" || o.ContainerState != "running" || o.MatrixUserID != "@w1:x" || o.RoomID != "!r:x" || o.Message != "" {
		t.Fatalf("observed text fields = %+v", o)
	}
	if o.AgentStatus != "idle" {
		t.Fatalf("agent status = %q, want idle", o.AgentStatus)
	}
	if o.RunningTaskCount == nil || *o.RunningTaskCount != 2 {
		t.Fatalf("running task count = %v, want 2", o.RunningTaskCount)
	}
	wantActive := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if o.LastActiveAt == nil || !o.LastActiveAt.Equal(wantActive) {
		t.Fatalf("last active = %v, want %v", o.LastActiveAt, wantActive)
	}
	wantRun := time.Date(2026, 11, 13, 0, 1, 0, 500000000, time.UTC)
	if o.LastRunAt == nil || !o.LastRunAt.Equal(wantRun) {
		t.Fatalf("last run = %v, want %v", o.LastRunAt, wantRun)
	}
	if o.LastFinishAt != nil {
		t.Fatalf("last finish = %v, want nil for an empty timestamp", o.LastFinishAt)
	}

	second := store.calls[1]
	if second.name != "w2" || second.generation != 9 {
		t.Fatalf("call = (%q, %d), want (w2, 9)", second.name, second.generation)
	}
	if second.observed.Phase != "Pending" || second.observed.LastActiveAt != nil {
		t.Fatalf("second observed = %+v, want only the phase", second.observed)
	}
}

func TestStatusHandlerInvalidJSON(t *testing.T) {
	store := newStatusStore(nil)
	rec := statusServe(newTestStatusHandler(store), `{"workers":[`)
	hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidRequest)
	if len(store.calls) != 0 {
		t.Fatal("store called for an undecodable body")
	}
}

func TestStatusHandlerInvalidTimestamp(t *testing.T) {
	store := newStatusStore(nil)
	rec := statusServe(newTestStatusHandler(store), `{"workers":[{"name":"w1","generation":1,"phase":"Running","lastActiveAt":"yesterday"}]}`)
	env := hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidRequest)
	if !strings.Contains(env.Message, "lastActiveAt") {
		t.Fatalf("message = %q, want it to name lastActiveAt", env.Message)
	}
	if len(store.calls) != 0 {
		t.Fatal("store called although validation failed; the batch must be all-or-nothing")
	}
}

func TestStatusHandlerMissingName(t *testing.T) {
	store := newStatusStore(nil)
	rec := statusServe(newTestStatusHandler(store), `{"workers":[{"generation":1,"phase":"Running"}]}`)
	env := hAssertError(t, rec, http.StatusBadRequest, httputil.CodeInvalidRequest)
	if !strings.Contains(env.Message, "name") {
		t.Fatalf("message = %q, want it to name the missing field", env.Message)
	}
	if len(store.calls) != 0 {
		t.Fatal("store called for a nameless report")
	}
}

func TestStatusHandlerStoreFailureStopsBatch(t *testing.T) {
	store := newStatusStore(map[string]bool{"w1": true, "w2": true})
	store.failOn = "w1"
	store.failure = errors.New("db down")
	rec := statusServe(newTestStatusHandler(store), `{"workers":[{"name":"w1","generation":1,"phase":"Running"},{"name":"w2","generation":2,"phase":"Pending"}]}`)
	hAssertError(t, rec, http.StatusInternalServerError, httputil.CodeInternal)
	if len(store.calls) != 1 {
		t.Fatalf("UpdateStatus calls = %d, want 1 (the batch stops on the first failure)", len(store.calls))
	}
}

func TestStatusHandlerEmptyBatch(t *testing.T) {
	store := newStatusStore(nil)
	rec := statusServe(newTestStatusHandler(store), `{"workers":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if want := `{"updated":0,"discarded":0}` + "\n"; rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}
