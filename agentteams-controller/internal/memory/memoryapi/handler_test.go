package memoryapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory/pc"
)

// stubStore injects a fixed error into every operation.
type stubStore struct{ err error }

func (s *stubStore) Create(context.Context, memory.NewEntry) (memory.Entry, error) {
	return memory.Entry{}, s.err
}
func (s *stubStore) Replace(context.Context, string, uint64, memory.Patch) (memory.Entry, error) {
	return memory.Entry{}, s.err
}
func (s *stubStore) Forget(context.Context, string, uint64) error { return s.err }
func (s *stubStore) Restore(context.Context, string, uint64) (memory.Entry, error) {
	return memory.Entry{}, s.err
}
func (s *stubStore) Search(context.Context, memory.SearchQuery) ([]memory.Entry, error) {
	return nil, s.err
}
func (s *stubStore) Export(context.Context, memory.Anchor) ([]memory.Entry, error) {
	return nil, s.err
}

var _ memory.Store = (*stubStore)(nil)

func doReq(t *testing.T, h http.Handler, method, path, raw string, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	var body io.Reader
	if raw != "" {
		body = strings.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), rec.Result().Header
}

func ifMatch(rev uint64) map[string]string {
	return map[string]string{"If-Match": memory.ETagForRevision(rev)}
}

func TestBadAnchors(t *testing.T) {
	srv := New(pc.NewInMemory())
	for _, path := range []string{
		"/memory/robot:x",  // unknown kind
		"/memory/alice",    // missing kind
		"/memory/human:",   // missing id
		"/memory/Team:t1",  // kind is case-sensitive
	} {
		status, body, _ := doReq(t, srv.Handler(), http.MethodPost, path,
			`{"content_type":"fact","text":"x"}`, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("POST %s = %d (body %s), want 400", path, status, body)
		}
	}
	status, body, _ := doReq(t, srv.Handler(), http.MethodGet, "/memory/Team:t1", "", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("GET bad anchor = %d (body %s), want 400", status, body)
	}
}

