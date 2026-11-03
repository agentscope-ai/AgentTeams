package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// S2 signal events (#1394 aligned, INT-3). Single event stream: each event
// is one object under the same shared root as tasks —
//   team scope:  teams/{team}/shared/events/{teamID}/{eventID}.json
//   standalone:  shared/events/{teamID}/{eventID}.json
//
// The 14-event list itself lives in the cross-language contract
// plugins/teamharness/contracts/signal-mapping.json (F8 precedent —
// task-transitions.json: in-code table here + test cross-check against the
// JSON, Python reads the JSON at runtime). The controller's existing project
// event feed (GetProjectEvents, project-domain taskTransition stream) is a
// different domain and is left untouched.

// signalEvent carries the 11-field signal event shape
// (additionalProperties: false — exactly these fields, no more).
type signalEvent struct {
	EventID        string         `json:"event_id"`
	TaskID         string         `json:"task_id"` // "" for project-level events
	LoopID         *string        `json:"loop_id"`
	EventType      string         `json:"event_type"`
	IsSignal       bool           `json:"is_signal"`
	SourceEvent    signalSource   `json:"source_event"`
	Ref            string         `json:"ref"`
	Fingerprint    string         `json:"fingerprint"`
	IterationIndex *int           `json:"iteration_index"`
	Payload        map[string]any `json:"payload"`
	OccurredAt     string         `json:"occurred_at"`
}

// signalSource names the protocol event that produced the signal
// (signal_event.schema.json source_event: origin enum, request_id nullable).
type signalSource struct {
	Origin    string  `json:"origin"`
	RequestID *string `json:"request_id"`
}

// signalEmitter is the Go-side projection of one contract row's go:*
// emitter. Keyed by handler name; cross-checked against the contract JSON
// by TestContractCoversAllEmitters (F8 pattern).
type signalEmitter struct {
	SourceEvent string // contract row id — hashed into the event id
	EventType   string // one of the 7 closed SIG-* markers
	Origin      string // signal_event.schema.json origin enum
	Level       string // contract levels[] value (contract cross-check only)
}

// signalEmitters — the five Go write endpoints (#1172). source_event values
// must match the contract rows' source_events (the id_rule hash input).
var signalEmitters = map[string]signalEmitter{
	"PauseProject":    {SourceEvent: "project_paused", EventType: "SIG-DECISION", Origin: "decision_lock", Level: "warn"},
	"ResumeProject":   {SourceEvent: "project_resumed", EventType: "SIG-DECISION", Origin: "decision_reopen", Level: "info"},
	"ReplanProject":   {SourceEvent: "project_replanned", EventType: "SIG-DECISION", Origin: "decision_lock", Level: "warn"},
	"CancelTask":      {SourceEvent: "task_cancelled", EventType: "SIG-DECISION", Origin: "decision_lock", Level: "error"},
	"CompleteProject": {SourceEvent: "project_completed", EventType: "SIG-COMPLETE", Origin: "submit", Level: "info"},
}

// signalEventID = "ev_" + first 16 hex of sha256(teamID|projectID|taskID|
// loopID|eventType|sourceEvent|iteration|fingerprint|attempt|auditSeq)
// (contract id_rule v1.2). fingerprint is the hex sha256 of the ref bytes
// after the main write — the stable identity of the logical operation's
// result. auditSeq is the project's committed-operation revision (the
// project meta's audit_seq, advanced by markAuditFields on each committed
// write; 0 where the emitter carries no project revision): the persistent
// identity of the operation INSTANCE, so two distinct operations that would
// otherwise write byte-identical state within the same wall-clock second
// (same-actor pause→resume→pause with the same reason) still hash distinct
// inputs, while a retry of the same operation (same committed seq, same
// state bytes) re-lands on the same id (consumer dedupes, #1394). No raw
// timestamp is part of the identity — never. Empty strings where the emitter
// carries no such identity. Must stay byte-identical to the contract
// id_rule and to plugins/teamharness/mcp/server.py _signal_event_id (both
// test suites pin the same fixed vector).
func signalEventID(teamID, projectID, taskID, loopID, eventType, sourceEvent string, iteration *int, fingerprint string, attempt, auditSeq int) string {
	iter := ""
	if iteration != nil {
		iter = strconv.Itoa(*iteration)
	}
	sum := sha256.Sum256([]byte(teamID + "|" + projectID + "|" + taskID + "|" + loopID + "|" + eventType + "|" + sourceEvent + "|" + iter + "|" + fingerprint + "|" + strconv.Itoa(attempt) + "|" + strconv.Itoa(auditSeq)))
	return "ev_" + hex.EncodeToString(sum[:])[:16]
}

