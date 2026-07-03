package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// sentMessage records a single SendMessageAsAdmin call for assertions.
type sentMessage struct {
	roomID string
	body   string
}

// stubMessengerClient implements matrix.Client, recording every
// SendMessageAsAdmin call. All other methods are no-ops; the message
// handler under test only ever calls SendMessageAsAdmin (via
// Provisioner.SendAdminMessage).
type stubMessengerClient struct {
	sent []sentMessage
	err  error
}

func (s *stubMessengerClient) EnsureUser(context.Context, matrix.EnsureUserRequest) (*matrix.UserCredentials, error) {
	return nil, nil
}
func (s *stubMessengerClient) CreateRoom(context.Context, matrix.CreateRoomRequest) (*matrix.RoomInfo, error) {
	return nil, nil
}
func (s *stubMessengerClient) ResolveRoomAlias(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (s *stubMessengerClient) DeleteRoomAlias(context.Context, string) error             { return nil }
func (s *stubMessengerClient) JoinRoom(context.Context, string, string) error            { return nil }
func (s *stubMessengerClient) LeaveRoom(context.Context, string, string) error           { return nil }
func (s *stubMessengerClient) SendMessage(context.Context, string, string, string) error { return nil }
func (s *stubMessengerClient) SendMessageAsAdmin(_ context.Context, roomID, body string) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, sentMessage{roomID: roomID, body: body})
	return nil
}
func (s *stubMessengerClient) Login(context.Context, string, string) (string, error) { return "", nil }
func (s *stubMessengerClient) SetDisplayName(context.Context, string, string, string) error {
	return nil
}
func (s *stubMessengerClient) AdminCommand(context.Context, string) error { return nil }
func (s *stubMessengerClient) ListJoinedRooms(context.Context, string) ([]string, error) {
	return nil, nil
}
func (s *stubMessengerClient) ListRoomMembers(context.Context, string) ([]matrix.RoomMember, error) {
	return nil, nil
}
func (s *stubMessengerClient) ListRoomMembersWithToken(context.Context, string, string) ([]matrix.RoomMember, error) {
	return nil, nil
}
func (s *stubMessengerClient) InviteToRoom(context.Context, string, string) error { return nil }
func (s *stubMessengerClient) InviteToRoomWithToken(context.Context, string, string, string) error {
	return nil
}
func (s *stubMessengerClient) KickFromRoom(context.Context, string, string, string) error {
	return nil
}
func (s *stubMessengerClient) KickFromRoomWithToken(context.Context, string, string, string, string) error {
	return nil
}
func (s *stubMessengerClient) UserID(localpart string) string { return "@" + localpart + ":test" }
func (s *stubMessengerClient) EnsureAppServiceUser(context.Context, string) (*matrix.UserCredentials, error) {
	return nil, nil
}
func (s *stubMessengerClient) LoginAppServiceUser(context.Context, string) (string, error) {
	return "", nil
}
func (s *stubMessengerClient) SetPasswordAsAdmin(context.Context, string, string) error { return nil }
func (s *stubMessengerClient) RegisterAppService(context.Context, matrix.AppServiceRegistration) error {
	return nil
}
func (s *stubMessengerClient) UnregisterAppService(context.Context, string) error { return nil }
func (s *stubMessengerClient) AppServiceSmokeTest(context.Context) error          { return nil }
func (s *stubMessengerClient) VerifyAccessToken(context.Context, string) error    { return nil }

func newMessageTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add hiclaw scheme: %v", err)
	}
	return scheme
}

func newTestProvisioner(m matrix.Client) *service.Provisioner {
	return service.NewProvisioner(service.ProvisionerConfig{Matrix: m})
}

func doMessageRequest(t *testing.T, handlerFn http.HandlerFunc, method, path, name string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	switch v := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case []byte:
		reader = bytes.NewReader(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, reader)
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	handlerFn(rec, req)
	return rec
}

