package service

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func limitedMatrixStore(t *testing.T, storage *matrixFaultStore) (*WorkerMatrixStore, WorkerStart, *WorkerMatrixState, string) {
	t.Helper()
	s, start, etag := startMatrixStore(t, storage)
	_, etag, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "one"), []WorkerMatrixInput{matrixInput("$old")},
		map[string]WorkerMatrixRoom{"!room:test": {Through: "$old"}})
	if err != nil {
		t.Fatal(err)
	}
	state, etag, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "two", "one"),
		[]WorkerMatrixInput{matrixInput("$new-one"), matrixInput("$new-two")},
		map[string]WorkerMatrixRoom{"!room:test": {Through: "$new-two", Gaps: []string{"limited-prev"}}})
	if err != nil {
		t.Fatal(err)
	}
	return s, start, state, etag
}

func TestWorkerMatrixBackfillReconnectsKnownAnchorInServerPageOrder(t *testing.T) {
	s, start, state, etag := limitedMatrixStore(t, &matrixFaultStore{Memory: ossfake.NewMemory()})
	gap := state.Rooms["!room:test"].Backfill["limited-prev"]
	if gap.Until != "$old" || gap.Before != WorkerMatrixEventKey("!room:test", "$new-one") || gap.FromToken != "limited-prev" {
		t.Fatalf("limited timeline lost coverage boundary: %+v", gap)
	}
	page := WorkerMatrixBackfill{RoomID: "!room:test", GapID: "limited-prev", FromToken: "limited-prev", NextToken: "older-page",
		Inputs: []WorkerMatrixInput{matrixInput("$middle-two"), matrixInput("$middle-one")}}
	state, etag, err := s.Backfill(t.Context(), "worker", start, etag, page)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(state *WorkerMatrixState) []string {
		result := []string{}
		for _, key := range state.Pending {
			input, err := s.Input(t.Context(), "worker", key)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, input.EventID)
		}
		return result
	}
	if got := ids(state); !reflect.DeepEqual(got, []string{"$old", "$middle-one", "$middle-two", "$new-one", "$new-two"}) {
		t.Fatalf("backfill was appended after newer pending work: %v", got)
	}
	// An empty filtered page with an end token is not the end of history.
	page.FromToken, page.NextToken, page.Inputs = "older-page", "last-page", nil
	state, etag, err = s.Backfill(t.Context(), "worker", start, etag, page)
	if err != nil || len(state.Rooms[page.RoomID].Gaps) != 1 {
		t.Fatalf("empty page closed known gap: %+v %v", state, err)
	}
	page.FromToken, page.NextToken = "last-page", ""
	page.Inputs = []WorkerMatrixInput{matrixInput("$missing"), matrixInput("$old"), matrixInput("$older-already-covered")}
	state, _, err = s.Backfill(t.Context(), "worker", start, etag, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Rooms[page.RoomID].Gaps) != 0 || len(state.Rooms[page.RoomID].Backfill) != 0 || state.Rooms[page.RoomID].Through != "$new-two" {
		t.Fatalf("known anchor did not close only the gap: %+v", state.Rooms[page.RoomID])
	}
	if got := ids(state); !reflect.DeepEqual(got, []string{"$old", "$missing", "$middle-one", "$middle-two", "$new-one", "$new-two"}) {
		t.Fatalf("backfill reordered or replayed already covered history: %v", got)
	}
}

