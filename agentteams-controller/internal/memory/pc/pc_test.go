package pc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
)

func strPtr(s string) *string { return &s }

func TestInMemorySemantics(t *testing.T) {
	ctx := context.Background()
	m := NewInMemory()
	anchor := memory.Anchor{Kind: memory.KindTeam, ID: "t1"}

	e, err := m.Create(ctx, memory.NewEntry{Anchor: anchor, ContentType: "fact", Text: "hello"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if e.Revision != 1 || e.ID == "" {
		t.Fatalf("created = %+v, want revision 1 with an id", e)
	}

	// scope isolation
	if got, _ := m.Search(ctx, memory.SearchQuery{Anchor: memory.Anchor{Kind: memory.KindTeam, ID: "other"}}); len(got) != 0 {
		t.Fatalf("other scope search = %d entries, want 0", len(got))
	}

	// CAS
	if _, err := m.Replace(ctx, anchor, e.ID, 1, memory.Patch{Text: strPtr("v2")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, err := m.Replace(ctx, anchor, e.ID, 1, memory.Patch{Text: strPtr("stale")}); !memory.IsConflict(err) {
		t.Fatalf("stale replace: err = %v, want ErrConflict", err)
	}

	// forget → tombstone at rev 3
	if err := m.Forget(ctx, anchor, e.ID, 2); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got, _ := m.Search(ctx, memory.SearchQuery{Anchor: anchor}); len(got) != 0 {
		t.Fatalf("search after forget = %d entries, want 0", len(got))
	}
	if _, err := m.Restore(ctx, anchor, e.ID, 2); !memory.IsConflict(err) {
		t.Fatalf("stale restore: err = %v, want ErrConflict", err)
	}
	restored, err := m.Restore(ctx, anchor, e.ID, 3)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Revision != 4 || restored.Text != "v2" {
		t.Fatalf("restored = %+v, want revision 4 with the pre-forget text", restored)
	}

	// idempotency
	first, err := m.Create(ctx, memory.NewEntry{
		Anchor: memory.Anchor{Kind: memory.KindHuman, ID: "h"}, ContentType: "fact", Text: "once",
		IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("create k: %v", err)
	}
	replay, err := m.Create(ctx, memory.NewEntry{
		Anchor: memory.Anchor{Kind: memory.KindHuman, ID: "h"}, ContentType: "fact", Text: "once",
		IdempotencyKey: "k",
	})
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay = %+v (err %v), want the first entry", replay, err)
	}
	if _, err := m.Create(ctx, memory.NewEntry{
		Anchor: memory.Anchor{Kind: memory.KindHuman, ID: "h"}, ContentType: "fact", Text: "twice",
		IdempotencyKey: "k",
	}); !memory.IsConflict(err) {
		t.Fatalf("key mismatch: err = %v, want ErrConflict", err)
	}

	// invalid anchor
	if _, err := m.Create(ctx, memory.NewEntry{Anchor: memory.Anchor{Kind: "nope"}, Text: "x"}); err != memory.ErrInvalidAnchor {
		t.Fatalf("invalid anchor: err = %v, want ErrInvalidAnchor", err)
	}
}

func TestClientFakePCRoundTrip(t *testing.T) {
	fake := NewFakePC(t)
	ctx := context.Background()
	c := fake.Client()
	anchor := memory.Anchor{Kind: memory.KindHuman, ID: "alice"}

	e, err := c.Create(ctx, memory.NewEntry{
		Anchor:         anchor,
		ContentType:    "fact",
		Text:           "via http",
		IdempotencyKey: "rt-key",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if e.Revision != 1 {
		t.Fatalf("created = %+v, want revision 1", e)
	}

	// idempotent replay over HTTP
	replay, err := c.Create(ctx, memory.NewEntry{
		Anchor:         anchor,
		ContentType:    "fact",
		Text:           "via http",
		IdempotencyKey: "rt-key",
	})
	if err != nil || replay.ID != e.ID {
		t.Fatalf("replay = %+v (err %v), want the first entry", replay, err)
	}

	updated, err := c.Replace(ctx, anchor, e.ID, 1, memory.Patch{Text: strPtr("v2")})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if updated.Revision != 2 {
		t.Fatalf("updated = %+v, want revision 2", updated)
	}

	if err := c.Forget(ctx, anchor, e.ID, 2); err != nil {
		t.Fatalf("forget: %v", err)
	}
	restored, err := c.Restore(ctx, anchor, e.ID, 3)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Revision != 4 {
		t.Fatalf("restored = %+v, want revision 4", restored)
	}

	got, err := c.Search(ctx, memory.SearchQuery{Anchor: anchor, Query: "V2"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != e.ID {
		t.Fatalf("search = %+v, want the one entry (case-insensitive)", got)
	}
	all, err := c.Export(ctx, anchor)
	if err != nil || len(all) != 1 {
		t.Fatalf("export = %d entries (err %v), want 1", len(all), err)
	}
}

func TestClientRawHTTPDetails(t *testing.T) {
	fake := NewFakePC(t)
	c := fake.Client()

	// ETag header on create
	resp, err := c.http.Do(rawRequest(t, http.MethodPost, fake.Server.URL+"/entries",
		`{"anchor":"human:bob","content_type":"fact","text":"etag-check"}`))
	if err != nil {
		t.Fatalf("raw create: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("raw create status = %d (body %s), want 201", resp.StatusCode, body)
	}
	if got := resp.Header.Get("ETag"); got != `"1"` {
		t.Fatalf("create ETag header = %q, want \"1\"", got)
	}
	var created memory.Entry
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// PUT without If-Match → 428 with the current entry
	resp, err = c.http.Do(rawRequest(t, http.MethodPut,
		fake.Server.URL+"/entries/"+created.ID+"?anchor=human:bob", `{"text":"x"}`))
	if err != nil {
		t.Fatalf("raw put: %v", err)
	}
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("put without If-Match status = %d (body %s), want 428", resp.StatusCode, body)
	}
	var pre errorResponse
	if err := json.Unmarshal(body, &pre); err != nil {
		t.Fatalf("decode 428: %v", err)
	}
	if pre.Current == nil || pre.Current.Revision != 1 {
		t.Fatalf("428 current = %+v, want the live entry at rev 1", pre.Current)
	}

	// PUT without If-Match on an unknown id → 404
	resp, err = c.http.Do(rawRequest(t, http.MethodPut,
		fake.Server.URL+"/entries/unknown?anchor=human:bob", `{"text":"x"}`))
	if err != nil {
		t.Fatalf("raw put unknown: %v", err)
	}
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("put unknown without If-Match = %d (body %s), want 404", resp.StatusCode, body)
	}

	// 500 upstream → generic error (not a domain error)
	fake.SetOverride("PUT", "/entries/"+created.ID, http.StatusInternalServerError, map[string]any{"code": "boom"})
	_, err = c.Replace(context.Background(), memory.Anchor{Kind: memory.KindHuman, ID: "bob"}, created.ID, 1, memory.Patch{Text: strPtr("x")})
	if err == nil || memory.IsConflict(err) || errors.Is(err, memory.ErrNotFound) || errors.Is(err, memory.ErrForbidden) {
		t.Fatalf("500: err = %v, want a generic (non-domain) error", err)
	}
}

func TestClientErrorMapping(t *testing.T) {
	fake := NewFakePC(t)
	c := fake.Client()
	ctx := context.Background()

	current := memory.Entry{ID: "x", Anchor: memory.Anchor{Kind: memory.KindHuman, ID: "alice"}, Revision: 5, ETag: `"5"`, Text: "t"}
	for _, status := range []int{http.StatusPreconditionFailed, http.StatusPreconditionRequired} {
		fake.SetOverride("PUT", "/entries/m1", status, map[string]any{"code": "c", "current": current})
		_, err := c.Replace(ctx, memory.Anchor{Kind: memory.KindHuman, ID: "alice"}, "m1", 1, memory.Patch{})
		if !memory.IsConflict(err) {
			t.Fatalf("%d: err = %v, want ErrConflict", status, err)
		}
		if cc := memory.ConflictOf(err); cc.Current.Revision != 5 {
			t.Fatalf("%d: current = %+v, want revision 5", status, cc.Current)
		}
	}

	for status, want := range map[int]error{
		http.StatusNotFound:     memory.ErrNotFound,
		http.StatusUnauthorized: memory.ErrForbidden,
		http.StatusForbidden:    memory.ErrForbidden,
	} {
		fake.SetOverride("PUT", "/entries/m2", status, map[string]any{"code": "c"})
		_, err := c.Replace(ctx, memory.Anchor{Kind: memory.KindHuman, ID: "alice"}, "m2", 1, memory.Patch{})
		if !errors.Is(err, want) {
			t.Fatalf("%d: err = %v, want %v", status, err, want)
		}
	}
}

func TestFakePCOverrideIsOneShot(t *testing.T) {
	fake := NewFakePC(t)
	ctx := context.Background()
	c := fake.Client()
	anchor := memory.Anchor{Kind: memory.KindTeam, ID: "t9"}

	e, err := c.Create(ctx, memory.NewEntry{Anchor: anchor, ContentType: "fact", Text: "real"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// first PUT: forced 412 with an upstream current
	upstream := memory.Entry{ID: "forced", Anchor: memory.Anchor{Kind: memory.KindTeam, ID: "t9"}, Revision: 9, ETag: `"9"`, Text: "forced-state"}
	fake.SetOverride("PUT", "/entries/"+e.ID, http.StatusPreconditionFailed, map[string]any{"code": "c", "current": upstream})
	_, err = c.Replace(ctx, anchor, e.ID, 1, memory.Patch{Text: strPtr("x")})
	if !memory.IsConflict(err) || memory.ConflictOf(err).Current.Text != "forced-state" {
		t.Fatalf("override hit: err = %v, want ErrConflict with the forced current", err)
	}

	// second PUT: the override is gone, the real state answers
	updated, err := c.Replace(ctx, anchor, e.ID, 1, memory.Patch{Text: strPtr("real write")})
	if err != nil {
		t.Fatalf("second put: %v, want the real replace to succeed", err)
	}
	if updated.Revision != 2 || updated.Text != "real write" {
		t.Fatalf("second put = %+v, want revision 2 with the real text", updated)
	}
}

// TestInMemoryCrossAnchorBinding pins the same store-level contract
// as the SQLite backend: an id that exists only under a different
// anchor behaves exactly like an unknown id — ErrNotFound, never
// ErrConflict (there is no current to carry), state left untouched.
func TestInMemoryCrossAnchorBinding(t *testing.T) {
	ctx := context.Background()
	m := NewInMemory()
	anchor := memory.Anchor{Kind: memory.KindHuman, ID: "alice"}
	team := memory.Anchor{Kind: memory.KindTeam, ID: "t1"}

	e, err := m.Create(ctx, memory.NewEntry{Anchor: anchor, ContentType: "fact", Text: "alice-only"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// (1) cross-anchor replace → ErrNotFound, entry untouched.
	if _, err := m.Replace(ctx, team, e.ID, 1, memory.Patch{Text: strPtr("pwned")}); err != memory.ErrNotFound {
		t.Fatalf("cross-anchor replace: err = %v, want ErrNotFound", err)
	}
	if got, _ := m.Export(ctx, anchor); len(got) != 1 || got[0].Revision != 1 || got[0].Text != "alice-only" {
		t.Fatalf("export after cross-anchor replace = %+v, want the untouched entry", got)
	}

	// (2) cross-anchor forget → ErrNotFound, entry still live.
	if err := m.Forget(ctx, team, e.ID, 1); err != memory.ErrNotFound {
		t.Fatalf("cross-anchor forget: err = %v, want ErrNotFound", err)
	}
	if got, _ := m.Export(ctx, anchor); len(got) != 1 || got[0].Revision != 1 {
		t.Fatalf("export after cross-anchor forget = %+v, want the live entry at rev 1", got)
	}

	// (3) cross-anchor restore → ErrNotFound, entry not restored.
	if err := m.Forget(ctx, anchor, e.ID, 1); err != nil {
		t.Fatalf("own forget: %v", err)
	}
	if _, err := m.Restore(ctx, team, e.ID, 2); err != memory.ErrNotFound {
		t.Fatalf("cross-anchor restore: err = %v, want ErrNotFound", err)
	}
	if got, _ := m.Export(ctx, anchor); len(got) != 0 {
		t.Fatalf("export after cross-anchor restore = %+v, want 0 entries", got)
	}
	// and the own anchor can still restore it
	restored, err := m.Restore(ctx, anchor, e.ID, 2)
	if err != nil {
		t.Fatalf("own restore: %v", err)
	}
	if restored.Revision != 3 || restored.Text != "alice-only" {
		t.Fatalf("restored = %+v, want revision 3 with the original text", restored)
	}

	// (4) stale writes: own anchor → ErrConflict with the same-anchor
	// current; foreign anchor → ErrNotFound, never a conflict (the
	// leak path from #1482).
	_, err = m.Replace(ctx, anchor, e.ID, 2, memory.Patch{Text: strPtr("v3")})
	if !memory.IsConflict(err) {
		t.Fatalf("stale replace: err = %v, want ErrConflict", err)
	}
	if c := memory.ConflictOf(err); c.Current.Revision != 3 || c.Current.Text != "alice-only" || c.Current.Anchor != anchor {
		t.Fatalf("conflict current = %+v, want the same-anchor state", c.Current)
	}
	if _, err := m.Replace(ctx, team, e.ID, 2, memory.Patch{Text: strPtr("v3")}); err != memory.ErrNotFound {
		t.Fatalf("cross-anchor stale replace: err = %v, want ErrNotFound", err)
	}
}

// TestClientCrossAnchorOverHTTP pins the anchor binding through the
// real wire protocol: the client sends the anchor as a query
// parameter, and a cross-anchor mutation comes back as ErrNotFound
// with the entry untouched.
func TestClientCrossAnchorOverHTTP(t *testing.T) {
	fake := NewFakePC(t)
	ctx := context.Background()
	c := fake.Client()
	anchor := memory.Anchor{Kind: memory.KindHuman, ID: "alice"}
	team := memory.Anchor{Kind: memory.KindTeam, ID: "t1"}

	e, err := c.Create(ctx, memory.NewEntry{Anchor: anchor, ContentType: "fact", Text: "alice-secret"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// cross-anchor mutations over the wire → ErrNotFound
	if _, err := c.Replace(ctx, team, e.ID, 1, memory.Patch{Text: strPtr("pwned")}); err != memory.ErrNotFound {
		t.Fatalf("cross-anchor replace: err = %v, want ErrNotFound", err)
	}
	if err := c.Forget(ctx, team, e.ID, 1); err != memory.ErrNotFound {
		t.Fatalf("cross-anchor forget: err = %v, want ErrNotFound", err)
	}
	if _, err := c.Restore(ctx, team, e.ID, 1); err != memory.ErrNotFound {
		t.Fatalf("cross-anchor restore: err = %v, want ErrNotFound", err)
	}

	// the entry is untouched under its own anchor
	all, err := c.Export(ctx, anchor)
	if err != nil || len(all) != 1 || all[0].Revision != 1 || all[0].Text != "alice-secret" {
		t.Fatalf("export = %+v (err %v), want the untouched entry at rev 1", all, err)
	}

	// the wire carries the anchor: the replace request must have
	// gone out with ?anchor=team:t1
	found := false
	for _, rec := range fake.Records() {
		if rec.Method == http.MethodPut && rec.Path == "/entries/"+e.ID {
			q, err := url.ParseQuery(rec.Query)
			if err != nil {
				t.Fatalf("parse query %q: %v", rec.Query, err)
			}
			if got := q.Get("anchor"); got != team.String() {
				t.Fatalf("replace anchor = %q, want %q", got, team.String())
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no recorded PUT for %s", e.ID)
	}
}

// --- raw helpers ------------------------------------------------------

func rawRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatalf("build raw request: %v", err)
	}
	return req
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return b
}
