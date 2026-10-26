package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func putSignalEvent(t *testing.T, store *ossfake.Memory, key string, ev signalEvent) {
	t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal signal event: %v", err)
	}
	if err := store.PutObject(context.Background(), key, data); err != nil {
		t.Fatalf("put signal event: %v", err)
	}
}

func signalTrackerEvent(eventID, taskID, ref, eventType, occurred string) signalEvent {
	return signalEvent{
		EventID:     eventID,
		TaskID:      taskID,
		EventType:   eventType,
		IsSignal:    true,
		SourceEvent: signalSource{Origin: "decision_lock"},
		Ref:         ref,
		Fingerprint: "fp-" + eventID,
		Payload:     map[string]any{"from_status": "active", "to_status": "paused", "action": "pause_task"},
		OccurredAt:  occurred,
	}
}

func getSignalEvents(t *testing.T, h *ProjectHandler, projectID, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/projects/"+projectID+"/signal-events"+query,
		nil,
	)
	req.SetPathValue("id", projectID)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectSignalEvents(rec, req)
	return rec
}

func decodeSignalEvents(t *testing.T, rec *httptest.ResponseRecorder) signalEventsResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp signalEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func seedSignalProject(t *testing.T) (*ossfake.Memory, *ProjectHandler) {
	t.Helper()
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active",
		"plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "planned",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t2/meta.json", map[string]any{
		"task_id": "t2", "project_id": "p2", "status": "planned",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))
	return store, h
}

func TestGetProjectSignalEvents_FiltersByProject(t *testing.T) {
	store, h := seedSignalProject(t)
	dir := "teams/alpha-team/shared/events/alpha-team/"
	putSignalEvent(t, store, dir+"ev_p1.json", signalTrackerEvent(
		"ev_p1", "", "teams/alpha-team/shared/projects/p1/meta.json",
		"SIG-DECISION", "2026-10-24T10:00:00Z",
	))
	putSignalEvent(t, store, dir+"ev_t1.json", signalTrackerEvent(
		"ev_t1", "t1", "teams/alpha-team/shared/tasks/t1/meta.json",
		"SIG-VERDICT", "2026-10-24T10:01:00Z",
	))
	// Foreign project and foreign task: must not leak into p1.
	putSignalEvent(t, store, dir+"ev_p2.json", signalTrackerEvent(
		"ev_p2", "", "teams/alpha-team/shared/projects/p2/meta.json",
		"SIG-DECISION", "2026-10-24T10:02:00Z",
	))
	putSignalEvent(t, store, dir+"ev_t2.json", signalTrackerEvent(
		"ev_t2", "t2", "teams/alpha-team/shared/tasks/t2/meta.json",
		"SIG-VERDICT", "2026-10-24T10:03:00Z",
	))

	resp := decodeSignalEvents(t, getSignalEvents(t, h, "p1", ""))
	if resp.Count != 2 {
		t.Fatalf("count=%d want 2 (project-level + own task): %+v", resp.Count, resp.Events)
	}
	if resp.Events[0].EventID != "ev_p1" || resp.Events[1].EventID != "ev_t1" {
		t.Fatalf("unexpected order/ids: %+v", resp.Events)
	}
}

func TestGetProjectSignalEvents_Pagination(t *testing.T) {
	store, h := seedSignalProject(t)
	dir := "teams/alpha-team/shared/events/alpha-team/"
	for i, at := range []string{
		"2026-10-24T10:00:00Z", "2026-10-24T10:01:00Z", "2026-10-24T10:02:00Z",
	} {
		id := "ev_" + string(rune('a'+i))
		putSignalEvent(t, store, dir+id+".json", signalTrackerEvent(
			id, "", "teams/alpha-team/shared/projects/p1/meta.json",
			"SIG-COMPLETE", at,
		))
	}
	first := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?limit=2"))
	if first.Count != 2 || first.NextCursor == "" {
		t.Fatalf("first page count=%d cursor=%q", first.Count, first.NextCursor)
	}
	second := decodeSignalEvents(t, getSignalEvents(
		t, h, "p1", "?limit=2&cursor="+first.NextCursor,
	))
	if second.Count != 1 {
		t.Fatalf("second page count=%d want 1", second.Count)
	}
	if second.Events[0].EventID == first.Events[0].EventID ||
		second.Events[0].EventID == first.Events[1].EventID {
		t.Fatalf("second page repeated an event: %+v", second.Events)
	}
}

