package pc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
)

// InMemory is an in-memory PC backend: full memory.Store semantics
// (CAS, tombstone forget/restore, idempotency, scope isolation) with
// deterministic sequential ids. It plugs directly into the contract
// suite and the HTTP layer.
type InMemory struct {
	mu      sync.Mutex
	entries map[string]*memEntry
	idem    map[string]*idemRecord
	nextID  int
	now     func() time.Time
}

type memEntry struct {
	e       memory.Entry
	deleted bool
}

type idemRecord struct {
	entryID     string
	payloadHash string
}

var (
	_ memory.Store = (*InMemory)(nil)
	_ Adapter      = (*InMemory)(nil)
)

// NewInMemory returns an empty backend.
func NewInMemory() *InMemory {
	return &InMemory{entries: map[string]*memEntry{}, idem: map[string]*idemRecord{}, now: time.Now}
}

// SetNow injects a clock (tests).
func (m *InMemory) SetNow(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// Peek returns the stored entry regardless of tombstone state, for
// the fake server's 428/412 current bodies. Not part of memory.Store.
func (m *InMemory) Peek(id string) (memory.Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[id]; ok {
		return e.e, true
	}
	return memory.Entry{}, false
}

// --- memory.Store ---------------------------------------------------

func (m *InMemory) Create(_ context.Context, in memory.NewEntry) (memory.Entry, error) {
	if !in.Anchor.Valid() {
		return memory.Entry{}, memory.ErrInvalidAnchor
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if in.IdempotencyKey.IsSet() {
		if rec, ok := m.idem[string(in.IdempotencyKey)]; ok {
			e, exists := m.entries[rec.entryID]
			if !exists || e.deleted {
				return memory.Entry{}, memory.ErrNotFound
			}
			if memory.PayloadHash(in) != rec.payloadHash {
				return memory.Entry{}, &memory.ErrConflict{Current: e.e}
			}
			return e.e, nil
		}
	}

	now := m.now()
	m.nextID++
	id := fmt.Sprintf("pcim-%04d", m.nextID)
	entry := memory.Entry{
		ID:          id,
		Anchor:      in.Anchor,
		ContentType: in.ContentType,
		Text:        in.Text,
		Revision:    1,
		ETag:        memory.ETagForRevision(1),
		Provenance:  in.Provenance,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	m.entries[id] = &memEntry{e: entry}
	if in.IdempotencyKey.IsSet() {
		m.idem[string(in.IdempotencyKey)] = &idemRecord{entryID: id, payloadHash: memory.PayloadHash(in)}
	}
	return entry, nil
}

func (m *InMemory) Replace(_ context.Context, id string, ifRevision uint64, patch memory.Patch) (memory.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok || e.deleted {
		return memory.Entry{}, memory.ErrNotFound
	}
	if e.e.Revision != ifRevision {
		return memory.Entry{}, &memory.ErrConflict{Current: e.e}
	}
	applyPatch(&e.e, ifRevision, patch, m.now())
	return e.e, nil
}

func (m *InMemory) Forget(_ context.Context, id string, ifRevision uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok || e.deleted {
		return memory.ErrNotFound
	}
	if e.e.Revision != ifRevision {
		return &memory.ErrConflict{Current: e.e}
	}
	e.deleted = true
	bump(&e.e, ifRevision, m.now())
	return nil
}

func (m *InMemory) Restore(_ context.Context, id string, ifRevision uint64) (memory.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok || !e.deleted {
		return memory.Entry{}, memory.ErrNotFound
	}
	if e.e.Revision != ifRevision {
		return memory.Entry{}, &memory.ErrConflict{Current: e.e}
	}
	e.deleted = false
	bump(&e.e, ifRevision, m.now())
	return e.e, nil
}

func (m *InMemory) Search(_ context.Context, q memory.SearchQuery) ([]memory.Entry, error) {
	if !q.Anchor.Valid() {
		return nil, memory.ErrInvalidAnchor
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	needle := strings.ToLower(q.Query)
	out := []memory.Entry{}
	for _, e := range m.entries {
		if e.deleted || e.e.Anchor != q.Anchor {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.e.Text), needle) {
			continue
		}
		out = append(out, e.e)
	}
	sortEntries(out)
	return out, nil
}

func (m *InMemory) Export(_ context.Context, anchor memory.Anchor) ([]memory.Entry, error) {
	if !anchor.Valid() {
		return nil, memory.ErrInvalidAnchor
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []memory.Entry{}
	for _, e := range m.entries {
		if e.deleted || e.e.Anchor != anchor {
			continue
		}
		out = append(out, e.e)
	}
	sortEntries(out)
	return out, nil
}

// applyPatch advances the entry to ifRevision+1 applying patch.
func applyPatch(e *memory.Entry, ifRevision uint64, patch memory.Patch, now time.Time) {
	if patch.ContentType != nil {
		e.ContentType = *patch.ContentType
	}
	if patch.Text != nil {
		e.Text = *patch.Text
	}
	if patch.Provenance != nil {
		e.Provenance = *patch.Provenance
	}
	bump(e, ifRevision, now)
}

// bump advances the revision and refreshes the ETag/timestamp.
func bump(e *memory.Entry, ifRevision uint64, now time.Time) {
	e.Revision = ifRevision + 1
	e.ETag = memory.ETagForRevision(e.Revision)
	e.UpdatedAt = now
}

// sortEntries orders by creation time, then ID, ascending.
func sortEntries(entries []memory.Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.Before(entries[j].CreatedAt)
		}
		return entries[i].ID < entries[j].ID
	})
}

// --- FakePC: HTTP fake with record/replay ---------------------------

// Record is one captured request.
type Record struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Override is a one-shot response injected for a (method, path).
type Override struct {
	Status int
	Body   any
}

// FakePC is an httptest server implementing the PC wire protocol on
// top of InMemory state, recording every request and supporting
// one-shot response overrides (e.g. force a 412 with a chosen
// current entry) for error-mapping tests.
type FakePC struct {
	*httptest.Server
	state     *InMemory
	mu        sync.Mutex
	records   []Record
	overrides map[string]Override
}

// NewFakePC starts a fake PC server for t (closed via t.Cleanup).
func NewFakePC(t *testing.T) *FakePC {
	t.Helper()
	f := &FakePC{state: NewInMemory(), overrides: map[string]Override{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /entries", f.handleCreate)
	mux.HandleFunc("PUT /entries/{id}", f.handleReplace)
	mux.HandleFunc("DELETE /entries/{id}", f.handleForget)
	mux.HandleFunc("POST /entries/{id}/restore", f.handleRestore)
	mux.HandleFunc("GET /entries", f.handleSearch)
	mux.HandleFunc("GET /export", f.handleExport)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// Client returns a *Client pointed at the fake.
func (f *FakePC) Client() *Client {
	return NewClientWithHTTP(f.Server.URL, f.Server.Client())
}

// State exposes the backing InMemory state.
func (f *FakePC) State() *InMemory { return f.state }

// Records returns a copy of the captured requests.
func (f *FakePC) Records() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.records...)
}

// SetOverride forces the next response for (method, path) to
// status/body. One-shot: the second call hits the real state.
func (f *FakePC) SetOverride(method, path string, status int, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overrides[method+" "+path] = Override{Status: status, Body: body}
}

func (f *FakePC) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	f.mu.Lock()
	f.records = append(f.records, Record{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	f.mu.Unlock()
}

func (f *FakePC) takeOverride(method, path string) (Override, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := method + " " + path
	if o, ok := f.overrides[key]; ok {
		delete(f.overrides, key)
		return o, true
	}
	return Override{}, false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		// A marshal failure must not leave the caller with an empty body and
		// a success status: report it explicitly.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"fake_encode_failed","message":"fake response could not be encoded"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writePCError(w http.ResponseWriter, err error) {
	switch {
	case memory.IsConflict(err):
		c := memory.ConflictOf(err)
		writeJSON(w, http.StatusPreconditionFailed, errorResponse{
			Code:    "revision_conflict",
			Message: "revision conflict",
			Current: &c.Current,
		})
	case errors.Is(err, memory.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Code: "not_found", Message: "entry not found"})
	case errors.Is(err, memory.ErrForbidden):
		writeJSON(w, http.StatusForbidden, errorResponse{Code: "forbidden", Message: "access forbidden"})
	default:
		writeJSON(w, http.StatusInternalServerError, errorResponse{Code: "internal", Message: "internal error"})
	}
}

// writePreconditionMissing renders 428 with the current entry when
// the id is known (live or forgotten), 404 otherwise.
func (f *FakePC) writePreconditionMissing(w http.ResponseWriter, id string) {
	if e, ok := f.state.Peek(id); ok {
		writeJSON(w, http.StatusPreconditionRequired, errorResponse{
			Code:    "precondition_required",
			Message: "If-Match header is required",
			Current: &e,
		})
		return
	}
	writeJSON(w, http.StatusNotFound, errorResponse{Code: "not_found", Message: "entry not found"})
}

// requireIfMatch parses the If-Match header or renders the 428/404.
func (f *FakePC) requireIfMatch(w http.ResponseWriter, r *http.Request, id string) (uint64, bool) {
	revision, ok := memory.ParseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		f.writePreconditionMissing(w, id)
		return 0, false
	}
	return revision, true
}

func (f *FakePC) handleCreate(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	if o, ok := f.takeOverride(r.Method, r.URL.Path); ok {
		writeJSON(w, o.Status, o.Body)
		return
	}
	var req createRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "bad_request", Message: "malformed create request"})
		return
	}
	key, err := memory.ParseIdempotencyKey(r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "bad_request", Message: "invalid Idempotency-Key"})
		return
	}
	entry, err := f.state.Create(r.Context(), memory.NewEntry{
		Anchor:         req.Anchor,
		ContentType:    req.ContentType,
		Text:           req.Text,
		Provenance:     req.Provenance,
		IdempotencyKey: key,
	})
	if err != nil {
		writePCError(w, err)
		return
	}
	w.Header().Set("ETag", entry.ETag)
	writeJSON(w, http.StatusCreated, entry)
}

