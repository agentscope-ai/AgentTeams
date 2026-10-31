// Package contract is the #1412 W0a behaviour contract: the same
// table-driven suite runs against every memory.Store backend — the
// SQLite file store and the PowerContext adapter (in-memory fake,
// plus the HTTP fake over httptest). The tests are the deliverable.
package contract

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory/memoryapi"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory/pc"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory/sqlite"
)

// --- harness ---------------------------------------------------------

type harness struct {
	name   string
	store  memory.Store
	server *memoryapi.Server
	fakePC *pc.FakePC // non-nil only for pc-fake-http
}

func newHarness(t *testing.T, name string) *harness {
	t.Helper()
	var store memory.Store
	var fakePC *pc.FakePC
	switch name {
	case "sqlite":
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "memory.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		store = s
	case "pc-fake-inmemory":
		store = pc.NewInMemory()
	case "pc-fake-http":
		fakePC = pc.NewFakePC(t)
		store = fakePC.Client()
	default:
		t.Fatalf("unknown backend %q", name)
	}
	return &harness{name: name, store: store, server: memoryapi.New(store), fakePC: fakePC}
}

type response struct {
	status int
	body   []byte
	header http.Header
}

func serveRequest(t *testing.T, h *harness, method, path, raw string, headers map[string]string) response {
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
	h.server.Handler().ServeHTTP(rec, req)
	return response{status: rec.Code, body: rec.Body.Bytes(), header: rec.Result().Header}
}

func (h *harness) doJSON(t *testing.T, method, path string, headers map[string]string, body any) response {
	t.Helper()
	var raw string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		raw = string(b)
	}
	return serveRequest(t, h, method, path, raw, headers)
}

func (h *harness) doRaw(t *testing.T, method, path string, headers map[string]string, raw string) response {
	t.Helper()
	return serveRequest(t, h, method, path, raw, headers)
}

func requireStatus(t *testing.T, resp response, want int, ctx string) {
	t.Helper()
	if resp.status != want {
		t.Fatalf("%s: status = %d, want %d (body: %s)", ctx, resp.status, want, resp.body)
	}
}

func entry(t *testing.T, resp response) memory.Entry {
	t.Helper()
	var e memory.Entry
	if err := json.Unmarshal(resp.body, &e); err != nil {
		t.Fatalf("decode entry: %v (body: %s)", err, resp.body)
	}
	return e
}

type conflictBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Current struct {
		Revision uint64 `json:"revision"`
		ETag     string `json:"etag"`
		Text     string `json:"text"`
	} `json:"current"`
}

func conflictBodyOf(t *testing.T, resp response) conflictBody {
	t.Helper()
	var c conflictBody
	if err := json.Unmarshal(resp.body, &c); err != nil {
		t.Fatalf("decode conflict body: %v (body: %s)", err, resp.body)
	}
	return c
}

func bodyMap(t *testing.T, resp response) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(resp.body, &m); err != nil {
		t.Fatalf("decode body as object: %v (body: %s)", err, resp.body)
	}
	return m
}

func (h *harness) createEntry(t *testing.T, anchor, contentType, text, idemKey string) memory.Entry {
	t.Helper()
	headers := map[string]string{}
	if idemKey != "" {
		headers["Idempotency-Key"] = idemKey
	}
	resp := h.doJSON(t, http.MethodPost, "/memory/"+anchor, headers, map[string]any{
		"content_type": contentType,
		"text":         text,
	})
	requireStatus(t, resp, http.StatusCreated, "create "+anchor)
	return entry(t, resp)
}

func (h *harness) search(t *testing.T, anchor, q string) []memory.Entry {
	t.Helper()
	path := "/memory/" + anchor
	if q != "" {
		path += "?q=" + url.QueryEscape(q)
	}
	resp := h.doJSON(t, http.MethodGet, path, nil, nil)
	requireStatus(t, resp, http.StatusOK, "search "+anchor)
	var env struct {
		Entries []memory.Entry `json:"entries"`
	}
	if err := json.Unmarshal(resp.body, &env); err != nil {
		t.Fatalf("decode search envelope: %v (body: %s)", err, resp.body)
	}
	return env.Entries
}

func (h *harness) export(t *testing.T, anchor string) []memory.Entry {
	t.Helper()
	resp := h.doJSON(t, http.MethodGet, "/memory/"+anchor+"/export", nil, nil)
	requireStatus(t, resp, http.StatusOK, "export "+anchor)
	var env struct {
		Entries []memory.Entry `json:"entries"`
	}
	if err := json.Unmarshal(resp.body, &env); err != nil {
		t.Fatalf("decode export envelope: %v (body: %s)", err, resp.body)
	}
	return env.Entries
}

