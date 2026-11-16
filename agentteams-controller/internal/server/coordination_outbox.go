package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

const coordinationPending = "control/coordination/pending/"
const coordinationDone = "control/coordination/completed/"

type coordinationDelivery struct {
	Transport      string            `json:"transport,omitempty"`
	Approvals      map[string]string `json:"approvals,omitempty"`
	Event          coordinationEvent `json:"event"`
	Status         string            `json:"status"`
	NativeID       string            `json:"native_id,omitempty"`
	Created        time.Time         `json:"created"`
	NextAttempt    time.Time         `json:"next_attempt"`
	LeaseUntil     time.Time         `json:"lease_until"`
	Attempts       int               `json:"attempts"`
	Error          string            `json:"error,omitempty"`
	MatrixEventID  string            `json:"matrix_event_id,omitempty"`
	AckEventID     string            `json:"ack_event_id,omitempty"`
	UserEventID    string            `json:"user_event_id,omitempty"`
	FailureEventID string            `json:"failure_event_id,omitempty"`
}

// PostEvent acknowledges durable queue admission, not delivery or acceptance.
func (h *CoordinationHandler) PostEvent(w http.ResponseWriter, r *http.Request) {
	if h.deps.OSS == nil {
		writeError(w, 503, "coordination storage unavailable")
		return
	}
	var event coordinationEvent
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := decoder.Decode(&event); err != nil {
		writeError(w, 400, "invalid coordination event")
		return
	}
	// Authority facts are rebuilt from authenticated identity and canonical state.
	event.ActorName = ""
	event.ActorRole = ""
	event.Team = ""
	event.StorageTeam = ""
	event.ID = ""
	event.External = false
	event.EligibleAssignees = nil
	resolved, err := h.resolve(r.Context(), event, authpkg.CallerFromContext(r.Context()))
	if err != nil {
		if errors.Is(err, errCoordinationRejected) {
			writeError(w, 403, "coordination event rejected")
		} else {
			writeError(w, 503, "coordination routing unavailable")
		}
		return
	}
	if resolved.External {
		httputil.WriteJSON(w, 200, map[string]any{"external": true})
		return
	}
	for _, prefix := range []string{coordinationDone, coordinationPending} {
		data, err := h.deps.OSS.GetObject(r.Context(), prefix+resolved.ID+".json")
		if err == nil {
			var old coordinationDelivery
			if json.Unmarshal(data, &old) != nil {
				writeError(w, 503, "invalid coordination record")
				return
			}
			// A retry cannot replace the payload associated with this execution fence.
			previous, _ := json.Marshal(old.Event)
			current, _ := json.Marshal(resolved)
			if !bytes.Equal(previous, current) {
				writeError(w, 409, "coordination identity already has a different payload")
				return
			}
			httputil.WriteJSON(w, 202, map[string]any{"accepted": true, "event_id": resolved.ID, "reused": true})
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			writeError(w, 503, "coordination storage unavailable")
			return
		}
	}
	delivery := coordinationDelivery{Event: resolved, Status: "queued", Created: time.Now().UTC()}
	delivery.Transport = coordinationTransport(resolved.Kind)
	data, _ := json.Marshal(delivery)
	if err := h.deps.OSS.PutObjectIfAbsent(r.Context(), coordinationPending+resolved.ID+".json", data); err != nil {
		if errors.Is(err, oss.ErrPreconditionFailed) {
			writeError(w, 409, "coordination admission raced; retry unchanged event")
			return
		}
		writeError(w, 503, "coordination queue unavailable")
		return
	}
	httputil.WriteJSON(w, 202, map[string]any{"accepted": true, "event_id": resolved.ID})
}

