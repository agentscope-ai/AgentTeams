package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type coordinationAdmission struct {
	Accepted  bool   `json:"accepted,omitempty"`
	Recipient string `json:"recipient,omitempty"`
	EventID   string `json:"event_id,omitempty"`
	Reused    bool   `json:"reused,omitempty"`
	External  bool   `json:"external,omitempty"`
}

type coordinationAdmissionError struct {
	Status  int
	Message string
}

func (e *coordinationAdmissionError) Error() string { return e.Message }

// enqueue is the single admission boundary for HTTP callers and Task actions.
// It always rebuilds authority from the caller and current canonical state.
func (h *CoordinationHandler) enqueue(ctx context.Context, event coordinationEvent, caller *authpkg.CallerIdentity) (coordinationAdmission, error) {
	fail := func(status int, message string) (coordinationAdmission, error) {
		return coordinationAdmission{}, &coordinationAdmissionError{status, message}
	}
	if h.deps.OSS == nil {
		return fail(503, "coordination storage unavailable")
	}
	event.ActorName, event.ActorRole, event.Team, event.StorageTeam, event.ID = "", "", "", "", ""
	event.External, event.EligibleAssignees = false, nil
	resolved, err := h.resolve(ctx, event, caller)
	if err != nil {
		if errors.Is(err, errCoordinationRejected) {
			return fail(403, "coordination event rejected")
		}
		return fail(503, "coordination routing unavailable")
	}
	if resolved.External {
		return coordinationAdmission{External: true}, nil
	}
	for _, prefix := range []string{coordinationDone, coordinationPending} {
		data, err := h.deps.OSS.GetObject(ctx, prefix+resolved.ID+".json")
		if err == nil {
			var old coordinationDelivery
			if json.Unmarshal(data, &old) != nil {
				return fail(503, "invalid coordination record")
			}
			previous, _ := json.Marshal(old.Event)
			current, _ := json.Marshal(resolved)
			if !bytes.Equal(previous, current) {
				return fail(409, "coordination identity already has a different payload")
			}
			if prefix == coordinationPending {
				h.wakeRunner()
			}
			return coordinationAdmission{Accepted: true, EventID: resolved.ID, Recipient: resolved.RecipientName, Reused: true}, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fail(503, "coordination storage unavailable")
		}
	}
	if err := h.registerHandoffTasks(ctx, resolved); err != nil {
		if errors.Is(err, errCoordinationRejected) {
			return fail(400, "invalid main task requests")
		}
		if errors.Is(err, oss.ErrPreconditionFailed) {
			return fail(409, "main task ID already belongs to another request")
		}
		return fail(503, "main task registration unavailable")
	}
	delivery := coordinationDelivery{Event: resolved, Status: "queued", Created: time.Now().UTC(), Transport: coordinationTransport(resolved.Kind, resolved.RecipientRuntime)}
	data, _ := json.Marshal(delivery)
	if err := h.deps.OSS.PutObjectIfAbsent(ctx, coordinationPending+resolved.ID+".json", data); err != nil {
		if errors.Is(err, oss.ErrPreconditionFailed) {
			return fail(409, "coordination admission raced; retry unchanged event")
		}
		return fail(503, "coordination queue unavailable")
	}
	h.wakeRunner()
	return coordinationAdmission{Accepted: true, EventID: resolved.ID, Recipient: resolved.RecipientName}, nil
}