func memPath(anchor, id string, suffix ...string) string {
	p := "/memory/" + anchor
	if id != "" {
		p += "/" + id
	}
	for _, s := range suffix {
		p += s
	}
	return p
}

func ifMatch(rev uint64) map[string]string {
	return map[string]string{"If-Match": memory.ETagForRevision(rev)}
}

func idemKey(k string) map[string]string {
	return map[string]string{"Idempotency-Key": k}
}

func strPtr(s string) *string { return &s }

// --- cases -----------------------------------------------------------

type caseSpec struct {
	name    string
	backend string // "" = all backends
	run     func(t *testing.T, h *harness)
}

var cases = []caseSpec{
	{"two-writer-cas", "", testTwoWriterCAS},
	{"conflict-body-shape", "", testConflictBodyShape},
	{"idempotency", "", testIdempotency},
	{"forget-restore", "", testForgetRestore},
	{"scope-isolation", "", testScopeIsolation},
	{"pc-error-mapping", "pc-fake-http", testPCErrors},
	{"no-credential-leak", "", testNoCredentialLeak},
}

func TestContract(t *testing.T) {
	for _, backend := range []string{"sqlite", "pc-fake-inmemory", "pc-fake-http"} {
		for _, tc := range cases {
			if tc.backend != "" && tc.backend != backend {
				continue
			}
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t, backend)
				tc.run(t, h)
			})
		}
	}
}

// Case 1: two writers, same base revision — one succeeds, the other
// gets 409 whose current is the winner's state. Sequential and
// concurrent.
func testTwoWriterCAS(t *testing.T, h *harness) {
	// sequential
	e := h.createEntry(t, "human:alice", "fact", "shared belief", "")
	respA := h.doJSON(t, http.MethodPut, memPath("human:alice", e.ID), ifMatch(1), map[string]any{"text": "writer-a"})
	requireStatus(t, respA, http.StatusOK, "writer A")
	ea := entry(t, respA)
	if ea.Revision != 2 || ea.Text != "writer-a" {
		t.Fatalf("writer A = %+v, want revision 2 text writer-a", ea)
	}
	respB := h.doJSON(t, http.MethodPut, memPath("human:alice", e.ID), ifMatch(1), map[string]any{"text": "writer-b"})
	requireStatus(t, respB, http.StatusConflict, "writer B (stale)")
	cb := conflictBodyOf(t, respB)
	if cb.Current.Revision != 2 || cb.Current.ETag != `"2"` || cb.Current.Text != "writer-a" {
		t.Fatalf("writer B conflict current = %+v, want the winner's state", cb.Current)
	}

	// concurrent: 5 writers on a fresh entry
	base := h.createEntry(t, "human:bob", "fact", "base", "")
	const n = 5
	texts := []string{"c0", "c1", "c2", "c3", "c4"}
	results := make([]response, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = serveRequest(t, h, http.MethodPut, memPath("human:bob", base.ID),
				`{"text":"`+texts[i]+`"}`, ifMatch(1))
		}(i)
	}
	close(start)
	wg.Wait()

	var winnerText string
	successes := 0
	for _, r := range results {
		if r.status == http.StatusOK {
			successes++
			winnerText = entry(t, r).Text
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent: %d successes, want exactly 1", successes)
	}
	for i, r := range results {
		if r.status != http.StatusOK && r.status != http.StatusConflict {
			t.Fatalf("concurrent writer %d: status = %d (body %s), want 200 or 409", i, r.status, r.body)
		}
		if r.status != http.StatusConflict {
			continue
		}
		c := conflictBodyOf(t, r)
		if c.Current.Revision != 2 || c.Current.ETag != `"2"` || c.Current.Text != winnerText {
			t.Fatalf("concurrent writer %d: current = %+v, want rev 2 text %q (the winner)", i, c.Current, winnerText)
		}
	}
	entries := h.export(t, "human:bob")
	if len(entries) != 1 || entries[0].Revision != 2 || entries[0].Text != winnerText {
		t.Fatalf("export after concurrent CAS = %+v, want one entry at rev 2 with the winner's text", entries)
	}
}

