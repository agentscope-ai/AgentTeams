package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// --- preflightRange ------------------------------------------------------------

func TestPreflightRange(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		size       int64
		wantStart  int64
		wantEnd    int64
		wantOK     bool
		wantReject bool
	}{
		{"no range", "", 100, 0, 0, false, false},
		{"first half", "bytes=0-4", 100, 0, 4, true, false},
		{"tail", "bytes=96-99", 100, 96, 99, true, false},
		{"start at size", "bytes=100-200", 100, 0, 0, false, true},
		{"open end", "bytes=5-", 100, 5, 99, true, false},
		{"open end at size", "bytes=100-", 100, 0, 0, false, true},
		{"suffix 10", "bytes=-10", 100, 90, 99, true, false},
		{"suffix larger than file", "bytes=-100", 100, 0, 99, true, false},
		{"suffix zero", "bytes=-0", 100, 0, 0, false, true},
		{"reversed", "bytes=9-4", 100, 0, 0, false, true},
		{"multi range", "bytes=0-4,6-8", 100, 0, 0, false, true},
		{"wrong unit", "items=0-4", 100, 0, 0, false, true},
		{"single byte", "bytes=5-5", 100, 5, 5, true, false},
		{"huge end clamps", "bytes=0-99999999999999999999", 100, 0, 99, true, false},
		{"huge start rejects", "bytes=99999999999999999999-100000000000000000000", 100, 0, 0, false, true},
		{"bare dash", "bytes=-", 100, 0, 0, false, true},
		{"non numeric", "bytes=abc", 100, 0, 0, false, true},
		{"negative literal", "bytes=-5-", 100, 0, 0, false, true},
		{"empty object rejects any range", "bytes=0-0", 0, 0, 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ok, reject := preflightRange(tc.raw, tc.size)
			if reject != tc.wantReject {
				t.Fatalf("reject = %v, want %v (start=%d end=%d ok=%v)", reject, tc.wantReject, start, end, ok)
			}
			if tc.wantReject {
				return
			}
			if ok != tc.wantOK || start != tc.wantStart || end != tc.wantEnd {
				t.Fatalf("got (%d,%d,%v), want (%d,%d,%v)", start, end, ok, tc.wantStart, tc.wantEnd, tc.wantOK)
			}
		})
	}
}

// --- objectSeeker ---------------------------------------------------------------

// noRangeStore adapts a StorageClient into one that does NOT implement
// oss.RangeReader: embedding the interface keeps every storage method but
// no OpenRange, so newObjectSeeker must fall back to the eager path.
type noRangeStore struct {
	oss.StorageClient
}

func seedSeekerFile(t *testing.T, content []byte) (*humanFilesHandler, *ossfake.Memory, string, []byte) {
	t.Helper()
	h, store, _ := newHumanFilesRig(t)
	key := humanFilesPrefix("alice") + "seeker.txt"
	if err := store.PutObject(context.Background(), key, content); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return h, store, key, content
}

func statRep(t *testing.T, h *humanFilesHandler, key string) fileRepresentation {
	t.Helper()
	rep, err := h.statFileRepresentation(context.Background(), key, "seeker.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return rep
}

func TestObjectSeeker_RangedPrimeOpensOnce(t *testing.T) {
	h, store, key, content := seedSeekerFile(t, bytes.Repeat([]byte("0123456789"), 10)) // 100 B
	rep := statRep(t, h, key)

	s := newObjectSeeker(h, context.Background(), key, rep)
	if s.rr == nil {
		t.Fatal("Memory-backed seeker must be ranged, got eager fallback")
	}
	if err := s.Prime(10, 20); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if got := store.OpenRangeCalls.Load(); got != 1 {
		t.Fatalf("Prime issued %d OpenRange calls, want exactly 1", got)
	}
	buf := make([]byte, 20)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatalf("read primed window: %v", err)
	}
	if !bytes.Equal(buf, content[10:30]) {
		t.Fatalf("primed window = %q, want %q", buf, content[10:30])
	}
	if got := s.BytesRead(); got != 20 {
		t.Fatalf("BytesRead = %d, want 20", got)
	}

	// The prime is a HINT, not a bound: reading past the primed window
	// continues from the next segment — it must never truncate the body.
	rest, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("read past window: %v", err)
	}
	if !bytes.Equal(rest, content[30:]) {
		t.Fatalf("past-window bytes = %q, want %q", rest, content[30:])
	}
	if got := s.Opens(); got != 2 {
		t.Fatalf("Opens = %d, want 2 (primed window + continuation)", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestObjectSeeker_SegmentWindow(t *testing.T) {
	// 5 MiB: the first segment covers the first 4 MiB, the tail its own.
	content := make([]byte, 5<<20)
	for i := range content {
		content[i] = byte(i%127 + 1)
	}
	h, _, key, _ := seedSeekerFile(t, content)
	rep := statRep(t, h, key)

	s := newObjectSeeker(h, context.Background(), key, rep)
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("full read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("body length %d differs from the seeded %d bytes", len(got), len(content))
	}
	if got := s.Opens(); got != 2 {
		t.Fatalf("Opens = %d, want 2 (4 MiB segment + 1 MiB tail)", got)
	}
}

func TestObjectSeeker_EOFAndSeek(t *testing.T) {
	h, _, key, _ := seedSeekerFile(t, bytes.Repeat([]byte("xy"), 50)) // 100 B
	rep := statRep(t, h, key)
	s := newObjectSeeker(h, context.Background(), key, rep)

	if pos, err := s.Seek(0, io.SeekEnd); err != nil || pos != rep.size {
		t.Fatalf("Seek(0,End) = (%d,%v), want (%d,nil)", pos, err, rep.size)
	}
	if pos, err := s.Seek(0, io.SeekStart); err != nil || pos != 0 {
		t.Fatalf("Seek(0,Start) = (%d,%v), want (0,nil)", pos, err)
	}
	if pos, err := s.Seek(5, io.SeekCurrent); err != nil || pos != 5 {
		t.Fatalf("Seek(5,Current) = (%d,%v), want (5,nil)", pos, err)
	}
	if pos, err := s.Seek(-3, io.SeekEnd); err != nil || pos != rep.size-3 {
		t.Fatalf("Seek(-3,End) = (%d,%v), want (%d,nil)", pos, err, rep.size-3)
	}
	if _, err := s.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("Seek(-1,Start) must fail (negative absolute offset)")
	}

	// Reading at the end of file is an immediate, clean EOF.
	if _, err := s.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("Seek(0,End): %v", err)
	}
	p := make([]byte, 16)
	if _, err := s.Read(p); err != io.EOF {
		t.Fatalf("Read at EOF = %v, want io.EOF", err)
	}
}

