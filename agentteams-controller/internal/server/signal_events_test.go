package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// S2 signal emission tests (INT-3). Contract cross-check follows the F8
// precedent (task-transitions.json): the in-code signalEmitters table is
// verified against plugins/teamharness/contracts/signal-mapping.json.

// Fixed cross-language vector (contract id_rule v1.2): first 16 hex of
// sha256("alpha-team|p1|t1||SIG-DECISION|project_paused||" +
// signalIDFixedFingerprint + "|0|1") — the trailing audit_seq=1 is the
// committed-operation revision for the first intervention on a project.
// The Python test suite (test_signal_emission.py) pins the exact same value,
// so a drift on either side fails both.
const signalIDFixedVector = "ev_afb958c6b34a7132"

// Fingerprint input of signalIDFixedVector: sha256 of the reference
// post-write state bytes `{"project_id": "p1"}`.
const signalIDFixedFingerprint = "6a454e45559e0a550fc0c106556516be8c0435281d643b53648c68d58c337c89"

// testSignalFingerprint hashes reference state bytes the way the emitters do.
func testSignalFingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// expectedSignalEventID derives the deterministic event id for an emit whose
// ref is the object currently stored at refKey: the id_rule fingerprint is
// the sha256 of exactly those stored post-write bytes. Call it right after
// the write that produced the bytes (a later operation rewrites the object
// and would change the fingerprint). auditSeq is the project's committed
// revision at that commit (id_rule v1.2).
func expectedSignalEventID(t *testing.T, store *ossfake.Memory, refKey, teamID, projectID, taskID, eventType, sourceEvent string, auditSeq int) string {
	t.Helper()
	data, err := store.GetObject(context.Background(), refKey)
	if err != nil {
		t.Fatalf("read ref %s for fingerprint: %v", refKey, err)
	}
	return signalEventID(teamID, projectID, taskID, "", eventType, sourceEvent, nil, testSignalFingerprint(data), 0, auditSeq)
}

