package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// auditRig builds an AuditHandler over a seeded in-memory store.
type auditRig struct {
	handler *AuditHandler
	sc      *ossfake.Memory
}

func newAuditRig(t *testing.T) *auditRig {
	t.Helper()
	sc := ossfake.NewMemory()
	d := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	lines := []string{
		auditLine(t, audit.Event{Who: "admin", Role: "admin", When: d, Target: "w1", TargetTeam: "alpha-team", Action: "capability_grant", Capability: "approval_policy"}),
		auditLine(t, audit.Event{Who: "admin", Role: "admin", When: d.Add(time.Minute), Target: "w2", TargetTeam: "beta-team", Action: "approval_level_change", Detail: "ask -> off"}),
		auditLine(t, audit.Event{Who: "admin", Role: "admin", When: d.Add(2 * time.Minute), Target: "w1", TargetTeam: "alpha-team", Action: "channel_credential_write", Detail: "field: matrix.accessToken"}),
	}
	if err := sc.PutObject(context.Background(), "audit/2026-09-14.jsonl", []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		t.Fatal(err)
	}
	return &auditRig{handler: NewAuditHandler(sc), sc: sc}
}

func auditLine(t *testing.T, ev audit.Event) string {
	t.Helper()
	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func (r *auditRig) do(t *testing.T, query string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, nil)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
	rec := httptest.NewRecorder()
	r.handler.List(rec, req)
	return rec
}

func auditAdminCaller() *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
}

func TestAuditAdminUnscopedSeesEverything(t *testing.T) {
	r := newAuditRig(t)
	rec := r.do(t, "", auditAdminCaller())
	if rec.Code != http.StatusOK {
		t.Fatalf("admin unscoped: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body auditListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 3 {
		t.Fatalf("want 3 events, got %d", len(body.Events))
	}
	// Newest first.
	if body.Events[0].Action != "channel_credential_write" || body.Events[2].Action != "capability_grant" {
		t.Fatalf("order wrong: %s ... %s", body.Events[0].Action, body.Events[2].Action)
	}
	// Derived kind + contract fields.
	if body.Events[0].Kind != "channel" || body.Events[0].Actor != "admin" || body.Events[0].Target != "w1" {
		t.Fatalf("projection wrong: %+v", body.Events[0])
	}
}

func TestAuditAdminTeamFilter(t *testing.T) {
	r := newAuditRig(t)
	rec := r.do(t, "?team=alpha-team", auditAdminCaller())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body auditListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 2 {
		t.Fatalf("team=alpha-team: want 2, got %d", len(body.Events))
	}
	for _, ev := range body.Events {
		if ev.TargetTeam != "alpha-team" {
			t.Fatalf("team filter leaked %s", ev.TargetTeam)
		}
	}
}

func TestAuditScopedReaderRequiresTeam(t *testing.T) {
	r := newAuditRig(t)
	human := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}
	rec := r.do(t, "", human)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("human unscoped: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "team scope required") {
		t.Fatalf("400 must be self-explanatory: %s", rec.Body.String())
	}
}

func TestAuditScopedReaderOwnTeam(t *testing.T) {
	r := newAuditRig(t)
	human := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}
	rec := r.do(t, "?team=alpha-team", human)
	if rec.Code != http.StatusOK {
		t.Fatalf("human own team: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body auditListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 2 {
		t.Fatalf("want 2 events in own team, got %d", len(body.Events))
	}
}

func TestAuditScopedReaderCrossTeamIs404(t *testing.T) {
	r := newAuditRig(t)
	human := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}
	rec := r.do(t, "?team=beta-team", human)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team: want 404 (W8), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuditTeamLeaderOwnTeam(t *testing.T) {
	r := newAuditRig(t)
	leader := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}
	rec := r.do(t, "?team=alpha-team", leader)
	if rec.Code != http.StatusOK {
		t.Fatalf("leader own team: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Unscoped: also 400 for leaders.
	rec = r.do(t, "", leader)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("leader unscoped: want 400, got %d", rec.Code)
	}
}

