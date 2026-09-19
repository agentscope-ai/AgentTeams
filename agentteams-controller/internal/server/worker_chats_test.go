package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestChatsHandler builds a handler with a fake K8s client (given
// teams and workers) whose worker URLs point at the given httptest server
// (mimicking one worker's qwenpaw app).
func newTestChatsHandler(t *testing.T, kubeMode string, ts *httptest.Server, objs ...runtime.Object) *ChatsHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewChatsHandler(k8s, "default", kubeMode, "agentteams-worker-")
	if ts != nil {
		h.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	}
	return h
}

func chatsListRequest(name, query string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/placeholder/chats"+query, nil)
	req.SetPathValue("name", name)
	return req
}

func chatsDetailRequest(name, chatID, query string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/placeholder/chats/"+chatID+query, nil)
	req.SetPathValue("name", name)
	req.SetPathValue("chat_id", chatID)
	return req
}

func chatsStatusRequest(name, chatID, query string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/placeholder/chats/"+chatID+"/status"+query, nil)
	req.SetPathValue("name", name)
	req.SetPathValue("chat_id", chatID)
	return req
}

func TestChatList_ForwardsVerbatim(t *testing.T) {
	const payload = `[{"id":"c1","name":"ch1","user_id":"alice","channel":"qq"},{"id":"c2","name":"ch2","user_id":"bob","channel":"matrix"}]`
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.listChats(rec, adminCaller(chatsListRequest("daily-carol", "?user_id=alice&channel=qq")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/chats" {
		t.Fatalf("upstream path=%q, want /api/chats", gotPath)
	}
	if gotQuery != "channel=qq&user_id=alice" {
		t.Fatalf("upstream query=%q, want channel=qq&user_id=alice", gotQuery)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), payload)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q, want application/json", ct)
	}
}