func (f *FakePC) handleReplace(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	if o, ok := f.takeOverride(r.Method, r.URL.Path); ok {
		writeJSON(w, o.Status, o.Body)
		return
	}
	id := r.PathValue("id")
	revision, ok := f.requireIfMatch(w, r, id)
	if !ok {
		return
	}
	var req replaceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "bad_request", Message: "malformed replace request"})
		return
	}
	entry, err := f.state.Replace(r.Context(), id, revision, memory.Patch{
		ContentType: req.ContentType,
		Text:        req.Text,
		Provenance:  req.Provenance,
	})
	if err != nil {
		writePCError(w, err)
		return
	}
	w.Header().Set("ETag", entry.ETag)
	writeJSON(w, http.StatusOK, entry)
}

func (f *FakePC) handleForget(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	if o, ok := f.takeOverride(r.Method, r.URL.Path); ok {
		writeJSON(w, o.Status, o.Body)
		return
	}
	id := r.PathValue("id")
	revision, ok := f.requireIfMatch(w, r, id)
	if !ok {
		return
	}
	if err := f.state.Forget(r.Context(), id, revision); err != nil {
		writePCError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *FakePC) handleRestore(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	if o, ok := f.takeOverride(r.Method, r.URL.Path); ok {
		writeJSON(w, o.Status, o.Body)
		return
	}
	id := r.PathValue("id")
	revision, ok := f.requireIfMatch(w, r, id)
	if !ok {
		return
	}
	entry, err := f.state.Restore(r.Context(), id, revision)
	if err != nil {
		writePCError(w, err)
		return
	}
	w.Header().Set("ETag", entry.ETag)
	writeJSON(w, http.StatusOK, entry)
}

func (f *FakePC) handleSearch(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	if o, ok := f.takeOverride(r.Method, r.URL.Path); ok {
		writeJSON(w, o.Status, o.Body)
		return
	}
	anchor, err := memory.ParseAnchor(r.URL.Query().Get("anchor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "bad_request", Message: "invalid anchor"})
		return
	}
	entries, err := f.state.Search(r.Context(), memory.SearchQuery{Anchor: anchor, Query: r.URL.Query().Get("q")})
	if err != nil {
		writePCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entriesResponse{Entries: entries})
}

func (f *FakePC) handleExport(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	if o, ok := f.takeOverride(r.Method, r.URL.Path); ok {
		writeJSON(w, o.Status, o.Body)
		return
	}
	anchor, err := memory.ParseAnchor(r.URL.Query().Get("anchor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "bad_request", Message: "invalid anchor"})
		return
	}
	entries, err := f.state.Export(r.Context(), anchor)
	if err != nil {
		writePCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entriesResponse{Entries: entries})
}