// Case 2: the 409 body has exactly the contract shape.
func testConflictBodyShape(t *testing.T, h *harness) {
	e := h.createEntry(t, "team:t1", "fact", "old", "")
	adv := h.doJSON(t, http.MethodPut, memPath("team:t1", e.ID), ifMatch(1), map[string]any{"text": "new"})
	requireStatus(t, adv, http.StatusOK, "advance to rev 2")

	stale := h.doJSON(t, http.MethodPut, memPath("team:t1", e.ID), ifMatch(1), map[string]any{"text": "stale"})
	requireStatus(t, stale, http.StatusConflict, "stale replace")

	m := bodyMap(t, stale)
	if len(m) != 3 {
		t.Fatalf("409 body top-level keys = %v, want exactly 3 (code, current, message)", m)
	}
	for _, k := range []string{"code", "current", "message"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("409 body missing key %q (keys: %v)", k, m)
		}
	}
	if m["code"] != "conflict" {
		t.Fatalf("409 code = %v, want \"conflict\"", m["code"])
	}
	cur, ok := m["current"].(map[string]any)
	if !ok {
		t.Fatalf("409 current is not an object: %v", m["current"])
	}
	if len(cur) != 3 {
		t.Fatalf("409 current keys = %v, want exactly 3 (revision, etag, text)", cur)
	}
	if cur["revision"] != float64(2) || cur["etag"] != `"2"` || cur["text"] != "new" {
		t.Fatalf("409 current = %v, want {revision:2, etag:\"2\", text:new}", cur)
	}
}

// Case 3: Idempotency-Key replay — same key+payload returns the
// first result and creates nothing; same key+different payload is a
// conflict; the key is bound to the anchor too.
func testIdempotency(t *testing.T, h *harness) {
	anchor := "team:t1"
	e1 := h.createEntry(t, anchor, "fact", "original", "key-1")

	replay := h.doJSON(t, http.MethodPost, "/memory/"+anchor, idemKey("key-1"), map[string]any{
		"content_type": "fact",
		"text":         "original",
	})
	requireStatus(t, replay, http.StatusCreated, "replay")
	e2 := entry(t, replay)
	if e2.ID != e1.ID || e2.Revision != e1.Revision || e2.ETag != e1.ETag {
		t.Fatalf("replay returned %+v, want the first entry %+v", e2, e1)
	}
	if got := h.export(t, anchor); len(got) != 1 {
		t.Fatalf("export after replay = %d entries, want exactly 1", len(got))
	}

	mismatch := h.doJSON(t, http.MethodPost, "/memory/"+anchor, idemKey("key-1"), map[string]any{
		"content_type": "fact",
		"text":         "changed",
	})
	requireStatus(t, mismatch, http.StatusConflict, "same key, different payload")
	c := conflictBodyOf(t, mismatch)
	if c.Current.Revision != 1 || c.Current.Text != "original" {
		t.Fatalf("mismatch current = %+v, want the first entry", c.Current)
	}
	if got := h.export(t, anchor); len(got) != 1 {
		t.Fatalf("export after mismatch = %d entries, want exactly 1", len(got))
	}

	e3 := h.createEntry(t, anchor, "fact", "another", "")
	if e3.ID == e1.ID {
		t.Fatal("create without a key reused the first entry's id")
	}
	if got := h.export(t, anchor); len(got) != 2 {
		t.Fatalf("export = %d entries, want 2", len(got))
	}

	// Same key, same content, different anchor: the payload hash
	// includes the anchor, so this is a conflict, not a cross-scope
	// replay.
	cross := h.doJSON(t, http.MethodPost, "/memory/human:carol", idemKey("key-1"), map[string]any{
		"content_type": "fact",
		"text":         "original",
	})
	requireStatus(t, cross, http.StatusConflict, "same key under a different anchor")
}