func TestGetProjectSignalEvents_TypeAndSinceFilters(t *testing.T) {
	store, h := seedSignalProject(t)
	dir := "teams/alpha-team/shared/events/alpha-team/"
	putSignalEvent(t, store, dir+"ev_a.json", signalTrackerEvent(
		"ev_a", "", "teams/alpha-team/shared/projects/p1/meta.json",
		"SIG-DECISION", "2026-10-24T10:00:00Z",
	))
	putSignalEvent(t, store, dir+"ev_b.json", signalTrackerEvent(
		"ev_b", "", "teams/alpha-team/shared/projects/p1/meta.json",
		"SIG-VERDICT", "2026-10-24T10:05:00Z",
	))

	byType := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?type=SIG-VERDICT"))
	if byType.Count != 1 || byType.Events[0].EventID != "ev_b" {
		t.Fatalf("type filter: %+v", byType.Events)
	}
	bySince := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?since=2026-10-24T10:01:00Z"))
	if bySince.Count != 1 || bySince.Events[0].EventID != "ev_b" {
		t.Fatalf("since filter: %+v", bySince.Events)
	}
}

func TestGetProjectSignalEvents_UnknownProject404(t *testing.T) {
	_, h := seedSignalProject(t)
	rec := getSignalEvents(t, h, "p-missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetProjectSignalEvents_SinceInstantSemantics pins the P2 regression:
// since is an RFC3339 instant lower bound (inclusive, >= since), not a
// lexicographic string filter.
func TestGetProjectSignalEvents_SinceInstantSemantics(t *testing.T) {
	store, h := seedSignalProject(t)
	dir := "teams/alpha-team/shared/events/alpha-team/"
	ref := "teams/alpha-team/shared/projects/p1/meta.json"
	putSignalEvent(t, store, dir+"ev_1000.json", signalTrackerEvent(
		"ev_1000", "", ref, "SIG-DECISION", "2026-10-24T10:00:00Z",
	))
	putSignalEvent(t, store, dir+"ev_1002.json", signalTrackerEvent(
		"ev_1002", "", ref, "SIG-VERDICT", "2026-10-24T10:02:00Z",
	))

	t.Run("OffsetZoneSince", func(t *testing.T) {
		// 18:01:00+08:00 is 10:01:00Z. String comparison would filter
		// both events ("10:0…" < "18:0…"), returning 0.
		resp := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?since=2026-10-24T18:01:00+08:00"))
		if resp.Count != 1 || resp.Events[0].EventID != "ev_1002" {
			t.Fatalf("offset-zone since: count=%d want 1 (ev_1002): %+v", resp.Count, resp.Events)
		}
	})
	t.Run("FractionalSecondSince", func(t *testing.T) {
		// 10:00:00Z is strictly before 10:00:00.500Z; string comparison
		// would keep it ("…00Z" > "…00.500Z"), returning 2.
		resp := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?since=2026-10-24T10:00:00.500Z"))
		if resp.Count != 1 || resp.Events[0].EventID != "ev_1002" {
			t.Fatalf("fractional since: count=%d want 1 (ev_1002): %+v", resp.Count, resp.Events)
		}
	})
	t.Run("InvalidSince400", func(t *testing.T) {
		rec := getSignalEvents(t, h, "p1", "?since=not-a-time")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestSignalCursor_PaddedUnpaddedInterop pins the P2 regression: cursors
// must survive the Go<->Python boundary — Go emits unpadded base64url
// (RawURLEncoding), Python emits padded (base64.urlsafe_b64encode).
func TestSignalCursor_PaddedUnpaddedInterop(t *testing.T) {
	payload := []byte("2026-10-24T10:00:00Z\x00ev_a")

	goCursor := encodeSignalCursor("2026-10-24T10:00:00Z", "ev_a")
	if strings.ContainsAny(goCursor, "=") {
		t.Fatalf("encodeSignalCursor must stay unpadded, got %q", goCursor)
	}
	if goCursor != base64.RawURLEncoding.EncodeToString(payload) {
		t.Fatalf("go cursor %q is not raw base64url of the payload", goCursor)
	}
	// What Python base64.urlsafe_b64encode(b"2026-10-24T10:00:00Z\x00ev_a")
	// produces: same alphabet, with '=' padding.
	pyCursor := base64.URLEncoding.EncodeToString(payload)
	if !strings.HasSuffix(pyCursor, "=") {
		t.Fatalf("padded fixture lost its padding: %q", pyCursor)
	}

	for name, cur := range map[string]string{"goUnpadded": goCursor, "pythonPadded": pyCursor} {
		ts, id, ok := decodeSignalCursor(cur)
		if !ok || ts != "2026-10-24T10:00:00Z" || id != "ev_a" {
			t.Fatalf("%s cursor %q: got (%q, %q, %v) want (2026-10-24T10:00:00Z, ev_a, true)",
				name, cur, ts, id, ok)
		}
	}
}

// TestGetProjectSignalEvents_PaddedCursorPagination: a cursor in Python
// padded form must page through the Go endpoint without a 400.
func TestGetProjectSignalEvents_PaddedCursorPagination(t *testing.T) {
	store, h := seedSignalProject(t)
	dir := "teams/alpha-team/shared/events/alpha-team/"
	for i, at := range []string{
		"2026-10-24T10:00:00Z", "2026-10-24T10:01:00Z", "2026-10-24T10:02:00Z",
	} {
		id := "ev_" + string(rune('a'+i))
		putSignalEvent(t, store, dir+id+".json", signalTrackerEvent(
			id, "", "teams/alpha-team/shared/projects/p1/meta.json",
			"SIG-COMPLETE", at,
		))
	}
	first := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?limit=2"))
	if first.Count != 2 || first.NextCursor == "" {
		t.Fatalf("first page count=%d cursor=%q", first.Count, first.NextCursor)
	}
	// Re-pad the Go cursor the way Python's urlsafe_b64encode would.
	padded := first.NextCursor + strings.Repeat("=", (4-len(first.NextCursor)%4)%4)
	second := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?limit=2&cursor="+padded))
	if second.Count != 1 || second.Events[0].EventID != "ev_c" {
		t.Fatalf("second page count=%d want 1 (ev_c): %+v", second.Count, second.Events)
	}
}

var normalizedOccurredAtPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

// TestSignalEventOccurredAtNormalized guards the invariant the read
// endpoint's lexicographic sort and cursor-skip rely on: both emitters
// write occurred_at as fixed-width UTC 'Z' second-precision RFC3339 —
// the controller via utcTimestamp, TeamHarness via _utc_timestamp
// (tz-aware UTC isoformat() with "+00:00" replaced by "Z") — for which
// string order equals chronological order.
func TestSignalEventOccurredAtNormalized(t *testing.T) {
	if got := utcTimestamp(); !normalizedOccurredAtPattern.MatchString(got) {
		t.Fatalf("utcTimestamp() = %q; emitter format drifted off normalized UTC 'Z' RFC3339", got)
	}
	ats := []string{
		"2026-10-24T10:00:00Z",
		"2026-10-24T09:59:59Z",
		"2026-10-25T00:00:00Z",
		"2026-10-24T10:00:01Z",
		"2026-10-23T23:59:59Z",
	}
	sorted := append([]string(nil), ats...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	times := make([]time.Time, len(sorted))
	for i, at := range sorted {
		var err error
		times[i], err = time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatalf("test data %q is not RFC3339: %v", at, err)
		}
	}
	for i := 1; i < len(times); i++ {
		if times[i-1].After(times[i]) {
			t.Fatalf("string order %v disagrees with chronological order", sorted)
		}
	}
}
