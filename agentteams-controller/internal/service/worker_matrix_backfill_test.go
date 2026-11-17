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
