package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func strPtr(s string) *string { return &s }

func TestStoreBasics(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	anchor := memory.Anchor{Kind: memory.KindHuman, ID: "alice"}

	e, err := s.Create(ctx, memory.NewEntry{
		Anchor:      anchor,
		ContentType: "fact",
		Text:        "likes go",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if e.Revision != 1 || e.ETag != `"1"` || e.Anchor != anchor {
		t.Fatalf("created entry = %+v, want revision 1 under %s", e, anchor)
	}

	// search: case-insensitive substring
	got, err := s.Search(ctx, memory.SearchQuery{Anchor: anchor, Query: "GO"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != e.ID {
		t.Fatalf("search = %+v, want the one entry", got)
	}

	// export
	all, err := s.Export(ctx, anchor)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("export = %d entries, want 1", len(all))
	}

	// replace at rev 1 → rev 2
	updated, err := s.Replace(ctx, e.ID, 1, memory.Patch{Text: strPtr("likes go and rust")})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if updated.Revision != 2 || updated.ETag != `"2"` || updated.Text != "likes go and rust" {
		t.Fatalf("updated = %+v, want revision 2 with the new text", updated)
	}
	if updated.ContentType != "fact" {
		t.Fatalf("unpatched field changed: %+v", updated.ContentType)
	}

	// stale replace → ErrConflict carrying the current
	_, err = s.Replace(ctx, e.ID, 1, memory.Patch{Text: strPtr("stale")})
	if !memory.IsConflict(err) {
		t.Fatalf("stale replace: err = %v, want ErrConflict", err)
	}
	if c := memory.ConflictOf(err); c.Current.Revision != 2 || c.Current.Text != "likes go and rust" {
		t.Fatalf("conflict current = %+v, want the current entry", c.Current)
	}

	// unknown id → ErrNotFound
	if _, err := s.Replace(ctx, "nope", 1, memory.Patch{Text: strPtr("x")}); err != memory.ErrNotFound {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}

	// forget → invisible to search, unreplaceable
	if err := s.Forget(ctx, e.ID, 2); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got, err := s.Search(ctx, memory.SearchQuery{Anchor: anchor}); err != nil || len(got) != 0 {
		t.Fatalf("search after forget = %d entries, err %v; want 0", len(got), err)
	}
	if _, err := s.Replace(ctx, e.ID, 3, memory.Patch{Text: strPtr("x")}); err != memory.ErrNotFound {
		t.Fatalf("replace forgotten: err = %v, want ErrNotFound", err)
	}

	// stale restore (tombstone is at rev 3) → conflict
	if _, err := s.Restore(ctx, e.ID, 1); !memory.IsConflict(err) {
		t.Fatalf("stale restore: err = %v, want ErrConflict", err)
	}
	// restore at the tombstone revision → live at rev 4
	restored, err := s.Restore(ctx, e.ID, 3)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Revision != 4 || restored.Text != "likes go and rust" {
		t.Fatalf("restored = %+v, want revision 4 with the pre-forget text", restored)
	}
	if got, err := s.Search(ctx, memory.SearchQuery{Anchor: anchor, Query: "rust"}); err != nil || len(got) != 1 {
		t.Fatalf("search after restore = %d entries, err %v; want 1", len(got), err)
	}
}

func TestStoreIdempotency(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	anchor := memory.Anchor{Kind: memory.KindTeam, ID: "t1"}
	first, err := s.Create(ctx, memory.NewEntry{
		Anchor:         anchor,
		ContentType:    "fact",
		Text:           "original",
		IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// replay: same key, same payload → same entry
	replay, err := s.Create(ctx, memory.NewEntry{
		Anchor:         anchor,
		ContentType:    "fact",
		Text:           "original",
		IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.ID != first.ID || replay.Revision != 1 {
		t.Fatalf("replay = %+v, want the first entry %+v", replay, first)
	}
	if all, _ := s.Export(ctx, anchor); len(all) != 1 {
		t.Fatalf("export after replay = %d entries, want 1", len(all))
	}

	// same key, different payload → conflict with the first entry
	_, err = s.Create(ctx, memory.NewEntry{
		Anchor:         anchor,
		ContentType:    "fact",
		Text:           "changed",
		IdempotencyKey: "k1",
	})
	if !memory.IsConflict(err) {
		t.Fatalf("key mismatch: err = %v, want ErrConflict", err)
	}

	// different key → a new entry
	other, err := s.Create(ctx, memory.NewEntry{
		Anchor:         anchor,
		ContentType:    "fact",
		Text:           "another",
		IdempotencyKey: "k2",
	})
	if err != nil {
		t.Fatalf("create k2: %v", err)
	}
	if other.ID == first.ID {
		t.Fatal("k2 reused the first entry's id")
	}
	if all, _ := s.Export(ctx, anchor); len(all) != 2 {
		t.Fatalf("export = %d entries, want 2", len(all))
	}

	// replay after the entry is forgotten → ErrNotFound
	if err := s.Forget(ctx, first.ID, 1); err != nil {
		t.Fatalf("forget: %v", err)
	}
	_, err = s.Create(ctx, memory.NewEntry{
		Anchor:         anchor,
		ContentType:    "fact",
		Text:           "original",
		IdempotencyKey: "k1",
	})
	if err != memory.ErrNotFound {
		t.Fatalf("replay after forget: err = %v, want ErrNotFound", err)
	}
}

func TestStoreConcurrentCAS(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	anchor := memory.Anchor{Kind: memory.KindAgent, ID: "a1"}
	e, err := s.Create(ctx, memory.NewEntry{Anchor: anchor, ContentType: "fact", Text: "base"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 8
	type outcome struct {
		entry memory.Entry
		err   error
	}
	outcomes := make([]outcome, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			entry, err := s.Replace(ctx, e.ID, 1, memory.Patch{Text: strPtr("writer-" + string(rune('0'+i)))})
			outcomes[i] = outcome{entry: entry, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	var winnerText string
	for i, o := range outcomes {
		if o.err == nil {
			successes++
			if o.entry.Revision != 2 {
				t.Fatalf("winner %d at revision %d, want 2", i, o.entry.Revision)
			}
			winnerText = o.entry.Text
			continue
		}
		if !memory.IsConflict(o.err) {
			t.Fatalf("writer %d: err = %v, want ErrConflict", i, o.err)
		}
		if c := memory.ConflictOf(o.err); c.Current.Revision != 2 {
			t.Fatalf("writer %d: conflict current at revision %d, want 2", i, c.Current.Revision)
		}
	}
	if successes != 1 {
		t.Fatalf("%d successful replaces, want exactly 1", successes)
	}
	// every loser must have seen the winner's state
	for i, o := range outcomes {
		if o.err == nil {
			continue
		}
		if c := memory.ConflictOf(o.err); c.Current.Text != winnerText {
			t.Fatalf("writer %d: conflict current text = %q, want the winner's %q", i, c.Current.Text, winnerText)
		}
	}
	all, err := s.Export(ctx, anchor)
	if err != nil || len(all) != 1 || all[0].Revision != 2 || all[0].Text != winnerText {
		t.Fatalf("export = %+v (err %v), want one entry at rev 2 with the winner's text", all, err)
	}
}

func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	anchor := memory.Anchor{Kind: memory.KindHuman, ID: "bob"}
	e, err := s.Create(ctx, memory.NewEntry{
		Anchor:         anchor,
		ContentType:    "fact",
		Text:           "persist me",
		IdempotencyKey: "p1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	all, err := s2.Export(ctx, anchor)
	if err != nil || len(all) != 1 || all[0].ID != e.ID {
		t.Fatalf("export after reopen = %+v (err %v), want the persisted entry", all, err)
	}

	// idempotency rows survive too
	replay, err := s2.Create(ctx, memory.NewEntry{
		Anchor:         anchor,
		ContentType:    "fact",
		Text:           "persist me",
		IdempotencyKey: "p1",
	})
	if err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	if replay.ID != e.ID {
		t.Fatalf("replay after reopen = %s, want %s", replay.ID, e.ID)
	}
}