func TestWorkerMatrixBackfillBoundaryDoesNotEraseMissingKnownHistory(t *testing.T) {
	s, start, _, etag := limitedMatrixStore(t, &matrixFaultStore{Memory: ossfake.NewMemory()})
	state, _, err := s.Backfill(t.Context(), "worker", start, etag, WorkerMatrixBackfill{RoomID: "!room:test", GapID: "limited-prev", FromToken: "limited-prev",
		Inputs: []WorkerMatrixInput{matrixInput("$visible-before-permission-boundary")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Rooms["!room:test"].Gaps) != 1 || !state.Rooms["!room:test"].Backfill["limited-prev"].Boundary {
		t.Fatal("missing end token was treated as proof of complete coverage")
	}
}

func TestWorkerMatrixBackfillRejectsStaleTokenAndFencesOldStart(t *testing.T) {
	storage := &matrixFaultStore{Memory: ossfake.NewMemory()}
	s, start, _, etag := limitedMatrixStore(t, storage)
	page := WorkerMatrixBackfill{RoomID: "!room:test", GapID: "limited-prev", FromToken: "older-private-token", NextToken: "next",
		Inputs: []WorkerMatrixInput{matrixInput("$middle")}}
	if _, _, err := s.Backfill(t.Context(), "worker", start, etag, page); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("stale backfill page accepted: %v", err)
	}
	page.FromToken = "limited-prev"
	storage.failKey = "agents/worker/runtime/matrix/events/" + WorkerMatrixEventKey(page.RoomID, "$middle") + ".json"
	if _, _, err := s.Backfill(t.Context(), "worker", start, etag, page); err == nil {
		t.Fatal("failed input upload advanced the pagination checkpoint")
	}
	storage.failKey = ""
	storage.afterInput = func() {
		target := WorkerStart{InstanceID: "target/start", ProcessID: "new-process", Runtime: "qwenpaw"}
		if _, _, err := s.Start(t.Context(), "worker", target, etag); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Backfill(t.Context(), "worker", start, etag, page); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("old in-flight pagination survived replacement: %v", err)
	}
	state, _, err := s.Load(t.Context(), "worker")
	if err != nil || state.Rooms[page.RoomID].Backfill[page.GapID].FromToken != "limited-prev" {
		t.Fatalf("old callback advanced shared gap: %+v %v", state, err)
	}
}

func TestWorkerMatrixLimitedTimelineOverlapDoesNotCreateAnUnreachableGap(t *testing.T) {
	s, start, etag := startMatrixStore(t, ossfake.NewMemory())
	_, etag, err := s.Ingest(t.Context(), "worker", start, etag, WorkerMatrixConsumer{}, []WorkerMatrixInput{matrixInput("$old")}, map[string]WorkerMatrixRoom{"!room:test": {Through: "$old"}})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := s.Ingest(t.Context(), "worker", start, etag, WorkerMatrixConsumer{}, []WorkerMatrixInput{matrixInput("$old"), matrixInput("$new")}, map[string]WorkerMatrixRoom{"!room:test": {Through: "$new", Gaps: []string{"prev-before-old"}}})
	if err != nil || len(state.Rooms["!room:test"].Gaps) != 0 {
		t.Fatalf("overlapping timeline forced backfill behind an already covered anchor: %+v %v", state, err)
	}
}

func TestWorkerMatrixFilteredInitialWindowBackfillsAtQueuePosition(t *testing.T) {
	const roomID = "!room:test"
	s, start, etag := startMatrixStore(t, ossfake.NewMemory())
	state, etag, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "first"), nil,
		map[string]WorkerMatrixRoom{roomID: {Gaps: []string{"older"}}})
	if err != nil || len(state.Pending) != 0 || state.Rooms[roomID].Through != "" {
		t.Fatalf("filtered window failed to commit cursor and gap together: %+v %v", state, err)
	}
	state, etag, err = s.Backfill(t.Context(), "worker", start, etag, WorkerMatrixBackfill{
		RoomID: roomID, GapID: "older", FromToken: "older", NextToken: "next-page"})
	if err != nil || len(state.Rooms[roomID].Gaps) != 1 {
		t.Fatalf("empty intermediate page closed history: %+v %v", state, err)
	}
	_, etag, err = s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "live", "first"),
		[]WorkerMatrixInput{matrixInput("$live")}, map[string]WorkerMatrixRoom{roomID: {Through: "$live"}})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = s.Backfill(t.Context(), "worker", start, etag, WorkerMatrixBackfill{
		RoomID: roomID, GapID: "older", FromToken: "next-page",
		Inputs: []WorkerMatrixInput{matrixInput("$middle"), matrixInput("$oldest")}})
	want := []string{WorkerMatrixEventKey(roomID, "$oldest"), WorkerMatrixEventKey(roomID, "$middle"), WorkerMatrixEventKey(roomID, "$live")}
	if err != nil || !reflect.DeepEqual(state.Pending, want) || len(state.Rooms[roomID].Gaps) != 0 || state.Rooms[roomID].Through != "$live" {
		t.Fatalf("first visibility boundary lost ordering/coverage: %+v %v", state, err)
	}
}