func TestChatList_AllWhitelistedParamsForwarded(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.listChats(rec, adminCaller(chatsListRequest("daily-carol", "?user_id=u1&channel=qq&archived=true&include_app_owned=false")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := "archived=true&channel=qq&include_app_owned=false&user_id=u1"
	if gotQuery != want {
		t.Fatalf("upstream query=%q, want %q", gotQuery, want)
	}
}

func TestChatList_RejectsUnknownQuery(t *testing.T) {
	h := newTestChatsHandler(t, "embedded", nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.listChats(rec, adminCaller(chatsListRequest("daily-carol", "?limit=10")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for unknown query parameter", rec.Code)
	}
}

func TestChatDetail_ForwardsVerbatim(t *testing.T) {
	const payload = `{"messages":[{"id":"m1","type":"message","role":"user","content":[{"type":"text","text":"hi"}],"status":"completed"}],"status":"idle"}`
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChat(rec, adminCaller(chatsDetailRequest("daily-carol", "0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f", "")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := "/api/chats/0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f"
	if gotPath != want {
		t.Fatalf("upstream path=%q, want %s", gotPath, want)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), payload)
	}
}

func TestChatStatus_Forwards(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChatStatus(rec, adminCaller(chatsStatusRequest("daily-carol", "0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f", "")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := "/api/chats/0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f/status"
	if gotPath != want {
		t.Fatalf("upstream path=%q, want %s", gotPath, want)
	}
	if rec.Body.String() != `{"status":"running"}` {
		t.Fatalf("body=%s, want verbatim status payload", rec.Body.String())
	}
}

// TestChatStatus_Upstream404IsTheVersionGate locks the version-agnostic
// behavior: GET /chats/{id}/status exists only on QwenPaw 2.2.1+; on older
// worker builds the upstream's own 404 ("Not Found") is returned verbatim
// so the dashboard can hide the status indicator instead of erroring.
func TestChatStatus_Upstream404IsTheVersionGate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChatStatus(rec, adminCaller(chatsStatusRequest("daily-carol", "0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f", "")))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 passed through from an older runtime", rec.Code)
	}
	if rec.Body.String() != `{"detail":"Not Found"}` {
		t.Fatalf("body=%s, want upstream 404 detail verbatim", rec.Body.String())
	}
}

func TestChatDetail_Upstream404ChatNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Chat not found: 0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f"}`))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChat(rec, adminCaller(chatsDetailRequest("daily-carol", "0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f", "")))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 passed through", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Chat not found") {
		t.Fatalf("body=%s, want upstream 404 detail verbatim", rec.Body.String())
	}
}

func TestChatDetail_RejectsQuery(t *testing.T) {
	h := newTestChatsHandler(t, "embedded", nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	for _, query := range []string{"?limit=10", "?user_id=alice"} {
		rec := httptest.NewRecorder()
		h.getChat(rec, adminCaller(chatsDetailRequest("daily-carol", "0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f", query)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d, want 400", query, rec.Code)
		}
	}
	for _, query := range []string{"?limit=10"} {
		rec := httptest.NewRecorder()
		h.getChatStatus(rec, adminCaller(chatsStatusRequest("daily-carol", "0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f", query)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status route query=%q status=%d, want 400", query, rec.Code)
		}
	}
}

func TestChat_RejectsInvalidChatID(t *testing.T) {
	h := newTestChatsHandler(t, "embedded", nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	for _, chatID := range []string{"", "ABC-123", "a/b", "a.b", "id?", "a_b_"} {
		rec := httptest.NewRecorder()
		h.getChat(rec, adminCaller(chatsDetailRequest("daily-carol", chatID, "")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("chat_id=%q status=%d, want 400", chatID, rec.Code)
		}
	}
}

func TestChat_UpstreamErrorBodyBounded(t *testing.T) {
	big := strings.Repeat("x", 8192)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(big))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.listChats(rec, adminCaller(chatsListRequest("daily-carol", "")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if len(rec.Body.String()) > 512 {
		t.Fatalf("502 body not bounded: %d bytes", len(rec.Body.String()))
	}
}

func TestChat_UnreachableWorker(t *testing.T) {
	// A server that immediately closes — the client gets a connection error.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := upstream.URL
	upstream.Close()

	h := newTestChatsHandler(t, "embedded", nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	h.workerBaseURL = func(string, map[string]string) string { return url }

	rec := httptest.NewRecorder()
	h.listChats(rec, adminCaller(chatsListRequest("daily-carol", "")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for unreachable worker", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unreachable") {
		t.Fatalf("body=%s, want unreachable message", rec.Body.String())
	}
}

func TestChat_KubeModeUnsupported(t *testing.T) {
	h := newTestChatsHandler(t, "k8s", nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.listChats(rec, adminCaller(chatsListRequest("daily-carol", "")))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
}

func TestChat_UnknownWorker(t *testing.T) {
	h := newTestChatsHandler(t, "embedded", nil)
	rec := httptest.NewRecorder()
	h.listChats(rec, adminCaller(chatsListRequest("ghost-worker", "")))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for unknown worker", rec.Code)
	}
}

func TestChat_RejectsInvalidWorkerName(t *testing.T) {
	h := newTestChatsHandler(t, "embedded", nil)
	for _, name := range []string{"", "Daily-Carol", "../etc", "a b"} {
		rec := httptest.NewRecorder()
		h.listChats(rec, adminCaller(chatsListRequest(name, "")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name=%q status=%d, want 400", name, rec.Code)
		}
	}
}

func TestChat_TeamLeaderCrossTeamDenied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("beta-team", "daily-carol")...)
	req := chatsListRequest("daily-carol", "")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.listChats(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team access (W8)", rec.Code)
	}
}

// l2Caller builds an L2 human request (in scope for market-team) carrying
// the given Matrix bearer token; token "" exercises the no-token case.
func l2Caller(req *http.Request, token string) *http.Request {
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// roomsStub wires a room-membership source into the handler and returns a
// pointer that records the token the source was called with (the L2 anchor
// must be the caller's OWN token — never a substitute identity).
func roomsStub(rooms []string, err error) (func(context.Context, string) ([]string, error), *string) {
	tokenSeen := new(string)
	return func(_ context.Context, token string) ([]string, error) {
		*tokenSeen = token
		return rooms, err
	}, tokenSeen
}

// roomListPayload is one worker's full chat list, covering every
// participation class: a matrix chat in !team:ex — the caller's room, an
// AGENT-DRIVEN session (chat user_id is the manager's MXID, not the
// caller's; this is the isolated share_session_in_group mode, the
// AgentTeams default, #7001) — a matrix chat in a room the caller is not
// in, and non-matrix channels (console/qq, other session namespaces).
const roomListPayload = `[` +
	`{"id":"aaaa0000-0000-4000-8000-000000000001","name":"matrix:!team:ex","session_id":"matrix:!team:ex","user_id":"@manager:ex","channel":"matrix"},` +
	`{"id":"aaaa0000-0000-4000-8000-000000000002","name":"matrix:!other:ex","session_id":"matrix:!other:ex","user_id":"@manager:ex","channel":"matrix"},` +
	`{"id":"aaaa0000-0000-4000-8000-000000000003","name":"console","session_id":"default","user_id":"default","channel":"console"},` +
	`{"id":"aaaa0000-0000-4000-8000-000000000004","name":"qq","session_id":"qq:9527","user_id":"9527","channel":"qq"}` +
	`]`

// TestChat_L2RoomParticipation_ListFiltersToCallerRooms is the core room
// level boundary: an L2 human gets the worker's matrix chats for the rooms
// she is a current member of — INCLUDING the agent-driven (manager/leader)
// sessions that carry the diagnostic evidence, and EXCLUDING other rooms
// and non-matrix channels. Client-supplied filters are dropped; the
// upstream fetch is forced to channel=matrix; the membership source is
// called with the caller's own token.
func TestChat_L2RoomParticipation_ListFiltersToCallerRooms(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(roomListPayload))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	fn, tokenSeen := roomsStub([]string{"!team:ex"}, nil)
	h.joinedRooms = fn

	req := chatsListRequest("market-writer", "?user_id=bob&include_app_owned=true&archived=false")
	req = l2Caller(req, "alice-token")
	rec := httptest.NewRecorder()
	h.listChats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if gotQuery != "channel=matrix" {
		t.Fatalf("upstream query=%q, want channel=matrix (client filters dropped for L2)", gotQuery)
	}
	if *tokenSeen != "alice-token" {
		t.Fatalf("joined_rooms called with token %q, want the caller's own token", *tokenSeen)
	}
	want := `[{"id":"aaaa0000-0000-4000-8000-000000000001","name":"matrix:!team:ex","session_id":"matrix:!team:ex","user_id":"@manager:ex","channel":"matrix"}]`
	if body := strings.TrimSpace(rec.Body.String()); body != want {
		t.Fatalf("list=%s, want exactly the caller-room matrix chat (agent-driven session included, other rooms/channels excluded)", body)
	}
}

// TestChat_L2RoomParticipation_InRoomAgentDrivenDetail200 locks the
// behavior that changed from the sender-level v1: a chat in the caller's
// room whose user_id is the MANAGER's MXID (agent-driven session, isolated
// mode) is readable — v1's strict "own MXID only" precheck 404'd it.
func TestChat_L2RoomParticipation_InRoomAgentDrivenDetail200(t *testing.T) {
	const payload = `{"messages":[{"id":"m1","type":"message","role":"user","content":[{"type":"text","text":"delegation log"}],"status":"completed"}],"status":"idle"}`
	var detailDials int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/chats/aaaa0000-0000-4000-8000-000000000001") {
			detailDials++
			_, _ = w.Write([]byte(payload))
			return
		}
		_, _ = w.Write([]byte(roomListPayload))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	h.joinedRooms, _ = roomsStub([]string{"!team:ex"}, nil)

	req := chatsDetailRequest("market-writer", "aaaa0000-0000-4000-8000-000000000001", "")
	req = l2Caller(req, "alice-token")
	rec := httptest.NewRecorder()
	h.getChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for an in-room chat", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), payload)
	}
	if detailDials != 1 {
		t.Fatalf("upstream detail dialed %d times, want 1", detailDials)
	}
}

// TestChat_L2RoomParticipation_OtherRoomDetail404: a chat in a room the
// caller is not in is hidden as a uniform 404 (upstream's own not-found
// shape) and the detail endpoint is never dialed — no existence probe, no
// content.
func TestChat_L2RoomParticipation_OtherRoomDetail404(t *testing.T) {
	var detailDials int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/chats/aaaa0000-0000-4000-8000-000000000002") {
			detailDials++
			_, _ = w.Write([]byte(`{"messages":[{"id":"m1","type":"message","role":"user","content":[{"type":"text","text":"other room secret"}],"status":"completed"}],"status":"idle"}`))
			return
		}
		_, _ = w.Write([]byte(roomListPayload))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	h.joinedRooms, _ = roomsStub([]string{"!team:ex"}, nil)

	req := chatsDetailRequest("market-writer", "aaaa0000-0000-4000-8000-000000000002", "")
	req = l2Caller(req, "alice-token")
	rec := httptest.NewRecorder()
	h.getChat(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404 for a chat in a room the caller is not in", rec.Code, rec.Body.String())
	}
	want := `{"detail":"Chat not found: aaaa0000-0000-4000-8000-000000000002"}`
	if rec.Body.String() != want {
		t.Fatalf("body=%s, want %s (uniform, indistinguishable from upstream 404)", rec.Body.String(), want)
	}
	if detailDials != 0 {
		t.Fatalf("upstream detail dialed %d times, want 0 (no content leak)", detailDials)
	}
}

// TestChat_L2RoomParticipation_NonMatrixChatDetail404: console/qq/app-owned
// chats have no "!"-prefixed room namespace, so matrixRoomID yields "" and
// they are never visible to an L2 human — fail closed, no per-channel
// allowlist.
func TestChat_L2RoomParticipation_NonMatrixChatDetail404(t *testing.T) {
	var detailDials int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/chats/aaaa0000-0000-4000-8000-000000000003") {
			detailDials++
			_, _ = w.Write([]byte(`{"messages":[],"status":"idle"}`))
			return
		}
		_, _ = w.Write([]byte(roomListPayload))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	h.joinedRooms, _ = roomsStub([]string{"!team:ex"}, nil)

	req := chatsDetailRequest("market-writer", "aaaa0000-0000-4000-8000-000000000003", "")
	req = l2Caller(req, "alice-token")
	rec := httptest.NewRecorder()
	h.getChat(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404 for a non-matrix chat", rec.Code, rec.Body.String())
	}
	if detailDials != 0 {
		t.Fatalf("upstream detail dialed %d times, want 0 (no content leak)", detailDials)
	}
}

// TestChat_L2RoomParticipation_SharedSessionModeVisible locks the second
// share_session_in_group mode (room-wide sharing, legacy/optional): the
// single room session stores the ROOM ID as user_id — sender-level
// anchoring could never match it; room-level anchoring admits it for
// every current room member.
func TestChat_L2RoomParticipation_SharedSessionModeVisible(t *testing.T) {
	const shared = `[{"id":"bbbb0000-0000-4000-8000-000000000001","name":"shared-room-session","session_id":"matrix:!team:ex","user_id":"!team:ex","channel":"matrix"}]`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(shared))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	h.joinedRooms, _ = roomsStub([]string{"!team:ex"}, nil)

	req := chatsListRequest("market-writer", "")
	req = l2Caller(req, "alice-token")
	rec := httptest.NewRecorder()
	h.listChats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != shared {
		t.Fatalf("list=%s, want the shared room session (user_id = room id)", body)
	}
}

// TestChat_L2RoomParticipation_StatusSameBoundary: the status route runs
// the same room precheck (in-room chat 200, other-room chat 404).
func TestChat_L2RoomParticipation_StatusSameBoundary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/chats/") && strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = w.Write([]byte(`{"status":"running"}`))
			return
		}
		_, _ = w.Write([]byte(roomListPayload))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	h.joinedRooms, _ = roomsStub([]string{"!team:ex"}, nil)
	caller := func(chatID string) *http.Request {
		return l2Caller(chatsStatusRequest("market-writer", chatID, ""), "alice-token")
	}

	// other room's chat → 404
	rec := httptest.NewRecorder()
	h.getChatStatus(rec, caller("aaaa0000-0000-4000-8000-000000000002"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other-room chat: status=%d, want 404", rec.Code)
	}
	// in-room chat → 200
	rec2 := httptest.NewRecorder()
	h.getChatStatus(rec2, caller("aaaa0000-0000-4000-8000-000000000001"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("in-room chat: status=%d body=%s, want 200", rec2.Code, rec2.Body.String())
	}
}

// TestChat_L2RoomParticipation_JoinedRoomsFailure404: when the membership
// source fails, participation cannot be proven — fail closed with the
// uniform 404, no upstream dial (an unprovable anchor must not leak a
// conversation view).
func TestChat_L2RoomParticipation_JoinedRoomsFailure404(t *testing.T) {
	var dials int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(roomListPayload))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	h.joinedRooms, _ = roomsStub(nil, fmt.Errorf("homeserver down"))

	req := chatsListRequest("market-writer", "")
	req = l2Caller(req, "alice-token")
	rec := httptest.NewRecorder()
	h.listChats(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want uniform 404 (fail closed)", rec.Code, rec.Body.String())
	}
	if dials != 0 {
		t.Fatalf("upstream dialed %d times, want 0", dials)
	}
}

// TestChat_L2RoomParticipation_NoToken404: an L2 caller whose request
// carries no bearer token cannot anchor participation — uniform 404.
func TestChat_L2RoomParticipation_NoToken404(t *testing.T) {
	var dials int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials++
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	h.joinedRooms, _ = roomsStub([]string{"!team:ex"}, nil)

	req := l2Caller(chatsListRequest("market-writer", ""), "")
	rec := httptest.NewRecorder()
	h.listChats(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want uniform 404 without a token", rec.Code, rec.Body.String())
	}
	if dials != 0 {
		t.Fatalf("upstream dialed %d times, want 0", dials)
	}
}

// TestChat_L2RoomParticipation_NoMatrixSource404: a deploy without a
// Matrix client (joinedRooms nil) fails closed for L2 humans.
func TestChat_L2RoomParticipation_NoMatrixSource404(t *testing.T) {
	var dials int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials++
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	// joinedRooms left nil — no Matrix source wired.

	req := l2Caller(chatsListRequest("market-writer", ""), "alice-token")
	rec := httptest.NewRecorder()
	h.listChats(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want uniform 404 with no matrix source", rec.Code, rec.Body.String())
	}
	if dials != 0 {
		t.Fatalf("upstream dialed %d times, want 0", dials)
	}
}

// TestChat_L2RoomParticipation_PrecheckUpstreamFailure502: when the
// precheck list call itself fails (worker 5xx), participation cannot be
// proven — 502, not a false 404 (a healthy worker must not be silently
// hidden).
func TestChat_L2RoomParticipation_PrecheckUpstreamFailure502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/chats/") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"running"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	h.joinedRooms, _ = roomsStub([]string{"!team:ex"}, nil)

	req := chatsStatusRequest("market-writer", "aaaa0000-0000-4000-8000-000000000001", "")
	req = l2Caller(req, "alice-token")
	rec := httptest.NewRecorder()
	h.getChatStatus(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502 (unprovable participation is not a 404)", rec.Code, rec.Body.String())
	}
}

// TestChat_L2RoomParticipation_ScopeCheckStillApplies locks the scoped
// caller fix end-to-end for the room-level boundary: an L2 human whose
// team contains the worker resolves 200 (findTeamMember's second return
// value is the member name, not the team name — the check must compare
// against the Team CR name).
func TestChat_L2RoomParticipation_ScopeCheckStillApplies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	h.joinedRooms, _ = roomsStub([]string{}, nil)

	req := l2Caller(chatsListRequest("market-writer", ""), "alice-token")
	rec := httptest.NewRecorder()
	h.listChats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for in-scope L2 human (empty room set is still a valid anchor)", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Fatalf("list=%s, want [] (no rooms joined)", body)
	}
}