// Run resumes pending receipts after restart. CAS leases serialize replicas;
// no model is involved in retry, address lookup, pause or stale-event decisions.
func (h *CoordinationHandler) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		h.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (h *CoordinationHandler) drain(ctx context.Context) {
	if h.deps.OSS == nil {
		return
	}
	cursor := ""
	for {
		entries, next, err := h.deps.OSS.ListObjectsRecursive(ctx, coordinationPending, cursor, 100)
		if err != nil {
			return
		}
		// Bound concurrent network I/O without serializing all Worker callbacks.
		sem := make(chan struct{}, 12)
		done := make(chan struct{}, len(entries))
		for _, entry := range entries {
			key := path.Join(coordinationPending, entry.Name)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() { defer func() { <-sem; done <- struct{}{} }(); h.step(ctx, key) }()
		}
		for range entries {
			select {
			case <-done:
			case <-ctx.Done():
				return
			}
		}
		if next == "" {
			return
		}
		cursor = next
	}
}
func (h *CoordinationHandler) step(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	strict, ok := h.deps.OSS.(conditionalWriter)
	if !ok {
		return
	}
	data, err := h.deps.OSS.GetObject(ctx, key)
	if err != nil {
		return
	}
	var d coordinationDelivery
	if json.Unmarshal(data, &d) != nil {
		return
	}
	now := time.Now().UTC()
	if d.LeaseUntil.After(now) || d.NextAttempt.After(now) {
		return
	}
	d.LeaseUntil = now.Add(30 * time.Second)
	claim, _ := json.Marshal(d)
	if strict.PutObjectIfMatchStrict(ctx, key, claim, contentETag(data)) != nil {
		return
	}
	if d.Status == "completed" || d.Status == "ignored" || d.Status == "failed" {
		// Resume a terminal receipt archive without invoking the Agent again.
	} else if d.Status == "dispatching" {
		// A crash/lost admission response is ambiguous. Never replay a possibly
		// side-effecting execution; a runtime idempotency receipt is required.
		d.Status = "failed"
		d.Error = "native admission outcome unknown; operator recovery required"
	} else {
		h.advance(ctx, &d, func() error {
			next, _ := json.Marshal(d)
			if err := strict.PutObjectIfMatchStrict(ctx, key, next, contentETag(claim)); err != nil {
				return err
			}
			claim = next
			return nil
		})
	}
	d.LeaseUntil = time.Time{}
	if d.Status == "failed" && d.FailureEventID == "" {
		id, err := h.projectEvent(ctx, d.Event, d.Event.ActorRole, d.Event.ActorName, d.Event.RoomID,
			d.Event.ID+"-failure", "协作通知未完成 · "+coordinationLabel(d.Event)+"\n"+d.Error+"\n回执："+d.Event.ID)
		if err == nil {
			d.FailureEventID = id
		}
	}
	d.NextAttempt = now.Add(2 * time.Second)
	if d.Error != "" {
		d.Attempts++
		d.NextAttempt = now.Add(10 * time.Second)
	}
	updated, _ := json.Marshal(d)
	if strict.PutObjectIfMatchStrict(ctx, key, updated, contentETag(claim)) != nil {
		return
	}
	if d.Status == "completed" || d.Status == "ignored" || d.Status == "failed" {
		if d.Status == "failed" && d.FailureEventID == "" {
			return // Retry the idempotent projection, never the Agent execution.
		}
		if err = h.deps.OSS.PutObjectIfAbsent(ctx, coordinationDone+d.Event.ID+".json", updated); err != nil && !errors.Is(err, oss.ErrPreconditionFailed) {
			return
		}
		// Archive first: a repeated admission can always find its durable receipt.
		_ = h.deps.OSS.DeleteObject(ctx, key)
	}
}

// GetEvent exposes delivery receipts to the originating/receiving Agent and operators;
// task state and report contents stay on their existing scoped read surfaces.
func (h *CoordinationHandler) GetEvent(w http.ResponseWriter, r *http.Request) {
	if h.deps.OSS == nil {
		writeError(w, 503, "coordination storage unavailable")
		return
	}
	id := r.PathValue("id")
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		writeError(w, 404, "coordination receipt not found")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		writeError(w, 403, "coordination receipt not available")
		return
	}
	for _, prefix := range []string{coordinationPending, coordinationDone} {
		data, err := h.deps.OSS.GetObject(r.Context(), prefix+id+".json")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			writeError(w, 503, "coordination storage unavailable")
			return
		}
		var d coordinationDelivery
		if json.Unmarshal(data, &d) != nil {
			writeError(w, 503, "invalid coordination receipt")
			return
		}
		recipient := false
		if d.Event.RecipientKind == "worker" && (caller.Role == authpkg.RoleWorker || caller.Role == authpkg.RoleTeamLeader) {
			worker, lookupErr := h.worker(r.Context(), caller.Username)
			recipient = lookupErr == nil && worker.Name == d.Event.RecipientName && worker.Status.MatrixUserID == d.Event.Recipient
		}
		if caller.Role != authpkg.RoleAdmin && (caller.Username != d.Event.ActorName || caller.Role != d.Event.ActorRole) && !recipient {
			writeError(w, 404, "coordination receipt not found")
			return
		}
		httputil.WriteJSON(w, 200, map[string]any{"event_id": id, "kind": d.Event.Kind, "project_id": d.Event.ProjectID, "task_id": d.Event.TaskID, "status": d.Status, "created": d.Created, "error": d.Error, "native_id": d.NativeID, "user_event_id": d.UserEventID, "failure_event_id": d.FailureEventID, "actor": d.Event.Actor, "recipient": d.Event.Recipient, "delivery_id": d.Event.DeliveryID, "submission_id": d.Event.SubmissionID, "room_id": d.Event.RoomID, "transport": d.Transport})
		return
	}
	writeError(w, 404, "coordination receipt not found")
}
