package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// getCountingOSS counts the two read calls a trash listing makes so a test
// can pin its cost. Every other method is the in-memory fake.
type getCountingOSS struct {
	*ossfake.Memory
	gets  atomic.Int64
	lists atomic.Int64
}

func (c *getCountingOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	c.gets.Add(1)
	return c.Memory.GetObject(ctx, key)
}

func (c *getCountingOSS) ListObjectsDetailed(ctx context.Context, prefix string) ([]oss.ObjectInfo, error) {
	c.lists.Add(1)
	return c.Memory.ListObjectsDetailed(ctx, prefix)
}

// hfTrashEntries decodes a trash listing response.
func hfTrashEntries(t *testing.T, rec *httptest.ResponseRecorder) []trashEntry {
	t.Helper()
	var body struct {
		Entries []trashEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode trash listing: %v: %s", err, rec.Body.String())
	}
	return body.Entries
}

// hfTrashPaths returns the sorted original paths of a trash listing.
func hfTrashPaths(t *testing.T, entries []trashEntry) []string {
	t.Helper()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	sort.Strings(out)
	return out
}

// TestHumanFiles_TrashListIsNotNPlusOne pins the trash listing's cost: one
// .trash/ listing plus one index read answers any number of entries, with no
// per-entry meta.json read. An entry no index covers falls back to its own
// meta.json read (the pre-index degradation) and the index is refilled, so
// the next listing is back to one read.
func TestHumanFiles_TrashListIsNotNPlusOne(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	ctx := context.Background()

	// Three soft deletes; each one caches its own summary.
	for _, path := range []string{"a.txt", "b.txt", "c/d.txt"} {
		if rec := hfPutFile(t, hfh, "alice", path, hfSelf(), "hello"); rec.Code != http.StatusOK {
			t.Fatalf("put %s: %d %s", path, rec.Code, rec.Body.String())
		}
		rec := httptest.NewRecorder()
		hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", path, hfSelf(), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("delete %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	want := []string{"files/a.txt", "files/b.txt", "files/c/d.txt"}

	// Steady state: one listing and one index read for all three entries.
	counting := &getCountingOSS{Memory: store}
	hfh.oss = counting
	rec := httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list trash: %d %s", rec.Code, rec.Body.String())
	}
	entries := hfTrashEntries(t, rec)
	if got := hfTrashPaths(t, entries); !equalStrings(got, want) {
		t.Fatalf("trash paths = %v, want %v", got, want)
	}
	for _, e := range entries {
		if e.Size != 5 || e.DeletedAt == "" || e.Caller != "alice" || e.Role != "human" {
			t.Fatalf("trash entry %+v is missing its summary", e)
		}
	}
	if gets, lists := counting.gets.Load(), counting.lists.Load(); gets != 1 || lists != 1 {
		t.Fatalf("list trash performed %d object reads and %d listings, want 1 and 1", gets, lists)
	}

	// A delete refreshes the index on the write path, so even the first
	// listing after it stays at one read per call.
	if rec := hfPutFile(t, hfh, "alice", "e.txt", hfSelf(), "hello"); rec.Code != http.StatusOK {
		t.Fatalf("put e.txt: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "e.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete e.txt: %d %s", rec.Code, rec.Body.String())
	}
	counting.gets.Store(0)
	counting.lists.Store(0)
	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list trash after a delete: %d %s", rec.Code, rec.Body.String())
	}
	if got := len(hfTrashEntries(t, rec)); got != 4 {
		t.Fatalf("entries after the fourth delete = %d, want 4", got)
	}
	if gets := counting.gets.Load(); gets != 1 {
		t.Fatalf("list trash after a delete performed %d object reads, want 1", gets)
	}

	// A stale index entry (restored out of the listing) is not resurrected.
	entries = hfTrashEntries(t, rec)
	rec = httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", entries[0].ID, hfSelf(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore %s: %d %s", entries[0].ID, rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list trash after restore: %d %s", rec.Code, rec.Body.String())
	}
	if got := len(hfTrashEntries(t, rec)); got != 3 {
		t.Fatalf("entries after restore = %d, want 3", got)
	}
	if idx, err := hfh.readTrashIndex(ctx, "alice"); err != nil || len(idx) != 3 {
		t.Fatalf("index after restore listing = %v, err %v; want 3 entries", idx, err)
	}

	// Cold index (a deployment upgraded from before it existed): the index
	// read finds nothing and each entry is resolved by its own meta.json
	// read. The listing still answers every entry, and the refill puts the
	// next listing back at one read.
	if err := store.DeleteObject(ctx, humanTrashIndexKey("alice")); err != nil {
		t.Fatalf("drop trash index: %v", err)
	}
	counting.gets.Store(0)
	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list trash without an index: %d %s", rec.Code, rec.Body.String())
	}
	if got := len(hfTrashEntries(t, rec)); got != 3 {
		t.Fatalf("entries without an index = %d, want 3", got)
	}
	if gets := counting.gets.Load(); gets != 4 {
		t.Fatalf("cold-index listing performed %d object reads, want 1 index read plus one per entry (4)", gets)
	}
	if _, err := store.GetObject(ctx, humanTrashIndexKey("alice")); err != nil {
		t.Fatalf("listing did not refill the index: %v", err)
	}
	counting.gets.Store(0)
	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if gets := counting.gets.Load(); gets != 1 {
		t.Fatalf("listing after the refill performed %d object reads, want 1", gets)
	}
	for _, e := range hfTrashEntries(t, rec) {
		purged := httptest.NewRecorder()
		hfh.purgeTrash(purged, hfTrashReq(t, http.MethodDelete, "alice", e.ID, hfAdmin(), ""))
		if purged.Code != http.StatusOK {
			t.Fatalf("purge: %d %s", purged.Code, purged.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK || len(hfTrashEntries(t, rec)) != 0 {
		t.Fatalf("empty trash: %d %s", rec.Code, rec.Body.String())
	}
	if idx, err := hfh.readTrashIndex(ctx, "alice"); err != nil || len(idx) != 0 {
		t.Fatalf("empty trash index = %v, err %v", idx, err)
	}
}

func TestHumanFiles_TrashIndexRecovery(t *testing.T) {
	for _, cache := range []string{`{`, `{"version":99,"entries":{}}`, `{"version":1,"entries":{}}`} {
		t.Run(cache, func(t *testing.T) {
			hfh, store, _ := newHumanFilesRig(t)
			ctx := context.Background()
			id, err := newTrashID()
			if err != nil {
				t.Fatal(err)
			}
			meta := trashMeta{Path: "files/a.txt", Size: 5, Caller: "alice", Role: "human"}
			data, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutObject(ctx, humanTrashPrefix("alice")+id+"/meta.json", data); err != nil {
				t.Fatal(err)
			}
			if err := store.PutObject(ctx, humanTrashIndexKey("alice"), []byte(cache)); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
			}
			entries := hfTrashEntries(t, rec)
			if len(entries) != 1 || entries[0].Path != meta.Path {
				t.Fatalf("recovered entries = %+v", entries)
			}
			idx, err := hfh.readTrashIndex(ctx, "alice")
			if err != nil || idx[id].Path != meta.Path {
				t.Fatalf("recovered index = %v, err %v", idx, err)
			}
		})
	}
}

type concurrentTrashIndexOSS struct {
	*ossfake.Memory
	reads   atomic.Int64
	arrived chan struct{}
	release chan struct{}
}

func (s *concurrentTrashIndexOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	data, err := s.Memory.GetObject(ctx, key)
	if key == humanTrashIndexKey("alice") && s.reads.Add(1) <= 2 {
		s.arrived <- struct{}{}
		<-s.release
	}
	return data, err
}

func TestHumanFiles_TrashIndexConcurrentWritesRecover(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	ctx := context.Background()
	metas := make(map[string]trashMeta)
	for _, path := range []string{"files/a.txt", "files/b.txt"} {
		id, err := newTrashID()
		if err != nil {
			t.Fatal(err)
		}
		m := trashMeta{Path: path, Size: 5, Caller: "alice", Role: "human"}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutObject(ctx, humanTrashPrefix("alice")+id+"/meta.json", data); err != nil {
			t.Fatal(err)
		}
		metas[id] = m
	}
	s := &concurrentTrashIndexOSS{Memory: store, arrived: make(chan struct{}, 2), release: make(chan struct{})}
	hfh.oss = s
	var wg sync.WaitGroup
	for id, m := range metas {
		wg.Go(func() { hfh.upsertTrashIndexEntry(ctx, "alice", id, m) })
	}
	// Both writers read the absent index before either can publish it.
	<-s.arrived
	<-s.arrived
	close(s.release)
	wg.Wait()
	idx, err := hfh.readTrashIndex(ctx, "alice")
	if err != nil || len(idx) != 1 {
		t.Fatalf("expected one lost cached summary, got %v, err %v", idx, err)
	}
	rec := httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK || len(hfTrashEntries(t, rec)) != 2 {
		t.Fatalf("concurrent-write recovery: %d %s", rec.Code, rec.Body.String())
	}
	idx, err = hfh.readTrashIndex(ctx, "alice")
	if err != nil || len(idx) != 2 {
		t.Fatalf("recovered index = %v, err %v", idx, err)
	}
}