// storedAuditSeq reads a project meta and returns its committed audit
// revision (0 for objects written before id_rule v1.2 — the tag has no
// omitempty, so current writes always serialize it).
func storedAuditSeq(t *testing.T, store *ossfake.Memory, metaKey string) int {
	t.Helper()
	data, err := store.GetObject(context.Background(), metaKey)
	if err != nil {
		t.Fatalf("read meta %s for audit_seq: %v", metaKey, err)
	}
	var m struct {
		AuditSeq int `json:"audit_seq"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode meta %s: %v", metaKey, err)
	}
	return m.AuditSeq
}

var signalEventTypeClosed = map[string]bool{
	"SIG-COMPLETE":  true,
	"SIG-DECISION":  true,
	"SIG-VERDICT":   true,
	"SIG-ARTIFACT":  true,
	"SIG-ATTENTION": true,
	"SIG-MISMATCH":  true,
	"SIG-CONTRACT":  true,
}

// origin enum of signal_event.schema.json (10 values).
var signalOriginClosed = map[string]bool{
	"submit":                    true,
	"decision_lock":             true,
	"decision_reopen":           true,
	"critic_verdict":            true,
	"stop_condition_predicate":  true,
	"artifact_write":            true,
	"handoff_write":             true,
	"attention_resolved":        true,
	"mismatch_recorded":         true,
	"contract_snapshot":         true,
}

type signalContractRow struct {
	SourceEvents []string  `json:"source_events"`
	EventType    string    `json:"event_type"`
	SourceLayer  string    `json:"source_layer"`
	Emitters     []string  `json:"emitters"`
	Level        string    `json:"level"`
	Conditional  *bool     `json:"conditional"`
	Type1394     *string   `json:"#1394_type"`
}

type signalContract struct {
	Version string              `json:"version"`
	IDRule  string              `json:"id_rule"`
	Levels  []string            `json:"levels"`
	Events  []signalContractRow `json:"events"`
}

func loadSignalContract(t *testing.T) signalContract {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "plugins", "teamharness", "contracts", "signal-mapping.json"))
	if err != nil {
		t.Fatalf("read signal-mapping.json: %v", err)
	}
	var c signalContract
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("decode signal-mapping.json: %v", err)
	}
	return c
}

func TestSignalEventIDFixedVector(t *testing.T) {
	if got := signalEventID("alpha-team", "p1", "t1", "", "SIG-DECISION", "project_paused", nil, signalIDFixedFingerprint, 0, 1); got != signalIDFixedVector {
		t.Fatalf("id_rule drift: got %s, want %s (contract id_rule fixed vector, cross-checked with Python)", got, signalIDFixedVector)
	}
	// attempt changes the id (retry of a different attempt is a different event).
	if got := signalEventID("alpha-team", "p1", "t1", "", "SIG-DECISION", "project_paused", nil, signalIDFixedFingerprint, 1, 1); got == signalIDFixedVector {
		t.Fatalf("attempt=1 must not collide with attempt=0")
	}
	// audit_seq changes the id: the same logical operation at a later
	// committed revision is a different event instance (v1.2).
	if got := signalEventID("alpha-team", "p1", "t1", "", "SIG-DECISION", "project_paused", nil, signalIDFixedFingerprint, 0, 2); got == signalIDFixedVector {
		t.Fatalf("audit_seq=2 must not collide with audit_seq=1")
	}
}

// P1 regressions (id_rule v1.2): distinct logical events must never share an
// event key, while a retry of the same operation (same committed revision +
// same state bytes) stays idempotent.

// Two projects in the same scope: the same logical operation (pause) must
// not share one event key (the original P1 reproduction).
func TestSignalEventIDTwoProjectsDistinct(t *testing.T) {
	stateP1 := []byte(`{"project_id":"p1","updated_at":"t1"}`)
	a := signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", nil, testSignalFingerprint(stateP1), 0, 1)
	b := signalEventID("alpha-team", "p2", "", "", "SIG-DECISION", "project_paused", nil, testSignalFingerprint([]byte(`{"project_id":"p2","updated_at":"t1"}`)), 0, 1)
	if a == b {
		t.Fatalf("two projects' pause events share an id: %s", a)
	}
	// Even with identical state bytes and revision, project identity alone
	// keeps the ids apart.
	c := signalEventID("alpha-team", "p2", "", "", "SIG-DECISION", "project_paused", nil, testSignalFingerprint(stateP1), 0, 1)
	if a == c {
		t.Fatalf("project identity alone must keep pause ids distinct: %s", a)
	}
}

// Two loop iterations of the same project: distinct iteration identity.
func TestSignalEventIDTwoIterationsDistinct(t *testing.T) {
	fp := testSignalFingerprint([]byte(`{"loop":{"current_iteration":1}}`))
	iter1, iter2 := 1, 2
	a := signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "loop_iteration_finished", &iter1, fp, 0, 1)
	b := signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "loop_iteration_finished", &iter2, fp, 0, 1)
	if a == b {
		t.Fatalf("two loop iterations share an id: %s", a)
	}
	// Retry of the same iteration (same state bytes + revision) re-lands on
	// the same id.
	c := signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "loop_iteration_finished", &iter1, fp, 0, 1)
	if a != c {
		t.Fatalf("retry of the same iteration changed the id: %s vs %s", a, c)
	}
}

// pause/resume/pause on one project: three distinct committed operations
// must yield three distinct ids even when the post-write state bytes are
// byte-identical (same-second pause cycle — the v1.1 collision); a retry of
// one operation (same committed revision + same bytes) keeps its id.
func TestSignalEventIDPauseResumePauseDistinct(t *testing.T) {
	// Both pauses write identical post-write bytes (same wall-clock second,
	// same actor, same reason) — only the committed revision differs.
	pauseState := []byte(`{"status":"paused","updated_at":"t1"}`)
	resumeState := []byte(`{"status":"active","updated_at":"t1"}`)
	pause1 := signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", nil, testSignalFingerprint(pauseState), 0, 1)
	resume := signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "project_resumed", nil, testSignalFingerprint(resumeState), 0, 2)
	pause2 := signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", nil, testSignalFingerprint(pauseState), 0, 3)
	if pause1 == resume || resume == pause2 || pause1 == pause2 {
		t.Fatalf("pause/resume/pause ids must be pairwise distinct: %s %s %s", pause1, resume, pause2)
	}
	// Retry of the second pause (same committed revision + same post-write
	// state) keeps its id.
	pause2Retry := signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", nil, testSignalFingerprint(pauseState), 0, 3)
	if pause2 != pause2Retry {
		t.Fatalf("retry of the second pause changed the id: %s vs %s", pause2, pause2Retry)
	}
}

func TestContractCoversAllEmitters(t *testing.T) {
	c := loadSignalContract(t)
	if c.Version != "1.2" {
		t.Fatalf("contract version=%q, want 1.2", c.Version)
	}
	if len(c.Events) != 14 {
		t.Fatalf("contract has %d events, want 14", len(c.Events))
	}
	levels := map[string]bool{}
	for _, l := range c.Levels {
		levels[l] = true
	}

	// Every contract row is well-formed.
	for i, row := range c.Events {
		if len(row.SourceEvents) == 0 {
			t.Fatalf("row %d: source_events empty", i)
		}
		if !signalEventTypeClosed[row.EventType] {
			t.Fatalf("row %d: event_type %q not in the 7 closed markers", i, row.EventType)
		}
		if !levels[row.Level] {
			t.Fatalf("row %d: level %q not in contract levels", i, row.Level)
		}
		if len(row.Emitters) == 0 {
			t.Fatalf("row %d: emitters empty", i)
		}
		for _, em := range row.Emitters {
			if !strings.HasPrefix(em, "go:") && !strings.HasPrefix(em, "py:") && !strings.HasPrefix(em, "#") {
				t.Fatalf("row %d: emitter %q must start with go:/py:/# (future or #1394-owned)", i, em)
			}
		}
	}

	// Go in-code table <-> contract go:* emitters are a bijection, and each
	// entry's event_type/level/source_event matches its contract row.
	byEmitter := map[string]signalContractRow{}
	for _, row := range c.Events {
		for _, em := range row.Emitters {
			if strings.HasPrefix(em, "go:") {
				byEmitter[em] = row
			}
		}
	}
	if len(byEmitter) != len(signalEmitters) {
		t.Fatalf("contract has %d go:* emitters, code table has %d", len(byEmitter), len(signalEmitters))
	}
	seenSourceEvents := map[string]string{}
	for name, em := range signalEmitters {
		row, ok := byEmitter["go:"+name]
		if !ok {
			t.Fatalf("code emitter %s has no contract row with go:%s", name, name)
		}
		if row.EventType != em.EventType {
			t.Fatalf("emitter %s: event_type=%s, contract row says %s", name, em.EventType, row.EventType)
		}
		if row.Level != em.Level {
			t.Fatalf("emitter %s: level=%s, contract row says %s", name, em.Level, row.Level)
		}
		inRow := false
		for _, se := range row.SourceEvents {
			if se == em.SourceEvent {
				inRow = true
			}
		}
		if !inRow {
			t.Fatalf("emitter %s: source_event %s not in contract row source_events %v", name, em.SourceEvent, row.SourceEvents)
		}
		if !signalOriginClosed[em.Origin] {
			t.Fatalf("emitter %s: origin %q not in the schema origin enum", name, em.Origin)
		}
		// Hash input uniqueness: two Go emitters must never share a source_event.
		if other, dup := seenSourceEvents[em.SourceEvent]; dup {
			t.Fatalf("source_event %s used by both %s and %s (id collision)", em.SourceEvent, other, name)
		}
		seenSourceEvents[em.SourceEvent] = name
	}

	// The two SIG-COMPLETE emit points (Go complete vs Python accept) hash
	// distinct source_events, so their ids cannot collide even at task_id="".
	complete := signalEmitters["CompleteProject"]
	if !strings.Contains(c.IDRule, "sha256(team_id|project_id|task_id|loop_id|event_type|source_event|iteration|fingerprint|attempt|audit_seq)") {
		t.Fatalf("contract id_rule does not document the v1.2 hash input order: %q", c.IDRule)
	}
	_ = complete
}

// readSignalEvent fetches and decodes one per-event object.
func readSignalEvent(t *testing.T, store *ossfake.Memory, key string) map[string]any {
	t.Helper()
	data, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatalf("event %s missing: %v", key, err)
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("event %s decode: %v", key, err)
	}
	return ev
}

// assertSignalEventShape checks the 11-field schema (additionalProperties:
// false) plus the identity fields shared by all emit tests.
func assertSignalEventShape(t *testing.T, ev map[string]any, wantType, wantRef, wantTaskID string) {
	t.Helper()
	wantFields := []string{"event_id", "task_id", "loop_id", "event_type", "is_signal",
		"source_event", "ref", "fingerprint", "iteration_index", "payload", "occurred_at"}
	if len(ev) != len(wantFields) {
		t.Fatalf("event has %d fields, want exactly %d (schema additionalProperties:false): %v", len(ev), len(wantFields), ev)
	}
	for _, k := range wantFields {
		if _, ok := ev[k]; !ok {
			t.Fatalf("event missing field %q: %v", k, ev)
		}
	}
	if ev["event_type"] != wantType {
		t.Fatalf("event_type=%v, want %v", ev["event_type"], wantType)
	}
	if ev["is_signal"] != true {
		t.Fatalf("is_signal=%v, want true (const)", ev["is_signal"])
	}
	if ev["task_id"] != wantTaskID {
		t.Fatalf("task_id=%v, want %q", ev["task_id"], wantTaskID)
	}
	if ev["ref"] != wantRef {
		t.Fatalf("ref=%v, want %q", ev["ref"], wantRef)
	}
	id, _ := ev["event_id"].(string)
	if !strings.HasPrefix(id, "ev_") || len(id) != 3+16 {
		t.Fatalf("event_id=%v, want ev_ + 16 hex", ev["event_id"])
	}
	if ts, _ := ev["occurred_at"].(string); ts == "" {
		t.Fatalf("occurred_at empty")
	}
	if fp, _ := ev["fingerprint"].(string); len(fp) != 64 {
		t.Fatalf("fingerprint=%v, want sha256 hex (64)", ev["fingerprint"])
	}
	src, ok := ev["source_event"].(map[string]any)
	if !ok {
		t.Fatalf("source_event=%v, want object", ev["source_event"])
	}
	if origin, _ := src["origin"].(string); !signalOriginClosed[origin] {
		t.Fatalf("source_event.origin=%v, want a schema enum value", src["origin"])
	}
}

func TestPauseResumeEmits(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", strings.NewReader(`{"reason":"customer review"}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, admin)
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status=%d body=%s", rec.Code, rec.Body.String())
	}
	metaKey := "teams/alpha-team/shared/projects/p1/meta.json"
	// The fingerprint is the sha256 of the stored post-pause meta bytes —
	// capture the id now, before the resume below rewrites the object.
	pauseID := expectedSignalEventID(t, store, metaKey, "alpha-team", "p1", "", "SIG-DECISION", "project_paused", storedAuditSeq(t, store, metaKey))
	if seq := storedAuditSeq(t, store, metaKey); seq != 1 {
		t.Fatalf("first committed operation must consume audit_seq 1, got %d", seq)
	}
	ev := readSignalEvent(t, store, "teams/alpha-team/shared/events/alpha-team/"+pauseID+".json")
	assertSignalEventShape(t, ev, "SIG-DECISION", "teams/alpha-team/shared/projects/p1/meta.json", "")
	if ev["event_id"] != pauseID {
		t.Fatalf("event_id=%v, want deterministic %v", ev["event_id"], pauseID)
	}
	if payload, _ := ev["payload"].(map[string]any); payload["project_id"] != "p1" || payload["reason"] != "customer review" {
		t.Fatalf("payload=%v, want project_id=p1 + reason", ev["payload"])
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/resume", nil)
	req2.SetPathValue("id", "p1")
	req2 = withCaller(req2, admin)
	rec2 := httptest.NewRecorder()
	h.ResumeProject(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	resumeID := expectedSignalEventID(t, store, metaKey, "alpha-team", "p1", "", "SIG-DECISION", "project_resumed", storedAuditSeq(t, store, metaKey))
	if seq := storedAuditSeq(t, store, metaKey); seq != 2 {
		t.Fatalf("second committed operation must consume audit_seq 2, got %d", seq)
	}
	ev2 := readSignalEvent(t, store, "teams/alpha-team/shared/events/alpha-team/"+resumeID+".json")
	assertSignalEventShape(t, ev2, "SIG-DECISION", "teams/alpha-team/shared/projects/p1/meta.json", "")
	if ev2["event_id"] != resumeID {
		t.Fatalf("resume event_id=%v, want %v", ev2["event_id"], resumeID)
	}
}

func TestReplanProjectEmitsDecision(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Old T1", "assigned_to": "dev1", "status": "planned", "depends_on": []string{}},
		},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	body := `{"tasks":[{"taskId":"t1","title":"New T1"},{"taskId":"t2","title":"T2","assignedTo":"dev2","dependsOn":["t1"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(body))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("replan status=%d body=%s", rec.Code, rec.Body.String())
	}
	replanMetaKey := "teams/alpha-team/shared/projects/p1/meta.json"
	replanID := expectedSignalEventID(t, store, replanMetaKey, "alpha-team", "p1", "", "SIG-DECISION", "project_replanned", storedAuditSeq(t, store, replanMetaKey))
	ev := readSignalEvent(t, store, "teams/alpha-team/shared/events/alpha-team/"+replanID+".json")
	assertSignalEventShape(t, ev, "SIG-DECISION", "teams/alpha-team/shared/projects/p1/meta.json", "")
	if ev["event_id"] != replanID {
		t.Fatalf("event_id=%v, want %v", ev["event_id"], replanID)
	}
	if payload, _ := ev["payload"].(map[string]any); payload["task_count"] != float64(2) {
		t.Fatalf("payload=%v, want task_count=2", ev["payload"])
	}
}

func TestCancelTaskEmitsDecision(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "planned", "depends_on": []string{}},
		},
	})
	putTask(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "planned",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel", strings.NewReader(`{"reason":"no longer needed"}`))
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CancelTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	// audit_seq comes from the PROJECT meta (the write that committed the
	// cancel), not from the task ref whose bytes form the fingerprint.
	cancelID := expectedSignalEventID(t, store, "shared/tasks/t1/meta.json", "global", "p1", "t1", "SIG-DECISION", "task_cancelled", storedAuditSeq(t, store, "shared/projects/p1/meta.json"))
	ev := readSignalEvent(t, store, "shared/events/global/"+cancelID+".json")
	assertSignalEventShape(t, ev, "SIG-DECISION", "shared/tasks/t1/meta.json", "t1")
	if ev["event_id"] != cancelID {
		t.Fatalf("event_id=%v, want %v", ev["event_id"], cancelID)
	}
}

func TestCompleteProjectEmitsComplete(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "completed", "depends_on": []string{}},
		},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/complete", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CompleteProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", rec.Code, rec.Body.String())
	}
	completeMetaKey := "teams/alpha-team/shared/projects/p1/meta.json"
	completeID := expectedSignalEventID(t, store, completeMetaKey, "alpha-team", "p1", "", "SIG-COMPLETE", "project_completed", storedAuditSeq(t, store, completeMetaKey))
	ev := readSignalEvent(t, store, "teams/alpha-team/shared/events/alpha-team/"+completeID+".json")
	assertSignalEventShape(t, ev, "SIG-COMPLETE", "teams/alpha-team/shared/projects/p1/meta.json", "")
	if ev["event_id"] != completeID {
		t.Fatalf("event_id=%v, want %v", ev["event_id"], completeID)
	}
}

// TestSignalEventIdempotent: an emit-level retry of the same committed
// operation (same stored post-write bytes → same fingerprint, same identity
// fields, same audit_seq) lands on the same key and must not modify the
// stored object — even when the retry carries a different payload or
// occurred_at. A *different* committed operation (advanced audit_seq — with
// or without any state-byte change) must produce a different key and leave
// the original object untouched.
func TestSignalEventIdempotent(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	ctx := context.Background()
	em := signalEmitters["PauseProject"]
	state := []byte(`{"status":"paused"}`)
	key := "teams/alpha-team/shared/events/alpha-team/" +
		signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", nil, testSignalFingerprint(state), 0, 1) + ".json"

	h.emitSignalEvent(ctx, em, "alpha-team", "alpha-team", "p1", "", nil,
		"teams/alpha-team/shared/projects/p1/meta.json", state,
		map[string]any{"project_id": "p1"}, 1)
	data1, err := store.GetObject(ctx, key)
	if err != nil {
		t.Fatalf("first emit missing: %v", err)
	}

	// Emit-level retry: same committed revision (1) + same post-write bytes
	// with a different payload: must be a no-op (original kept).
	h.emitSignalEvent(ctx, em, "alpha-team", "alpha-team", "p1", "", nil,
		"teams/alpha-team/shared/projects/p1/meta.json", state,
		map[string]any{"project_id": "p1", "note": "retry"}, 1)
	data2, err := store.GetObject(ctx, key)
	if err != nil {
		t.Fatalf("object vanished after re-emit: %v", err)
	}
	if !bytes.Equal(data1, data2) {
		t.Fatalf("re-emit modified the stored event: %s vs %s", data1, data2)
	}

	// A later committed operation (a second pause after a resume). Even if
	// the post-write bytes are byte-identical (same-second cycle), the
	// advanced committed revision (2) must yield a different key, and the
	// first object must remain untouched.
	state2 := state // identical bytes: the v1.1 collision window
	key2 := "teams/alpha-team/shared/events/alpha-team/" +
		signalEventID("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", nil, testSignalFingerprint(state2), 0, 2) + ".json"
	if key2 == key {
		t.Fatalf("second operation must use a different key, got the same: %s", key)
	}
	h.emitSignalEvent(ctx, em, "alpha-team", "alpha-team", "p1", "", nil,
		"teams/alpha-team/shared/projects/p1/meta.json", state2,
		map[string]any{"project_id": "p1"}, 2)
	if _, err := store.GetObject(ctx, key2); err != nil {
		t.Fatalf("second operation event missing at %s: %v", key2, err)
	}
	data3, err := store.GetObject(ctx, key)
	if err != nil {
		t.Fatalf("original event vanished after second operation: %v", err)
	}
	if !bytes.Equal(data1, data3) {
		t.Fatalf("second operation modified the first event: %s vs %s", data1, data3)
	}
}

// TestPauseResumePauseSameSecondRealHandlers is the maintainer's P1
// regression on the real public handlers: pause → resume → pause on the same
// project within one wall-clock second, same actor, same reason. All three
// requests return 200 and id_rule v1.1 only landed two event objects — the
// second pause wrote byte-identical post-write state (second-precision
// updated_at, same actor, same reason) → same fingerprint → same event id →
// deduped. Under v1.2 the identity is the committed audit_seq, never the
// clock: all three operations must land three distinct events.
func TestPauseResumePauseSameSecondRealHandlers(t *testing.T) {
	store := ossfake.NewMemory()
	metaKey := "teams/alpha-team/shared/projects/p1/meta.json"
	eventsPrefix := "teams/alpha-team/shared/events/alpha-team/"

	// Pin the clock to one fixed second: every operation stamps the same
	// RFC3339 second (the v1.1 collision window).
	fixed := time.Date(2026, 10, 22, 8, 30, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixed }
	t.Cleanup(func() { nowFunc = oldNow })

	putProject(store, metaKey, map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	readMeta := func() map[string]any {
		data, err := store.GetObject(context.Background(), metaKey)
		if err != nil {
			t.Fatalf("read project meta: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("decode project meta: %v", err)
		}
		return m
	}
	do := func(op string, body string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/"+op, strings.NewReader(body))
		req.SetPathValue("id", "p1")
		req = withCaller(req, admin)
		rec := httptest.NewRecorder()
		switch op {
		case "pause":
			h.PauseProject(rec, req)
		case "resume":
			h.ResumeProject(rec, req)
		default:
			t.Fatalf("unknown op %q", op)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", op, rec.Code, rec.Body.String())
		}
	}

	// 1) pause — committed revision 1.
	do("pause", `{"reason":"customer review"}`)
	pause1Meta := readMeta()
	pause1ID := expectedSignalEventID(t, store, metaKey, "alpha-team", "p1", "", "SIG-DECISION", "project_paused", storedAuditSeq(t, store, metaKey))
	if got := storedAuditSeq(t, store, metaKey); got != 1 {
		t.Fatalf("pause #1 must commit audit_seq 1, got %d", got)
	}
	ev1 := readSignalEvent(t, store, eventsPrefix+pause1ID+".json")
	assertSignalEventShape(t, ev1, "SIG-DECISION", metaKey, "")

	// 2) resume — committed revision 2.
	do("resume", "")
	resumeID := expectedSignalEventID(t, store, metaKey, "alpha-team", "p1", "", "SIG-DECISION", "project_resumed", storedAuditSeq(t, store, metaKey))
	if got := storedAuditSeq(t, store, metaKey); got != 2 {
		t.Fatalf("resume must commit audit_seq 2, got %d", got)
	}
	ev2 := readSignalEvent(t, store, eventsPrefix+resumeID+".json")
	assertSignalEventShape(t, ev2, "SIG-DECISION", metaKey, "")

	// 3) pause again, same actor + same reason, same second — committed
	// revision 3. v1.1 deduped this one.
	do("pause", `{"reason":"customer review"}`)
	pause2Meta := readMeta()
	pause2ID := expectedSignalEventID(t, store, metaKey, "alpha-team", "p1", "", "SIG-DECISION", "project_paused", storedAuditSeq(t, store, metaKey))
	if got := storedAuditSeq(t, store, metaKey); got != 3 {
		t.Fatalf("pause #2 must commit audit_seq 3, got %d", got)
	}
	ev3 := readSignalEvent(t, store, eventsPrefix+pause2ID+".json")
	assertSignalEventShape(t, ev3, "SIG-DECISION", metaKey, "")

	// All three events exist and are pairwise distinct.
	if ev1["event_id"] != pause1ID || ev2["event_id"] != resumeID || ev3["event_id"] != pause2ID {
		t.Fatalf("event ids %v %v %v, want %v %v %v", ev1["event_id"], ev2["event_id"], ev3["event_id"], pause1ID, resumeID, pause2ID)
	}
	if pause1ID == resumeID || resumeID == pause2ID || pause1ID == pause2ID {
		t.Fatalf("pause/resume/pause events must be pairwise distinct: %s %s %s", pause1ID, resumeID, pause2ID)
	}

	// The two pause cycles wrote the same audited state in the same
	// wall-clock second (proving the test covers the v1.1 collision window):
	// identical status / updated_at / updated_by / pause_reason.
	if pause1Meta["status"] != "paused" || pause2Meta["status"] != "paused" {
		t.Fatalf("both pauses must leave the project paused: %v / %v", pause1Meta["status"], pause2Meta["status"])
	}
	if pause1Meta["updated_at"] != pause2Meta["updated_at"] {
		t.Fatalf("same-second premise broken: pause #1 updated_at=%v, pause #2=%v", pause1Meta["updated_at"], pause2Meta["updated_at"])
	}
	if pause1Meta["updated_at"] != fixed.Format(time.RFC3339) {
		t.Fatalf("clock pin not in effect: updated_at=%v, want %v", pause1Meta["updated_at"], fixed.Format(time.RFC3339))
	}
	if pause1Meta["updated_by"] != pause2Meta["updated_by"] || pause1Meta["updated_by"] != "admin" {
		t.Fatalf("same-actor premise broken: %v / %v", pause1Meta["updated_by"], pause2Meta["updated_by"])
	}
	if pause1Meta["pause_reason"] != pause2Meta["pause_reason"] || pause1Meta["pause_reason"] != "customer review" {
		t.Fatalf("same-reason premise broken: %v / %v", pause1Meta["pause_reason"], pause2Meta["pause_reason"])
	}

	// Exactly three event objects under the events root.
	keys, err := store.ListObjects(context.Background(), eventsPrefix)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("events root has %d objects, want 3: %v", len(keys), keys)
	}

	// A 4th pause is rejected (already paused) and must not add an event.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", strings.NewReader(`{"reason":"customer review"}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, admin)
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("4th pause status=%d, want 409", rec.Code)
	}
	keys, err = store.ListObjects(context.Background(), eventsPrefix)
	if err != nil {
		t.Fatalf("list events after 4th pause: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("rejected 4th pause changed the events root: %d objects, want 3: %v", len(keys), keys)
	}
}
