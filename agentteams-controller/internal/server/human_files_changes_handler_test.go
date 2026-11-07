package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// hfChangesReq builds a request against the change feed route.
func hfChangesReq(t *testing.T, name, query string, caller *authpkg.CallerIdentity) *http.Request {
	t.Helper()
	req := hfReq(t, http.MethodGet, name, "", caller, nil)
	req.URL.Path = "/api/v1/humans/" + name + "/files/changes"
	if query != "" {
		req.URL.RawQuery = query
	}
	return req
}

// hfGetChanges runs the change feed handler; the decoded body is only
// valid when the response is 200 (the cursor rejections have a different
// shape and are decoded by the caller).
func hfGetChanges(t *testing.T, hfh *humanFilesHandler, name, query string, caller *authpkg.CallerIdentity) (*httptest.ResponseRecorder, humanFileChangesResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	hfh.fileChanges(rec, hfChangesReq(t, name, query, caller))
	var body humanFileChangesResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("changes body decode: %v: %s", err, rec.Body.String())
		}
	}
	return rec, body
}

// hfChangeIDs lists the surviving change ids of one human, ascending.
func hfChangeIDs(t *testing.T, store *ossfake.Memory, username string) []string {
	t.Helper()
	names, err := store.ListObjects(context.Background(), humanChangesPrefix(username))
	if err != nil {
		t.Fatalf("list change log: %v", err)
	}
	ids := make([]string, 0, len(names))
	for _, n := range names {
		if id, ok := changeIDFromObjectName(n); ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// TestHumanFileChanges_NoChangeEchoesCursor covers the steady-state poll:
// a cursor with nothing after it gets 200 with empty items/deleted and
// the cursor returned verbatim, and the JSON carries arrays, not nulls.
func TestHumanFileChanges_NoChangeEchoesCursor(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	rec, body := hfGetChanges(t, hfh, "alice", "since=v1:", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("empty feed: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(body.Items) != 0 || len(body.Deleted) != 0 {
		t.Fatalf("empty feed must have no entries: %+v", body)
	}
	if body.Cursor != "v1:" {
		t.Fatalf("cursor must round-trip verbatim, got %q", body.Cursor)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw body decode: %v", err)
	}
	if _, ok := raw["items"].([]any); !ok {
		t.Fatalf("items must be a JSON array: %s", rec.Body.String())
	}
	if _, ok := raw["deleted"].([]any); !ok {
		t.Fatalf("deleted must be a JSON array: %s", rec.Body.String())
	}

	// A write lands, then resuming from the feed's own cursor sees
	// nothing new and echoes that cursor back.
	if rec := hfPutFile(t, hfh, "alice", "docs/a.md", hfSelf(), "hello"); rec.Code != http.StatusOK {
		t.Fatalf("put: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec, body = hfGetChanges(t, hfh, "alice", "since=v1:", hfSelf())
	if rec.Code != http.StatusOK || len(body.Items) != 1 {
		t.Fatalf("feed after put: code %d, items %d: %s", rec.Code, len(body.Items), rec.Body.String())
	}
	cursor := body.Cursor
	if cursor == "v1:" {
		t.Fatalf("cursor must have advanced, body: %s", rec.Body.String())
	}
	rec, body = hfGetChanges(t, hfh, "alice", "since="+cursor, hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("resume poll: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(body.Items) != 0 || len(body.Deleted) != 0 {
		t.Fatalf("resume poll must be empty: %+v", body)
	}
	if body.Cursor != cursor {
		t.Fatalf("resume cursor must round-trip verbatim, got %q want %q", body.Cursor, cursor)
	}
}

// TestHumanFileChanges_PutAppearsOnceAndCursorAdvances: one PUT produces
// exactly one change-log object and exactly one item, with the item's
// size and etag matching the write.
func TestHumanFileChanges_PutAppearsOnceAndCursorAdvances(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	content := "hello change"
	rec := hfPutFile(t, hfh, "alice", "docs/a.md", hfSelf(), content)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var putBody struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &putBody); err != nil {
		t.Fatalf("put body decode: %v", err)
	}

	rec, body := hfGetChanges(t, hfh, "alice", "since=v1:", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("feed: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(body.Items) != 1 {
		t.Fatalf("want exactly one item, got %d: %s", len(body.Items), rec.Body.String())
	}
	item := body.Items[0]
	if item.Path != "files/docs/a.md" || item.Op != "put" || item.Size != int64(len(content)) || item.ETag != putBody.ETag {
		t.Fatalf("item mismatch: %+v", item)
	}
	if len(body.Deleted) != 0 {
		t.Fatalf("unexpected deleted entries: %v", body.Deleted)
	}
	if body.Cursor == "v1:" {
		t.Fatalf("cursor must have advanced: %s", rec.Body.String())
	}

	ids := hfChangeIDs(t, store, "alice")
	if len(ids) != 1 {
		t.Fatalf("want exactly one change-log object, got %d: %v", len(ids), ids)
	}
	if want := "v1:" + ids[0]; body.Cursor != want {
		t.Fatalf("cursor %q does not address the recorded change %q", body.Cursor, want)
	}
}

// TestHumanFileChanges_DeleteAppearsInDeleted: a soft delete surfaces as a
// path in deleted on the next delta, and the cursor advances past it.
func TestHumanFileChanges_DeleteAppearsInDeleted(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "a.md", hfSelf(), "delete me"); rec.Code != http.StatusOK {
		t.Fatalf("put: got %d: %s", rec.Code, rec.Body.String())
	}
	_, body := hfGetChanges(t, hfh, "alice", "since=v1:", hfSelf())
	if len(body.Items) != 1 {
		t.Fatalf("want the put in the first delta, got %+v", body)
	}
	cursor := body.Cursor

	rec := httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "a.md", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d: %s", rec.Code, rec.Body.String())
	}

	rec, body = hfGetChanges(t, hfh, "alice", "since="+cursor, hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("feed: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(body.Items) != 0 {
		t.Fatalf("delete delta must have no items: %+v", body)
	}
	if len(body.Deleted) != 1 || body.Deleted[0] != "files/a.md" {
		t.Fatalf("deleted must be [files/a.md], got %v", body.Deleted)
	}
	if body.Cursor == cursor {
		t.Fatalf("cursor must advance past the delete: %s", rec.Body.String())
	}
}

// TestHumanFileChanges_ExpiredCursor410: a cursor whose entry was pruned
// gets 410 CursorExpired with latest = the newest surviving cursor; a
// non-empty cursor against a fully pruned log gets latest = the bare
// initial cursor.
func TestHumanFileChanges_ExpiredCursor410(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "a.md", hfSelf(), "one"); rec.Code != http.StatusOK {
		t.Fatalf("put 1: got %d: %s", rec.Code, rec.Body.String())
	}
	time.Sleep(2 * time.Millisecond) // distinct nanosecond stamps keep id order = time order
	if rec := hfPutFile(t, hfh, "alice", "b.md", hfSelf(), "two"); rec.Code != http.StatusOK {
		t.Fatalf("put 2: got %d: %s", rec.Code, rec.Body.String())
	}
	ids := hfChangeIDs(t, store, "alice")
	if len(ids) != 2 {
		t.Fatalf("want two change entries, got %v", ids)
	}
	prefix := humanChangesPrefix("alice")

	// Simulate the retention sweep dropping the oldest surviving entry.
	if err := store.DeleteObject(context.Background(), prefix+ids[0]+".json"); err != nil {
		t.Fatalf("prune oldest: %v", err)
	}
	rec, _ := hfGetChanges(t, hfh, "alice", "since=v1:"+ids[0], hfSelf())
	if rec.Code != http.StatusGone {
		t.Fatalf("expired cursor: got %d, want 410: %s", rec.Code, rec.Body.String())
	}
	var cur humanFileCursorError
	if err := json.Unmarshal(rec.Body.Bytes(), &cur); err != nil {
		t.Fatalf("410 body decode: %v: %s", err, rec.Body.String())
	}
	if cur.Code != "CursorExpired" || cur.Latest != "v1:"+ids[1] {
		t.Fatalf("410 body mismatch: %+v", cur)
	}

	// The whole log pruned: latest falls back to the bare initial cursor.
	if err := store.DeleteObject(context.Background(), prefix+ids[1]+".json"); err != nil {
		t.Fatalf("prune newest: %v", err)
	}
	rec, _ = hfGetChanges(t, hfh, "alice", "since=v1:"+ids[1], hfSelf())
	if rec.Code != http.StatusGone {
		t.Fatalf("fully pruned cursor: got %d, want 410: %s", rec.Code, rec.Body.String())
	}
	cur = humanFileCursorError{}
	if err := json.Unmarshal(rec.Body.Bytes(), &cur); err != nil {
		t.Fatalf("410 body decode: %v: %s", err, rec.Body.String())
	}
	if cur.Code != "CursorExpired" || cur.Latest != humanChangeCursorPrefix {
		t.Fatalf("410 body mismatch: %+v", cur)
	}
}

// TestHumanFileChanges_InvalidSince400: a missing or malformed since is
// rejected 400 InvalidCursor, and a caller without access to the area
// gets the same indistinguishable 404 as every other file-area read.
func TestHumanFileChanges_InvalidSince400(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	for _, query := range []string{"", "since=garbage", "since=v2:012345678901234567890123", "since=v1:not-a-change-id", "since=v1:123"} {
		rec, _ := hfGetChanges(t, hfh, "alice", query, hfSelf())
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: got %d, want 400: %s", query, rec.Code, rec.Body.String())
		}
		var cur humanFileCursorError
		if err := json.Unmarshal(rec.Body.Bytes(), &cur); err != nil {
			t.Fatalf("400 body decode: %v: %s", err, rec.Body.String())
		}
		if cur.Code != "InvalidCursor" {
			t.Fatalf("query %q: code %q, want InvalidCursor", query, cur.Code)
		}
	}

	rec, _ := hfGetChanges(t, hfh, "alice", "since=v1:", hfBob())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign caller: got %d, want 404", rec.Code)
	}
}

// TestHumanFileChanges_RetentionPrunesOnAppend: an entry older than the
// retention window is dropped by the sweep an append triggers, and its
// cursor then answers 410.
func TestHumanFileChanges_RetentionPrunesOnAppend(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	hfh.changesPruneInterval = 0 // prune on every append, not the 30s gate

	oldID := fmt.Sprintf("%020d%06x", time.Now().UTC().Add(-8*24*time.Hour).UnixNano(), 7)
	oldEntry := `{"op":"put","path":"files/old.md","size":1,"etag":"stale","ts":"2026-10-28T00:00:00Z"}`
	if err := store.PutObject(context.Background(), humanChangesPrefix("alice")+oldID+".json", []byte(oldEntry)); err != nil {
		t.Fatalf("seed old entry: %v", err)
	}

	if rec := hfPutFile(t, hfh, "alice", "fresh.md", hfSelf(), "fresh"); rec.Code != http.StatusOK {
		t.Fatalf("put: got %d: %s", rec.Code, rec.Body.String())
	}
	ids := hfChangeIDs(t, store, "alice")
	if len(ids) != 1 {
		t.Fatalf("the stale entry must have been pruned, survivors: %v", ids)
	}

	rec, _ := hfGetChanges(t, hfh, "alice", "since=v1:"+oldID, hfSelf())
	if rec.Code != http.StatusGone {
		t.Fatalf("stale cursor: got %d, want 410: %s", rec.Code, rec.Body.String())
	}
	var cur humanFileCursorError
	if err := json.Unmarshal(rec.Body.Bytes(), &cur); err != nil {
		t.Fatalf("410 body decode: %v: %s", err, rec.Body.String())
	}
	if cur.Code != "CursorExpired" || cur.Latest != "v1:"+ids[0] {
		t.Fatalf("410 body mismatch: %+v", cur)
	}
}

// TestHumanFileChanges_RetentionCountCap: the log is capped at
// humanChangeRetentionMax entries; the sweep drops the oldest excess.
func TestHumanFileChanges_RetentionCountCap(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	hfh.changesPruneInterval = 0

	prefix := humanChangesPrefix("alice")
	nowNano := time.Now().UTC().UnixNano()
	entry := []byte(`{"op":"put","path":"files/x","size":0,"etag":"e","ts":"2026-11-07T00:00:00Z"}`)
	for i := 0; i <= humanChangeRetentionMax; i++ { // max+1 entries
		id := fmt.Sprintf("%020d%06x", nowNano, i)
		if err := store.PutObject(context.Background(), prefix+id+".json", entry); err != nil {
			t.Fatalf("seed entry %d: %v", i, err)
		}
	}

	hfh.pruneHumanChanges(context.Background(), "alice")

	ids := hfChangeIDs(t, store, "alice")
	if len(ids) != humanChangeRetentionMax {
		t.Fatalf("want %d surviving entries, got %d", humanChangeRetentionMax, len(ids))
	}
	if oldest := fmt.Sprintf("%020d%06x", nowNano, 0); ids[0] == oldest {
		t.Fatalf("the oldest entry must have been dropped, survivors start at %q", ids[0])
	}
	if newest := fmt.Sprintf("%020d%06x", nowNano, humanChangeRetentionMax); ids[len(ids)-1] != newest {
		t.Fatalf("the newest entry must survive, survivors end at %q", ids[len(ids)-1])
	}
}

// TestHumanFileChanges_RestoreAndPurgeSurface: restore surfaces in items,
// while purging trash does not repeat a live-file deletion.
func TestHumanFileChanges_RestoreAndPurgeSurface(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "a.md", hfSelf(), "v1-content"); rec.Code != http.StatusOK {
		t.Fatalf("put: got %d: %s", rec.Code, rec.Body.String())
	}

	delete := func(t *testing.T) string {
		t.Helper()
		rec := httptest.NewRecorder()
		hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "a.md", hfSelf(), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("delete: got %d: %s", rec.Code, rec.Body.String())
		}
		var del struct {
			TrashID string `json:"trashId"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &del); err != nil {
			t.Fatalf("delete body decode: %v", err)
		}
		return del.TrashID
	}

	id1 := delete(t)
	rec := httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", id1, hfSelf(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: got %d: %s", rec.Code, rec.Body.String())
	}

	id2 := delete(t)
	rec = httptest.NewRecorder()
	hfh.purgeTrash(rec, hfTrashReq(t, http.MethodDelete, "alice", id2, hfAdmin(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("purge: got %d: %s", rec.Code, rec.Body.String())
	}

	rec, body := hfGetChanges(t, hfh, "alice", "since=v1:", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("feed: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(body.Items) != 2 || body.Items[0].Op != "put" || body.Items[1].Op != "restore" {
		t.Fatalf("items must be [put, restore] for files/a.md: %+v", body.Items)
	}
	if body.Items[0].Path != "files/a.md" || body.Items[1].Path != "files/a.md" {
		t.Fatalf("item paths must be the files/ path: %+v", body.Items)
	}
	// Only the two soft deletes affect the live files/ path.
	if len(body.Deleted) != 2 {
		t.Fatalf("deleted must contain only the two soft deletes: %v", body.Deleted)
	}
	for _, p := range body.Deleted {
		if p != "files/a.md" {
			t.Fatalf("deleted must restate files/a.md: %v", body.Deleted)
		}
	}
}
