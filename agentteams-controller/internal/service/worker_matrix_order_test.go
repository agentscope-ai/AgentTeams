package service

import (
	"slices"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestWorkerMatrixPendingKeepsAcceptedBatchOrder(t *testing.T) {
	s, start, etag := startMatrixStore(t, ossfake.NewMemory())
	inputs := []WorkerMatrixInput{matrixInput("$one"), matrixInput("$two")}
	key := func(input WorkerMatrixInput) string { return WorkerMatrixEventKey(input.RoomID, input.EventID) }
	if key(inputs[0]) < key(inputs[1]) {
		inputs[0], inputs[1] = inputs[1], inputs[0]
	}
	state, etag, err := s.Ingest(t.Context(), "worker", start, etag, WorkerMatrixConsumer{}, inputs, nil)
	want := []string{key(inputs[0]), key(inputs[1])}
	if err != nil || !slices.Equal(state.Pending, want) {
		t.Fatalf("message identities were sorted instead of retaining batch order: %+v %v", state, err)
	}
	state, _, err = s.Ingest(t.Context(), "worker", start, etag, WorkerMatrixConsumer{}, []WorkerMatrixInput{inputs[1], inputs[0]}, nil)
	if err != nil || !slices.Equal(state.Pending, want) {
		t.Fatalf("redelivery reordered accepted messages: %+v %v", state, err)
	}
}