func TestWorkerMatrixFilteredFreshRoomAdoptsNewestVisibleInput(t *testing.T) {
	const roomID = "!room:test"
	s, start, etag := startMatrixStore(t, ossfake.NewMemory())
	_, etag, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "first"), nil,
		map[string]WorkerMatrixRoom{roomID: {Gaps: []string{"gap"}}})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := s.Backfill(t.Context(), "worker", start, etag, WorkerMatrixBackfill{RoomID: roomID,
		GapID: "gap", FromToken: "gap", Inputs: []WorkerMatrixInput{matrixInput("$newest"), matrixInput("$oldest")}})
	if err != nil || state.Rooms[roomID].Through != "$newest" || len(state.Rooms[roomID].Gaps) != 0 {
		t.Fatalf("missing reconnect anchor after visible history exhausted: %+v %v", state, err)
	}
}

func TestWorkerMatrixFilteredWindowStillRequiresKnownAnchor(t *testing.T) {
	const roomID = "!room:test"
	s, start, etag := startMatrixStore(t, ossfake.NewMemory())
	_, etag, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "first"),
		[]WorkerMatrixInput{matrixInput("$old")}, map[string]WorkerMatrixRoom{roomID: {Through: "$old"}})
	if err != nil {
		t.Fatal(err)
	}
	_, etag, err = s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "next", "first"), nil,
		map[string]WorkerMatrixRoom{roomID: {Through: "$old", Gaps: []string{"gap"}}})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := s.Backfill(t.Context(), "worker", start, etag, WorkerMatrixBackfill{
		RoomID: roomID, GapID: "gap", FromToken: "gap"})
	if err != nil || !state.Rooms[roomID].Backfill["gap"].Boundary || len(state.Rooms[roomID].Gaps) != 1 {
		t.Fatalf("exhaustion erased missing known anchor: %+v %v", state, err)
	}
}

func TestWorkerMatrixConsecutiveFilteredWindowsKeepRoomOrder(t *testing.T) {
	const roomID = "!room:test"
	for _, pages := range [][]string{{"gap-one", "gap-two"}, {"gap-two", "gap-one"}} {
		t.Run(pages[0]+"-first", func(t *testing.T) {
			s, start, etag := startMatrixStore(t, ossfake.NewMemory())
			var err error
			for _, window := range []struct{ from, token, gap string }{{"", "one", "gap-one"}, {"one", "two", "gap-two"}} {
				_, etag, err = s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", window.token, window.from), nil,
					map[string]WorkerMatrixRoom{roomID: {Gaps: []string{window.gap}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			var state *WorkerMatrixState
			for _, gap := range pages {
				state, etag, err = s.Backfill(t.Context(), "worker", start, etag, WorkerMatrixBackfill{
					RoomID: roomID, GapID: gap, FromToken: gap, Inputs: []WorkerMatrixInput{matrixInput("$" + gap)}})
				if err != nil {
					t.Fatal(err)
				}
			}
			want := []string{WorkerMatrixEventKey(roomID, "$gap-one"), WorkerMatrixEventKey(roomID, "$gap-two")}
			if !reflect.DeepEqual(state.Pending, want) || state.Rooms[roomID].Through != "$gap-two" {
				t.Fatalf("filtered windows lost chronological order: %+v", state)
			}
		})
	}
}