// TestChat_L3HumanDenied: L3 (worker-scoped) humans have no chats access
// in v1 — consistent with #1277, which keeps the other read surfaces
// (checkpoints/skills/...) team-scoped and lists extensions as follow-ups.
func TestChat_L3HumanDenied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be dialed for L3")
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("market-team", "market-writer")...)
	req := chatsListRequest("market-writer", "")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "dave", AccessibleWorkers: []string{"market-writer"}})
	rec := httptest.NewRecorder()
	h.listChats(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for L3 human (no chats access in v1)", rec.Code)
	}
}

// TestChat_StandaloneHumanDenied: an L2 human with no teams cannot read any
// worker's sessions (W8: 404, not 403).
func TestChat_StandaloneHumanDenied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	req := chatsListRequest("daily-carol", "")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "outsider"})
	rec := httptest.NewRecorder()
	h.listChats(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for standalone human (W8)", rec.Code)
	}
}

// TestChatsWorkerBaseURL_PrefixAndPortResolution covers the address
// resolution matrix: default / non-default / empty container prefixes. The
// console port is ALWAYS the effective system port: the worker system env
// defines AGENTTEAMS_CONSOLE_PORT and the system-wins user-env merge
// discards conflicting spec.env values before the container is created, so
// any user-declared port (valid, padded, invalid, out-of-range) must
// resolve to the same port the container actually listens on.
func TestChatsWorkerBaseURL_PrefixAndPortResolution(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		env    map[string]string
		want   string
	}{
		{"default prefix and port", "agentteams-worker-", nil, "http://agentteams-worker-alice:8088"},
		{"non-default prefix", "acme-worker-", nil, "http://acme-worker-alice:8088"},
		{"empty prefix (auto-prefix disabled)", "", nil, "http://alice:8088"},
		{"user port discarded (system wins)", "agentteams-worker-", map[string]string{"AGENTTEAMS_CONSOLE_PORT": "9090"}, "http://agentteams-worker-alice:8088"},
		{"custom prefix, user port discarded", "acme-worker-", map[string]string{"AGENTTEAMS_CONSOLE_PORT": " 7000 "}, "http://acme-worker-alice:8088"},
		{"invalid user port discarded", "agentteams-worker-", map[string]string{"AGENTTEAMS_CONSOLE_PORT": "not-a-port"}, "http://agentteams-worker-alice:8088"},
		{"out-of-range user port discarded", "agentteams-worker-", map[string]string{"AGENTTEAMS_CONSOLE_PORT": "99999"}, "http://agentteams-worker-alice:8088"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ChatsHandler{containerPrefix: tc.prefix}
			if got := h.defaultWorkerBaseURL("alice", tc.env); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestChats_EffectivePrefixAndPortReachUpstream is the end-to-end
// regression: a controller configured with a non-default container prefix
// and a worker whose spec.env conflicts with the system console port must
// dial exactly http://{prefix}{name}:{effective-port}.
func TestChats_EffectivePrefixAndPortReachUpstream(t *testing.T) {
	objs := checkpointTeamWithWorkers("team-a", "daily-carol")
	objs[1].(*v1beta1.Worker).Spec.Env = map[string]string{"AGENTTEAMS_CONSOLE_PORT": "9090"}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()

	h := NewChatsHandler(k8s, "default", "embedded", "acme-worker-")
	rt := &captureRoundTripper{gotURL: &url.URL{}}
	h.http = &http.Client{Transport: rt}

	rec := httptest.NewRecorder()
	h.listChats(rec, adminCaller(chatsListRequest("daily-carol", "")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := "http://acme-worker-daily-carol:8088/api/chats"
	if rt.gotURL.String() != want {
		t.Fatalf("upstream URL=%s, want %s", rt.gotURL.String(), want)
	}
}

// TestChatsPort_MatchesContainerCreationEnvChain is the cross-component
// regression: the port the proxy resolves must equal the port the docker
// backend derives, computed through the real container-creation env chain
// (WorkerEnvBuilder.Build + the shared system-wins merge).
func TestChatsPort_MatchesContainerCreationEnvChain(t *testing.T) {
	userEnv := map[string]string{"AGENTTEAMS_CONSOLE_PORT": "9090"}

	builder := service.NewWorkerEnvBuilder(config.WorkerEnvDefaults{})
	sysEnv := builder.Build("daily-carol", &service.WorkerProvisionResult{
		GatewayKey:    "gk",
		MatrixToken:   "mt",
		RoomID:        "!room",
		MinIOPassword: "mp",
	})
	service.MergeUserEnv(sysEnv, userEnv) // same semantics the reconciler applies
	dockerSide := sysEnv[service.WorkerConsolePortEnv]

	proxySide := service.EffectiveWorkerConsolePort(userEnv)

	if dockerSide != proxySide {
		t.Fatalf("docker backend port %q != chats proxy port %q — proxy would 502", dockerSide, proxySide)
	}
	if dockerSide != "8088" {
		t.Fatalf("effective console port=%q, want %q (system default, user value must be discarded)", dockerSide, "8088")
	}
}

// TestChats_UpstreamHeadersCopiedOn200 pins that a 200 response streams the
// upstream body verbatim (large transcripts must not be capped or re-encoded).
func TestChats_UpstreamBodyStreamsVerbatim(t *testing.T) {
	big := strings.Repeat(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"lorem ipsum "}],"status":"completed"},`, 4096)
	payload := `{"messages":[` + strings.TrimSuffix(big, ",") + `],"status":"idle"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestChatsHandler(t, "embedded", upstream, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChat(rec, adminCaller(chatsDetailRequest("daily-carol", "0d9f3d6e-1a2b-4c3d-8e5f-6a7b8c9d0e1f", "")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), []byte(payload)) {
		t.Fatalf("large transcript not streamed verbatim: got %d bytes, want %d", rec.Body.Len(), len(payload))
	}
}

// Regression (review): the dial target must be the effective runtime name
// (spec.workerName when set) — the CR name only addresses the Kubernetes
// resource. A worker imported/renamed as `worker-cr` + `spec.workerName:
// runtime-alice` previously dialed the wrong container host for all three
// chat routes. The scoped-caller authorization above stays keyed on the
// original resource/team identity (covered by the participation tests).
func TestChatsProxy_DialsEffectiveRuntimeName(t *testing.T) {
	var dials []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "/chats/"):
			_, _ = w.Write([]byte(`{"id":"chat-1"}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer upstream.Close()

	renamed := checkpointWorker("worker-cr")
	renamed.Spec.WorkerName = "runtime-alice"
	plain := checkpointWorker("plain-cr")
	plain.Spec.WorkerName = "" // no override → effective name is the CR name
	team := checkpointTeam("team-a", "worker-cr", "plain-cr")
	h := newTestChatsHandler(t, "embedded", upstream, team, renamed, plain)
	h.workerBaseURL = func(name string, env map[string]string) string {
		dials = append(dials, name)
		return upstream.URL
	}

	call := func(run func(http.ResponseWriter, *http.Request), req *http.Request) int {
		rec := httptest.NewRecorder()
		run(rec, adminCaller(req))
		return rec.Code
	}

	if code := call(h.listChats, chatsListRequest("worker-cr", "")); code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	if code := call(h.getChat, chatsDetailRequest("worker-cr", "chat-1", "")); code != http.StatusOK {
		t.Fatalf("detail: status %d", code)
	}
	if code := call(h.getChatStatus, chatsStatusRequest("worker-cr", "chat-1", "")); code != http.StatusOK {
		t.Fatalf("status: status %d", code)
	}
	if code := call(h.listChats, chatsListRequest("plain-cr", "")); code != http.StatusOK {
		t.Fatalf("plain list: status %d", code)
	}

	want := []string{"runtime-alice", "runtime-alice", "runtime-alice", "plain-cr"}
	if len(dials) != len(want) {
		t.Fatalf("dials = %v, want %v", dials, want)
	}
	for i := range want {
		if dials[i] != want[i] {
			t.Fatalf("dial[%d] = %q, want %q (full: %v)", i, dials[i], want[i], dials)
		}
	}
}