func TestCreateValidationAndETag(t *testing.T) {
	srv := New(pc.NewInMemory())

	status, body, _ := doReq(t, srv.Handler(), http.MethodPost, "/memory/team:t1",
		`{"content_type":"fact","text":""}`, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("empty text = %d (body %s), want 400", status, body)
	}
	status, body, _ = doReq(t, srv.Handler(), http.MethodPost, "/memory/team:t1",
		`{"content_type":"","text":"x"}`, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("empty content_type = %d (body %s), want 400", status, body)
	}

	status, body, header := doReq(t, srv.Handler(), http.MethodPost, "/memory/team:t1",
		`{"content_type":"fact","text":"hello"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create = %d (body %s), want 201", status, body)
	}
	var e memory.Entry
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if got := header.Get("ETag"); got != e.ETag || got != `"1"` {
		t.Fatalf("ETag header = %q, want the entry etag \"1\"", got)
	}
}

func TestIfMatchRules(t *testing.T) {
	store := pc.NewInMemory()
	srv := New(store)
	ctx := context.Background()
	e, err := store.Create(ctx, memory.NewEntry{
		Anchor: memory.Anchor{Kind: memory.KindTeam, ID: "t1"}, ContentType: "fact", Text: "base",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	base := "/memory/team:t1/" + e.ID

	// missing If-Match on every write → 428
	status, body, _ := doReq(t, srv.Handler(), http.MethodPut, base, `{"text":"x"}`, nil)
	if status != http.StatusPreconditionRequired {
		t.Fatalf("PUT without If-Match = %d (body %s), want 428", status, body)
	}
	status, _, _ = doReq(t, srv.Handler(), http.MethodDelete, base, "", nil)
	if status != http.StatusPreconditionRequired {
		t.Fatalf("DELETE without If-Match = %d, want 428", status)
	}
	status, _, _ = doReq(t, srv.Handler(), http.MethodPost, base+"/restore", "", nil)
	if status != http.StatusPreconditionRequired {
		t.Fatalf("restore without If-Match = %d, want 428", status)
	}

	// unparsable If-Match → 428
	for _, v := range []string{"abc", "*"} {
		status, _, _ = doReq(t, srv.Handler(), http.MethodPut, base, `{"text":"x"}`,
			map[string]string{"If-Match": v})
		if status != http.StatusPreconditionRequired {
			t.Fatalf("PUT If-Match %q = %d, want 428", v, status)
		}
	}

	// advance to rev 2, then a stale write → 409
	updated, err := store.Replace(ctx, e.ID, 1, memory.Patch{Text: strPtr("v2")})
	if err != nil || updated.Revision != 2 {
		t.Fatalf("advance: %v (entry %+v)", err, updated)
	}
	status, body, _ = doReq(t, srv.Handler(), http.MethodPut, base, `{"text":"stale"}`, ifMatch(1))
	if status != http.StatusConflict {
		t.Fatalf("stale PUT = %d (body %s), want 409", status, body)
	}

	// unknown id with a valid If-Match → 404
	status, body, _ = doReq(t, srv.Handler(), http.MethodPut, "/memory/team:t1/nope",
		`{"text":"x"}`, ifMatch(1))
	if status != http.StatusNotFound {
		t.Fatalf("PUT unknown id = %d (body %s), want 404", status, body)
	}

	// forget → 204; restore at the tombstone revision → 200 at rev 3
	status, _, _ = doReq(t, srv.Handler(), http.MethodDelete, base, "", ifMatch(2))
	if status != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", status)
	}
	status, body, _ = doReq(t, srv.Handler(), http.MethodPost, base+"/restore", "", ifMatch(3))
	if status != http.StatusOK {
		t.Fatalf("restore = %d (body %s), want 200", status, body)
	}
	var er memory.Entry
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatalf("decode restored: %v", err)
	}
	if er.Revision != 4 || er.Text != "v2" {
		t.Fatalf("restored = %+v, want revision 4 with the pre-forget text", er)
	}
}

func TestSearchAndExport(t *testing.T) {
	store := pc.NewInMemory()
	srv := New(store)
	ctx := context.Background()
	for _, text := range []string{"alpha note", "beta note"} {
		if _, err := store.Create(ctx, memory.NewEntry{
			Anchor: memory.Anchor{Kind: memory.KindAgent, ID: "a1"}, ContentType: "fact", Text: text,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// case-insensitive substring
	status, body, _ := doReq(t, srv.Handler(), http.MethodGet, "/memory/agent:a1?q=ALPHA", "", nil)
	if status != http.StatusOK {
		t.Fatalf("search = %d (body %s), want 200", status, body)
	}
	var env struct {
		Entries []memory.Entry `json:"entries"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(env.Entries) != 1 || env.Entries[0].Text != "alpha note" {
		t.Fatalf("search = %+v, want only the alpha entry", env.Entries)
	}

	status, body, _ = doReq(t, srv.Handler(), http.MethodGet, "/memory/agent:a1/export", "", nil)
	if status != http.StatusOK {
		t.Fatalf("export = %d (body %s), want 200", status, body)
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(env.Entries) != 2 {
		t.Fatalf("export = %d entries, want 2", len(env.Entries))
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		want  int
		code  string
		check func(t *testing.T, body []byte)
	}{
		{"forbidden", memory.ErrForbidden, http.StatusForbidden, "forbidden", nil},
		{"not found", memory.ErrNotFound, http.StatusNotFound, "not_found", nil},
		{"conflict", &memory.ErrConflict{Current: memory.Entry{
			Revision: 4, ETag: `"4"`, Text: "winner",
		}}, http.StatusConflict, "conflict", func(t *testing.T, body []byte) {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("decode 409: %v", err)
			}
			cur, ok := m["current"].(map[string]any)
			if !ok || cur["revision"] != float64(4) || cur["etag"] != `"4"` || cur["text"] != "winner" {
				t.Fatalf("409 current = %v, want the conflict's current entry", m["current"])
			}
		}},
		{"internal", errors.New("boom token=secret123"), http.StatusInternalServerError, "internal", func(t *testing.T, body []byte) {
			// the body must be the exact static envelope, with no
			// trace of the store error
			s := string(body)
			if strings.Contains(s, "boom") || strings.Contains(s, "secret123") {
				t.Fatalf("500 body leaks the store error: %s", s)
			}
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("decode 500: %v", err)
			}
			if m["code"] != "internal" || m["message"] != "internal error" {
				t.Fatalf("500 body = %v, want the static internal envelope", m)
			}
			if _, hasCurrent := m["current"]; hasCurrent {
				t.Fatalf("500 body must not carry a current view: %s", s)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(&stubStore{err: tc.err})
			var status int
			var body []byte
			if tc.want == http.StatusConflict || tc.err == memory.ErrNotFound {
				// exercise through a write route
				var statusOut int
				var bodyOut []byte
				statusOut, bodyOut, _ = doReq(t, srv.Handler(), http.MethodPut,
					"/memory/team:t1/id-1", `{"text":"x"}`, ifMatch(1))
				status, body = statusOut, bodyOut
			} else {
				status, body, _ = doReq(t, srv.Handler(), http.MethodGet,
					"/memory/team:t1/export", "", nil)
			}
			if status != tc.want {
				t.Fatalf("status = %d (body %s), want %d", status, body, tc.want)
			}
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if m["code"] != tc.code {
				t.Fatalf("code = %v, want %q", m["code"], tc.code)
			}
			if tc.check != nil {
				tc.check(t, body)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