func TestSendManagerMessageHappyPath(t *testing.T) {
	scheme := newMessageTestScheme(t)
	manager := &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{Name: "main", Namespace: "default"},
		Status:     v1beta1.ManagerStatus{RoomID: "!admin-dm:test"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Manager{}).
		WithObjects(manager).
		Build()
	stub := &stubMessengerClient{}
	handler := NewMessageHandler(k8sClient, newTestProvisioner(stub), "default")

	rec := doMessageRequest(t, handler.SendManagerMessage, http.MethodPost, "/api/v1/managers/main/message", "main", messageRequest{Body: "hello manager"})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(stub.sent))
	}
	if stub.sent[0].roomID != "!admin-dm:test" || stub.sent[0].body != "hello manager" {
		t.Fatalf("unexpected sent message: %+v", stub.sent[0])
	}

	var resp messageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.RoomID != "!admin-dm:test" || !resp.Sent {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestSendManagerMessageBadJSON(t *testing.T) {
	scheme := newMessageTestScheme(t)
	manager := &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{Name: "main", Namespace: "default"},
		Status:     v1beta1.ManagerStatus{RoomID: "!admin-dm:test"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Manager{}).
		WithObjects(manager).
		Build()
	stub := &stubMessengerClient{}
	handler := NewMessageHandler(k8sClient, newTestProvisioner(stub), "default")

	rec := doMessageRequest(t, handler.SendManagerMessage, http.MethodPost, "/api/v1/managers/main/message", "main", []byte("{not json"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.sent) != 0 {
		t.Fatalf("expected no message sent, got %d", len(stub.sent))
	}
}

func TestSendManagerMessageEmptyBody(t *testing.T) {
	scheme := newMessageTestScheme(t)
	manager := &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{Name: "main", Namespace: "default"},
		Status:     v1beta1.ManagerStatus{RoomID: "!admin-dm:test"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Manager{}).
		WithObjects(manager).
		Build()
	stub := &stubMessengerClient{}
	handler := NewMessageHandler(k8sClient, newTestProvisioner(stub), "default")

	rec := doMessageRequest(t, handler.SendManagerMessage, http.MethodPost, "/api/v1/managers/main/message", "main", messageRequest{Body: ""})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.sent) != 0 {
		t.Fatalf("expected no message sent, got %d", len(stub.sent))
	}
}

func TestSendManagerMessageUnknownManager(t *testing.T) {
	scheme := newMessageTestScheme(t)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Manager{}).
		Build()
	stub := &stubMessengerClient{}
	handler := NewMessageHandler(k8sClient, newTestProvisioner(stub), "default")

	rec := doMessageRequest(t, handler.SendManagerMessage, http.MethodPost, "/api/v1/managers/ghost/message", "ghost", messageRequest{Body: "hi"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendManagerMessageRoomNotProvisioned(t *testing.T) {
	scheme := newMessageTestScheme(t)
	manager := &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{Name: "main", Namespace: "default"},
		Status:     v1beta1.ManagerStatus{RoomID: ""},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Manager{}).
		WithObjects(manager).
		Build()
	stub := &stubMessengerClient{}
	handler := NewMessageHandler(k8sClient, newTestProvisioner(stub), "default")

	rec := doMessageRequest(t, handler.SendManagerMessage, http.MethodPost, "/api/v1/managers/main/message", "main", messageRequest{Body: "hi"})

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.sent) != 0 {
		t.Fatalf("expected no message sent, got %d", len(stub.sent))
	}
}

func TestSendTeamMessagePrefersLeaderDM(t *testing.T) {
	scheme := newMessageTestScheme(t)
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "default"},
		Status: v1beta1.TeamStatus{
			TeamRoomID:     "!team:test",
			LeaderDMRoomID: "!leader-dm:test",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Team{}).
		WithObjects(team).
		Build()
	stub := &stubMessengerClient{}
	handler := NewMessageHandler(k8sClient, newTestProvisioner(stub), "default")

	rec := doMessageRequest(t, handler.SendTeamMessage, http.MethodPost, "/api/v1/teams/backend/message", "backend", messageRequest{Body: "reprioritize"})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.sent) != 1 || stub.sent[0].roomID != "!leader-dm:test" || stub.sent[0].body != "reprioritize" {
		t.Fatalf("expected message body to leader DM room, got %+v", stub.sent)
	}
}

func TestSendTeamMessageFallsBackToTeamRoom(t *testing.T) {
	scheme := newMessageTestScheme(t)
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "default"},
		Status: v1beta1.TeamStatus{
			TeamRoomID:     "!team:test",
			LeaderDMRoomID: "",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Team{}).
		WithObjects(team).
		Build()
	stub := &stubMessengerClient{}
	handler := NewMessageHandler(k8sClient, newTestProvisioner(stub), "default")

	rec := doMessageRequest(t, handler.SendTeamMessage, http.MethodPost, "/api/v1/teams/backend/message", "backend", messageRequest{Body: "reprioritize"})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.sent) != 1 || stub.sent[0].roomID != "!team:test" || stub.sent[0].body != "reprioritize" {
		t.Fatalf("expected message body to team room, got %+v", stub.sent)
	}
}

func TestSendTeamMessageUnknownTeam(t *testing.T) {
	scheme := newMessageTestScheme(t)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()
	stub := &stubMessengerClient{}
	handler := NewMessageHandler(k8sClient, newTestProvisioner(stub), "default")

	rec := doMessageRequest(t, handler.SendTeamMessage, http.MethodPost, "/api/v1/teams/ghost/message", "ghost", messageRequest{Body: "hi"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendTeamMessageRoomNotProvisioned(t *testing.T) {
	scheme := newMessageTestScheme(t)
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "default"},
		Status:     v1beta1.TeamStatus{},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Team{}).
		WithObjects(team).
		Build()
	stub := &stubMessengerClient{}
	handler := NewMessageHandler(k8sClient, newTestProvisioner(stub), "default")

	rec := doMessageRequest(t, handler.SendTeamMessage, http.MethodPost, "/api/v1/teams/backend/message", "backend", messageRequest{Body: "hi"})

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}
