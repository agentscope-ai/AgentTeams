package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResetHumanPassword(t *testing.T) {
	const newPassword = "test only 新密码"
	for _, tc := range []struct {
		name, role, body      string
		systemAdmin, disabled bool
		upstream, status      int
	}{
		{name: "success", role: authpkg.RoleAdmin, upstream: 200, status: 204},
		{name: "basic member denied", role: authpkg.RoleHuman, status: 403},
		{name: "manager denied", role: authpkg.RoleManager, status: 403},
		{name: "system account denied", role: authpkg.RoleAdmin, systemAdmin: true, status: 409},
		{name: "legacy deployment", role: authpkg.RoleAdmin, disabled: true, status: 503},
		{name: "short password", role: authpkg.RoleAdmin, body: `{"newPassword":"short"}`, status: 400},
		{name: "upstream rejects", role: authpkg.RoleAdmin, upstream: 403, status: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/_matrix/client/v3/login":
					_, _ = w.Write([]byte(`{"access_token":"test-admin-token"}`))
				case "/_synapse/admin/v1/reset_password/@target:test":
					resetCalls++
					var body struct {
						Password string `json:"new_password"`
						Logout   bool   `json:"logout_devices"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password != newPassword ||
						!body.Logout || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-admin-token" {
						t.Error("invalid Matrix password reset request")
					}
					w.WriteHeader(tc.upstream)
				default:
					t.Errorf("unexpected Matrix request to %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			human := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
				Status: v1beta1.HumanStatus{MatrixUserID: "@target:test", InitialPassword: "initial"}}
			if tc.systemAdmin {
				human.Labels = map[string]string{v1beta1.LabelSystemAdmin: "true"}
			}
			k8s := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithStatusSubresource(human).WithObjects(human).Build()
			store := ossfake.NewMemory()
			handler := NewResourceHandler(k8s, "default", nil, "", audit.NewClient(store))
			handler.passwordResetEnabled = !tc.disabled
			handler.passwordResetter = matrix.NewTuwunelClient(matrix.Config{ServerURL: upstream.URL, Domain: "test", AdminUser: "admin"}, upstream.Client())
			body := tc.body
			if body == "" {
				input, _ := json.Marshal(map[string]string{"newPassword": newPassword})
				body = string(input)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/humans/target/reset-password", strings.NewReader(body))
			req.SetPathValue("name", "target")
			req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{Role: tc.role, Username: "operator"}))
			res := httptest.NewRecorder()
			handler.ResetHumanPassword(res, req)
			if res.Code != tc.status || (resetCalls == 1) != (tc.upstream != 0) || strings.Contains(res.Body.String(), newPassword) {
				t.Fatalf("unexpected result: status=%d, Matrix reset calls=%d", res.Code, resetCalls)
			}
			var updated v1beta1.Human
			if err := k8s.Get(req.Context(), client.ObjectKeyFromObject(human), &updated); err != nil {
				t.Fatal(err)
			}
			if tc.status == 204 {
				events := auditEvents(t, store)
				if updated.Status.InitialPassword != "" || len(events) != 1 || !strings.Contains(events[0], "human_password_reset") ||
					strings.Contains(events[0], newPassword) {
					t.Fatal("successful reset should clear the initial password and audit only the operation")
				}
			} else if updated.Status.InitialPassword != "initial" {
				t.Fatal("unsuccessful reset changed the initial password")
			}
		})
	}
}
