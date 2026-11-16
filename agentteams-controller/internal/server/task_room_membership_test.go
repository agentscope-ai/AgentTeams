package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestTaskRoomAutoJoinOnlyInvitedTeamHumans(t *testing.T) {
	h, _ := coordinationFixture(t)
	var team v1beta1.Team
	_ = h.deps.Client.Get(context.Background(), client.ObjectKey{Name: "alpha", Namespace: "default"}, &team)
	team.Spec.Admin = &v1beta1.TeamAdminSpec{Name: "admin", MatrixUserID: "@admin:test"}
	team.Spec.HumanMembers = []v1beta1.TeamMemberSpec{{Name: "alice", MatrixUserID: "@alice:test"}}
	if err := h.deps.Client.Update(context.Background(), &team); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"admin", "alice", "bob", "outsider"} {
		human := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Status: v1beta1.HumanStatus{MatrixUserID: "@" + name + ":test"}}
		if name == "bob" {
			human.Spec.AccessibleTeams = []string{"alpha"}
		}
		if err := h.deps.Client.Create(context.Background(), human); err != nil {
			t.Fatal(err)
		}
	}
	joined := map[string]bool{}
	joins := 0
	roomTeam := "alpha"
	homeserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/state/room.meta/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"roomKind": "task_room", "teamName": roomTeam, "leaderWorker": map[string]any{"userId": "@lead:test"}})
		case strings.HasSuffix(r.URL.Path, "/members"):
			events := []any{}
			for _, name := range []string{"admin", "alice", "bob", "outsider"} {
				status := "invite"
				if joined[name] {
					status = "join"
				}
				events = append(events, map[string]any{"state_key": "@" + name + ":test", "content": map[string]any{"membership": status}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"chunk": events})
		case strings.HasSuffix(r.URL.Path, "/login"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["type"] != "m.login.application_service" {
				t.Error("auto-join changed identity protocol")
			}
			name := body["identifier"].(map[string]any)["user"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": name + "-token", "user_id": "@" + name + ":test"})
		case strings.HasSuffix(r.URL.Path, "/join"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-token")
			if name == "outsider" {
				t.Error("joined an unrelated invited user")
			}
			joined[name] = true
			joins++
			_, _ = w.Write([]byte(`{"room_id":"!task:test"}`))
		default:
			t.Errorf("unexpected Matrix request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer homeserver.Close()
	cfg := matrix.Config{ServerURL: homeserver.URL, Domain: "test", AppServiceEnabled: true, AppServiceToken: "test-as-token"}
	mx := matrix.NewTuwunelClient(cfg, homeserver.Client())
	creds := &service.FileCredentialStore{Dir: t.TempDir()}
	_ = creds.Save(context.Background(), "lead-cr", &service.WorkerCredentials{MatrixToken: "leader-token"})
	h.deps.MatrixClient = mx
	h.deps.MatrixConfig = cfg
	h.deps.Provisioner = service.NewProvisioner(service.ProvisionerConfig{Matrix: mx, MatrixConfig: cfg, Creds: creds, MatrixDomain: "test"})
	call := func(role, scope string) *httptest.ResponseRecorder {
		req := withCaller(httptest.NewRequest("POST", "/join-humans", nil), &authpkg.CallerIdentity{Role: role, Username: "lead-cr", Team: scope})
		req.SetPathValue("name", "alpha")
		req.SetPathValue("roomId", "!task:test")
		rec := httptest.NewRecorder()
		h.JoinTaskRoomHumans(rec, req)
		return rec
	}
	for i := 0; i < 2; i++ {
		rec := call(authpkg.RoleTeamLeader, "alpha")
		if rec.Code != 200 {
			t.Fatalf("auto-join %d: %s", rec.Code, rec.Body.String())
		}
	}
	if joins != 3 || !joined["admin"] || !joined["alice"] || !joined["bob"] || joined["outsider"] {
		t.Fatalf("joins=%d users=%v", joins, joined)
	}
	roomTeam = "foreign"
	if rec := call(authpkg.RoleTeamLeader, "alpha"); rec.Code != 403 {
		t.Fatal("accepted a foreign task room")
	}
	if rec := call(authpkg.RoleWorker, "alpha"); rec.Code != 403 {
		t.Fatal("Worker obtained Human session authority")
	}
	if rec := call(authpkg.RoleTeamLeader, "foreign"); rec.Code != 403 {
		t.Fatal("accepted a foreign caller scope")
	}
	if joins != 3 {
		t.Fatal("unauthorized or repeated request joined users")
	}
}
