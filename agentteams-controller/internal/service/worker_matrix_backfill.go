package service

import (
	"context"
	"fmt"
	"slices"
)

// Backfill retains a limited timeline's hole until pagination reaches its old
// coverage anchor. Missing end may be a visibility boundary, not that anchor.
// Both page order and pagination ownership come from the shared snapshot.
func (s *WorkerMatrixStore) Backfill(ctx context.Context, name string, start WorkerStart, expected string, page WorkerMatrixBackfill) (*WorkerMatrixState, string, error) {
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	if expected == "" || etag != expected || state.ActiveStart != start {
		return nil, "", ErrWorkerStateConflict
	}
	room := state.Rooms[page.RoomID]
	gap, exists := room.Backfill[page.GapID]
	if !exists || !slices.Contains(room.Gaps, page.GapID) || page.FromToken == "" || page.FromToken != gap.FromToken {
		return nil, "", ErrWorkerStateConflict
	}
	if page.NextToken != "" && page.NextToken == page.FromToken {
		return nil, "", fmt.Errorf("%w: pagination did not advance", ErrWorkerStateInvalid)
	}
	before := slices.Index(state.Pending, gap.Before)
	if before < 0 {
		return nil, "", fmt.Errorf("%w: missing gap ordering anchor requires verification", ErrWorkerStateConflict)
	}
	inputs := slices.Clone(page.Inputs)
	reached := false
	for i, input := range inputs {
		if input.RoomID != page.RoomID {
			return nil, "", fmt.Errorf("%w: backfill page room changed", ErrWorkerStateInvalid)
		}
		if gap.Until != "" && input.EventID == gap.Until {
			inputs, reached = inputs[:i+1], true
			break // Older history is already covered; never enqueue it again.
		}
	}
	slices.Reverse(inputs)
	keys, err := s.persistInputs(ctx, name, state, inputs)
	if err != nil {
		return nil, "", err
	}
	insert := []string{}
	for _, key := range keys {
		if key != WorkerMatrixEventKey(page.RoomID, gap.Until) && !slices.Contains(state.Pending, key) && !slices.Contains(insert, key) {
			insert = append(insert, key)
		}
	}
	state.Pending = slices.Insert(state.Pending, before, insert...)
	if reached {
		room.Gaps = slices.DeleteFunc(room.Gaps, func(id string) bool { return id == page.GapID })
		delete(room.Backfill, page.GapID)
	} else {
		if len(insert) > 0 {
			gap.Before = insert[0]
		}
		if page.NextToken != "" {
			gap.FromToken, gap.Boundary = page.NextToken, false
		} else {
			gap.Boundary = true // Retained for verification/permission repair.
		}
		room.Backfill[page.GapID] = gap
	}
	state.Rooms[page.RoomID] = room
	return s.commit(ctx, state, etag)
}