// Case 4: forget then restore — the forgotten entry disappears from
// search/export, is unreplaceable, and restores at revision+1.
func testForgetRestore(t *testing.T, h *harness) {
	e := h.createEntry(t, "agent:a1", "fact", "precious", "")

	del := h.doJSON(t, http.MethodDelete, memPath("agent:a1", e.ID), ifMatch(1), "")
	requireStatus(t, del, http.StatusNoContent, "forget")

	if got := h.search(t, "agent:a1", ""); len(got) != 0 {
		t.Fatalf("search after forget = %d entries, want 0", len(got))
	}
	if got := h.export(t, "agent:a1"); len(got) != 0 {
		t.Fatalf("export after forget = %d entries, want 0", len(got))
	}

	replace := h.doJSON(t, http.MethodPut, memPath("agent:a1", e.ID), ifMatch(2), map[string]any{"text": "nope"})
	requireStatus(t, replace, http.StatusNotFound, "replace a forgotten entry")

	staleRestore := h.doJSON(t, http.MethodPost, memPath("agent:a1", e.ID, "/restore"), ifMatch(1), "")
	requireStatus(t, staleRestore, http.StatusConflict, "restore with a stale revision")
	if c := conflictBodyOf(t, staleRestore); c.Current.Revision != 2 {
		t.Fatalf("stale restore current.Revision = %d, want 2 (the tombstone)", c.Current.Revision)
	}

	restore := h.doJSON(t, http.MethodPost, memPath("agent:a1", e.ID, "/restore"), ifMatch(2), "")
	requireStatus(t, restore, http.StatusOK, "restore at the tombstone revision")
	er := entry(t, restore)
	if er.Revision != 3 || er.Text != "precious" {
		t.Fatalf("restored entry = %+v, want revision 3 with the original text", er)
	}
	if got := h.search(t, "agent:a1", "precious"); len(got) != 1 {
		t.Fatalf("search after restore = %d entries, want 1", len(got))
	}
}

// Case 5: scope isolation — entries are only ever visible under
// their own anchor.
func testScopeIsolation(t *testing.T, h *harness) {
	e := h.createEntry(t, "human:alice", "fact", "alice only", "")

	if got := h.search(t, "team:t1", ""); len(got) != 0 {
		t.Fatalf("team:t1 search = %d entries, want 0", len(got))
	}
	if got := h.export(t, "team:t1"); len(got) != 0 {
		t.Fatalf("team:t1 export = %d entries, want 0", len(got))
	}
	if got := h.search(t, "agent:a1", ""); len(got) != 0 {
		t.Fatalf("agent:a1 search = %d entries, want 0", len(got))
	}
	if got := h.export(t, "agent:a1"); len(got) != 0 {
		t.Fatalf("agent:a1 export = %d entries, want 0", len(got))
	}
	if got := h.export(t, "human:alice"); len(got) != 1 || got[0].ID != e.ID {
		t.Fatalf("human:alice export = %+v, want exactly the created entry", got)
	}
}