func TestObjectSeeker_PrimeStaleSnapshot(t *testing.T) {
	h, _, key, _ := seedSeekerFile(t, []byte("v1"))
	rep := statRep(t, h, key)
	s := newObjectSeeker(h, context.Background(), key, rep)

	if err := h.oss.PutObject(context.Background(), key, []byte("v2-longer")); err != nil {
		t.Fatalf("concurrent put: %v", err)
	}
	if err := s.Prime(0, 5); !errors.Is(err, oss.ErrPreconditionFailed) {
		t.Fatalf("Prime after version move = %v, want ErrPreconditionFailed", err)
	}
}

func TestObjectSeeker_EagerFallback(t *testing.T) {
	h, _, key, content := seedSeekerFile(t, []byte("eager payload"))
	// Drop the RangeReader capability: the seeker must serve the whole
	// body through the eager path instead.
	h.oss = &noRangeStore{StorageClient: h.oss}
	rep := statRep(t, h, key)

	s := newObjectSeeker(h, context.Background(), key, rep)
	if s.rr != nil {
		t.Fatal("wrapper without OpenRange must yield an eager seeker")
	}
	if err := s.Prime(0, 0); err != nil {
		t.Fatalf("eager Prime: %v", err)
	}
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("eager read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("eager body = %q, want %q", got, content)
	}
}

func TestObjectSeeker_EagerStaleSnapshot(t *testing.T) {
	h, _, key, _ := seedSeekerFile(t, []byte("base v1"))
	h.oss = &noRangeStore{StorageClient: h.oss}
	rep := statRep(t, h, key)

	if err := h.oss.PutObject(context.Background(), key, []byte("base v2 changed")); err != nil {
		t.Fatalf("concurrent put: %v", err)
	}
	s := newObjectSeeker(h, context.Background(), key, rep)
	if err := s.loadEager(); !errors.Is(err, errHumanFileSnapshotUnstable) {
		t.Fatalf("eager load after version move = %v, want errHumanFileSnapshotUnstable", err)
	}
}

func TestObjectSeeker_CloseIdempotent(t *testing.T) {
	h, _, key, _ := seedSeekerFile(t, []byte("close me"))
	rep := statRep(t, h, key)
	s := newObjectSeeker(h, context.Background(), key, rep)
	if err := s.Prime(0, 5); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// The preflight and the seeker must agree on the window bounds for the
// suffix and open-end shapes (guards the shared parse path).
func TestPreflightRangeWindowConsistency(t *testing.T) {
	for _, raw := range []string{"bytes=-10", "bytes=-100", "bytes=5-", "bytes=0-4"} {
		start, end, ok, reject := preflightRange(raw, 100)
		if reject || !ok {
			t.Fatalf("%s: unexpected preflight result (%d,%d,%v)", raw, start, end, ok)
		}
		if start > end || end >= 100 {
			t.Fatalf("%s: window (%d,%d) outside [0,100)", raw, start, end)
		}
	}
}