func TestAuditManagerUnscoped(t *testing.T) {
	r := newAuditRig(t)
	manager := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}
	rec := r.do(t, "", manager)
	if rec.Code != http.StatusOK {
		t.Fatalf("manager unscoped: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuditParameterValidation(t *testing.T) {
	bad := base64.RawURLEncoding.EncodeToString([]byte(`{"date":"2026-09-14","seq":1,"ts":"bad"}`))
	badDate := base64.RawURLEncoding.EncodeToString([]byte(`{"date":"bad","seq":1,"ts":"2026-09-14T10:00:00Z"}`))
	r := newAuditRig(t)
	for _, query := range []string{
		"?limit=0",
		"?limit=201",
		"?limit=abc",
		"?from=not-a-time",
		"?to=not-a-time",
		"?cursor=!!!not-base64!!!",
		"?cursor=YWJj",       // valid base64, not JSON
		"?cursor=" + bad,     // well-formed JSON, unparseable ts
		"?cursor=" + badDate, // well-formed JSON, unparseable date
	} {
		rec := r.do(t, query, auditAdminCaller())
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: want 400, got %d: %s", query, rec.Code, rec.Body.String())
		}
	}
}

// TestAuditFromAfterToIs400 pins the range error mapping. The same-day case
// compares the original instants: the day truncation must not hide the
// within-day ordering (reviewer repro: from=11:00Z&to=10:00Z on one day).
func TestAuditFromAfterToIs400(t *testing.T) {
	r := newAuditRig(t)
	for _, query := range []string{
		"?from=2026-09-15T00:00:00Z&to=2026-09-14T00:00:00Z", // cross-day
		"?from=2026-09-14T11:00:00Z&to=2026-09-14T10:00:00Z", // same-day ordering
	} {
		rec := r.do(t, query, auditAdminCaller())
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: want 400, got %d: %s", query, rec.Code, rec.Body.String())
		}
	}
}

func TestAuditStorageFailureIs502(t *testing.T) {
	sc := &failingAuditStorage{Memory: ossfake.NewMemory()}
	handler := NewAuditHandler(sc)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), auditAdminCaller()))
	rec := httptest.NewRecorder()
	handler.List(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("storage failure: want 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "audit store unavailable") {
		t.Fatalf("502 must use the error envelope: %s", rec.Body.String())
	}
}

func TestAuditMalformedObjectIs502(t *testing.T) {
	sc := ossfake.NewMemory()
	if err := sc.PutObject(context.Background(), "audit/2026-09-14.jsonl", []byte("{broken\n")); err != nil {
		t.Fatal(err)
	}
	handler := NewAuditHandler(sc)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), auditAdminCaller()))
	rec := httptest.NewRecorder()
	handler.List(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("malformed object: want 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuditPaginationRoundTrip(t *testing.T) {
	r := newAuditRig(t)
	rec := r.do(t, "?limit=2", auditAdminCaller())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body auditListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 2 || body.Cursor == "" {
		t.Fatalf("page 1: want 2 events + cursor, got %d, cursor=%q", len(body.Events), body.Cursor)
	}
	// Follow the cursor: the remaining single event, no further cursor.
	rec = r.do(t, "?limit=2&cursor="+body.Cursor, auditAdminCaller())
	if rec.Code != http.StatusOK {
		t.Fatalf("page 2: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body2 auditListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	if len(body2.Events) != 1 || body2.Cursor != "" {
		t.Fatalf("page 2: want 1 event, no cursor; got %d, cursor=%q", len(body2.Events), body2.Cursor)
	}
	// No overlap between pages.
	if body2.Events[0].Ts == body.Events[1].Ts && body2.Events[0].Action == body.Events[1].Action && body2.Events[0].Detail == body.Events[1].Detail {
		t.Fatalf("cursor must not repeat the previous page's last event: %+v", body2.Events[0])
	}
}

// failingAuditStorage fails every get: the unbounded path lists the prefix
// (empty here) and then finds nothing — so fail at ListObjects to force the
// storage error on the enumeration path as well.
type failingAuditStorage struct {
	*ossfake.Memory
}

func (f *failingAuditStorage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	return nil, errors.New("simulated storage outage")
}