// Case 6 (pc-fake-http only): the Client maps upstream PC errors onto
// domain errors, and sends the contract headers.
func testPCErrors(t *testing.T, h *harness) {
	if h.fakePC == nil {
		t.Skip("http fake only")
	}
	ctx := context.Background()
	store := h.store

	upstream := memory.Entry{
		ID:          "upstream-7",
		Anchor:      memory.Anchor{Kind: memory.KindTeam, ID: "t1"},
		ContentType: "fact",
		Text:        "upstream-state",
		Revision:    7,
		ETag:        `"7"`,
	}
	conflictBody := map[string]any{
		"code":    "revision_conflict",
		"message": "upstream says no",
		"current": upstream,
	}

	// 412 with a usable current → ErrConflict carrying it.
	h.fakePC.SetOverride("PUT", "/entries/whatever-1", http.StatusPreconditionFailed, conflictBody)
	_, err := store.Replace(ctx, "whatever-1", 1, memory.Patch{})
	if !memory.IsConflict(err) {
		t.Fatalf("412: err = %v, want ErrConflict", err)
	}
	if c := memory.ConflictOf(err); c.Current.Revision != 7 || c.Current.Text != "upstream-state" {
		t.Fatalf("412 current = %+v, want the upstream's entry", c.Current)
	}

	// 428 maps the same way.
	h.fakePC.SetOverride("PUT", "/entries/whatever-2", http.StatusPreconditionRequired, conflictBody)
	_, err = store.Replace(ctx, "whatever-2", 1, memory.Patch{})
	if !memory.IsConflict(err) {
		t.Fatalf("428: err = %v, want ErrConflict", err)
	}

	// 404 → ErrNotFound.
	h.fakePC.SetOverride("PUT", "/entries/whatever-3", http.StatusNotFound, map[string]any{"code": "not_found"})
	_, err = store.Replace(ctx, "whatever-3", 1, memory.Patch{})
	if !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("404: err = %v, want ErrNotFound", err)
	}

	// 401 / 403 → ErrForbidden.
	for i, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		h.fakePC.SetOverride("PUT", "/entries/whatever-auth-"+string(rune('0'+i)), status, map[string]any{"code": "denied"})
		_, err = store.Replace(ctx, "whatever-auth-"+string(rune('0'+i)), 1, memory.Patch{})
		if !errors.Is(err, memory.ErrForbidden) {
			t.Fatalf("%d: err = %v, want ErrForbidden", status, err)
		}
	}

	// A precondition failure without a usable current is a generic
	// error — never a partial trust of the body.
	h.fakePC.SetOverride("PUT", "/entries/whatever-9", http.StatusPreconditionFailed, map[string]any{"code": "revision_conflict"})
	_, err = store.Replace(ctx, "whatever-9", 1, memory.Patch{})
	if err == nil || memory.IsConflict(err) || errors.Is(err, memory.ErrNotFound) || errors.Is(err, memory.ErrForbidden) {
		t.Fatalf("412 without current: err = %v, want a generic (non-domain) error", err)
	}

	// The client sends the contract headers: Idempotency-Key on
	// create, If-Match on replace.
	e, err := store.Create(ctx, memory.NewEntry{
		Anchor:         memory.Anchor{Kind: memory.KindHuman, ID: "rec"},
		ContentType:    "fact",
		Text:           "record me",
		IdempotencyKey: "rec-key-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Replace(ctx, e.ID, 1, memory.Patch{Text: strPtr("v2")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	var sawIdem, sawIfMatch bool
	for _, rec := range h.fakePC.Records() {
		if rec.Method == http.MethodPost && rec.Path == "/entries" && rec.Header.Get("Idempotency-Key") == "rec-key-1" {
			sawIdem = true
		}
		if rec.Method == http.MethodPut && rec.Path == "/entries/"+e.ID && rec.Header.Get("If-Match") == `"1"` {
			sawIfMatch = true
		}
	}
	if !sawIdem {
		t.Fatal("no recorded POST /entries carrying Idempotency-Key: rec-key-1")
	}
	if !sawIfMatch {
		t.Fatal("no recorded PUT /entries/<id> carrying If-Match: \"1\"")
	}
}

// Case 7: no credential leak — request material (tokens, secrets,
// passwords, api keys) never appears in error bodies or strings.
func testNoCredentialLeak(t *testing.T, h *harness) {
	fragments := []string{"token=tok-abc123", "secret=sec-xyz789", "password=hunter2", "api_key=ak-000111"}
	dirty := strings.Join(fragments, " ")

	// clean baseline, advanced to revision 2
	e := h.createEntry(t, "human:alice", "fact", "clean-baseline", "")
	adv := h.doJSON(t, http.MethodPut, memPath("human:alice", e.ID), ifMatch(1), map[string]any{"text": "clean-baseline-v2"})
	requireStatus(t, adv, http.StatusOK, "advance")

	// (a) stale PUT carrying secrets → 409; the body carries only
	// the winner's clean current.
	resp := h.doJSON(t, http.MethodPut, memPath("human:alice", e.ID), ifMatch(1), map[string]any{"text": dirty})
	requireStatus(t, resp, http.StatusConflict, "stale PUT with secrets")
	assertNoFragment(t, string(resp.body), fragments)
	if c := conflictBodyOf(t, resp); c.Current.Text != "clean-baseline-v2" {
		t.Fatalf("conflict current.text = %q, want the clean baseline", c.Current.Text)
	}

	// (b) malformed JSON carrying secrets → 400, static message.
	resp = h.doRaw(t, http.MethodPut, memPath("human:alice", e.ID), ifMatch(2), `{"text":" `+dirty+`"`)
	requireStatus(t, resp, http.StatusBadRequest, "malformed PUT with secrets")
	assertNoFragment(t, string(resp.body), fragments)

	// (c) valid body with secrets but no If-Match → 428, static
	// message (the body is never read).
	resp = h.doRaw(t, http.MethodPut, memPath("human:alice", e.ID), nil, `{"text":"`+dirty+`"}`)
	requireStatus(t, resp, http.StatusPreconditionRequired, "PUT without If-Match")
	assertNoFragment(t, string(resp.body), fragments)

	// (d) the store-level error string is static too.
	_, err := h.store.Replace(context.Background(), e.ID, 1, memory.Patch{Text: strPtr(dirty)})
	if err == nil {
		t.Fatal("expected a conflict from the stale store-level replace")
	}
	assertNoFragment(t, err.Error(), fragments)
}

func assertNoFragment(t *testing.T, s string, fragments []string) {
	t.Helper()
	for _, f := range fragments {
		if strings.Contains(s, f) {
			t.Fatalf("output leaks credential fragment %q in: %s", f, s)
		}
	}
}
