package controller

import (
	"errors"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
)

func TestComputeMemberPhase(t *testing.T) {
	tests := []struct {
		name           string
		currentPhase   string
		matrixUserID   string
		desiredState   string
		containerState string
		reconcileErr   error
		want           string
	}{
		// Running path
		{
			name:           "Running + running = Running",
			desiredState:   "Running",
			containerState: string(backend.StatusRunning),
			want:           "Running",
		},
		{
			name:           "Running + ready = Running",
			desiredState:   "Running",
			containerState: string(backend.StatusReady),
			want:           "Running",
		},
		{
			name:           "Running + starting = Starting",
			desiredState:   "Running",
			containerState: string(backend.StatusStarting),
			want:           "Starting",
		},
		{
			name:           "Running + not_found = Pending",
			desiredState:   "Running",
			containerState: string(backend.StatusNotFound),
			want:           "Pending",
		},
		{
			name:           "Running + stopped = Pending",
			desiredState:   "Running",
			containerState: string(backend.StatusStopped),
			want:           "Pending",
		},
		{
			name:           "Running + sleeping = Pending",
			desiredState:   "Running",
			containerState: string(backend.StatusSleeping),
			want:           "Pending",
		},
		{
			name:           "Running + unknown = Pending",
			desiredState:   "Running",
			containerState: string(backend.StatusUnknown),
			want:           "Pending",
		},
		// Sleeping path
		{
			name:           "Sleeping + stopping = Stopping",
			desiredState:   "Sleeping",
			containerState: "stopping",
			want:           "Stopping",
		},
		{
			name:           "Sleeping + sleeping = Sleeping",
			desiredState:   "Sleeping",
			containerState: string(backend.StatusSleeping),
			want:           "Sleeping",
		},
		{
			name:           "Sleeping + not_found = Sleeping",
			desiredState:   "Sleeping",
			containerState: string(backend.StatusNotFound),
			want:           "Sleeping",
		},
		// Stopped path
		{
			name:           "Stopped + stopping = Stopping",
			desiredState:   "Stopped",
			containerState: "stopping",
			want:           "Stopping",
		},
		{
			name:           "Stopped + not_found = Stopped",
			desiredState:   "Stopped",
			containerState: string(backend.StatusNotFound),
			want:           "Stopped",
		},
		// Error path
		{
			name:         "Error + no matrixUserID = Failed",
			matrixUserID: "",
			desiredState: "Running",
			reconcileErr: errors.New("provision failed"),
			want:         "Failed",
		},
		{
			name:         "Error + matrixUserID + no phase = Pending",
			matrixUserID: "@bot:example.com",
			currentPhase: "",
			desiredState: "Running",
			reconcileErr: errors.New("transient"),
			want:         "Pending",
		},
		{
			name:         "Error + matrixUserID + existing phase = preserve",
			matrixUserID: "@bot:example.com",
			currentPhase: "Running",
			desiredState: "Running",
			reconcileErr: errors.New("transient"),
			want:         "Running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeMemberPhase(tt.currentPhase, tt.matrixUserID, tt.desiredState, tt.containerState, tt.reconcileErr)
			if got != tt.want {
				t.Errorf("computeMemberPhase() = %q, want %q", got, tt.want)
			}
		})
	}
}