// signalEventKey places the per-event object under the same shared root as
// tasks (taskMetaKeys style: team scope vs standalone, deliberately no
// cross-scope fallback).
func signalEventKey(team, teamID, eventID string) string {
	if team != "" {
		return fmt.Sprintf("teams/%s/shared/events/%s/%s.json", team, teamID, eventID)
	}
	return fmt.Sprintf("shared/events/%s/%s.json", teamID, eventID)
}

// emitSignalEvent writes one per-event object, best-effort. It never blocks,
// fails, or rolls back the caller's request: the write it accompanies has
// already succeeded, the event is a convenience (same policy as
// interventionNotify). Idempotent: an existing object at the same key is
// left untouched (the deterministic id makes a re-emit a no-op).
// auditSeq is the project's committed-operation revision at the moment of
// this commit (id_rule v1.2) — for the project-write endpoints that is
// meta.AuditSeq as just written, and for CancelTask the revision persisted
// by the attempt that committed the project write (its convergence retry
// emits with the read value, not a fresh increment).
func (h *ProjectHandler) emitSignalEvent(ctx context.Context, em signalEmitter, team, teamID, projectID, taskID string, iteration *int, ref string, refBytes []byte, payload map[string]any, auditSeq int) {
	fingerprint := sha256.Sum256(refBytes)
	eventID := signalEventID(teamID, projectID, taskID, "", em.EventType, em.SourceEvent, iteration, hex.EncodeToString(fingerprint[:]), 0, auditSeq)
	key := signalEventKey(team, teamID, eventID)
	if _, err := h.oss.GetObject(ctx, key); err == nil {
		return // already emitted — idempotent re-emit, keep the original
	} else if !errors.Is(err, os.ErrNotExist) {
		log.FromContext(ctx).V(1).Info("signal event dedup check failed; proceeding with write", "key", key, "error", err.Error())
	}
	ev := signalEvent{
		EventID:        eventID,
		TaskID:         taskID,
		LoopID:         nil,
		EventType:      em.EventType,
		IsSignal:       true,
		SourceEvent:    signalSource{Origin: em.Origin, RequestID: nil},
		Ref:            ref,
		Fingerprint:    hex.EncodeToString(fingerprint[:]),
		IterationIndex: iteration,
		Payload:        payload,
		OccurredAt:     utcTimestamp(),
	}
	data, err := json.Marshal(ev)
	if err != nil {
		log.FromContext(ctx).V(1).Info("signal event marshal failed (best-effort, dropped)", "key", key, "error", err.Error())
		return
	}
	if err := h.oss.PutObject(ctx, key, data); err != nil {
		log.FromContext(ctx).V(1).Info("signal event emit failed (best-effort, dropped)", "key", key, "error", err.Error())
	}
}

// teamID resolves the event path's {team_id} segment: the stamped project
// team_id, then the resolved storage scope, then "global" (matches the
// Python side's _runtime_team_name fallback).
func signalTeamID(meta *projectMeta, team string) string {
	if meta.TeamID != "" {
		return meta.TeamID
	}
	if team != "" {
		return team
	}
	return "global"
}

// refBytes re-marshal a just-written project meta with the same serializer
// writeProjectMeta uses (json.Marshal), so the event fingerprint matches the
// bytes actually stored.
func refBytes(meta *projectMeta) []byte {
	data, err := json.Marshal(meta)
	if err != nil {
		return nil // best-effort: empty fingerprint rather than a failed request
	}
	return data
}
