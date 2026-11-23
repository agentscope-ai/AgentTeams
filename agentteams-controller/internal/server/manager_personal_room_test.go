package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestManagerResponsesUseOnlyCallersPrivateRoom(t *testing.T) {
	manager := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Status: v1beta1.ManagerStatus{RoomID: "!system:localhost"}}
	for _, tc := range []struct {
		name       string
		level      int
		system     bool
		mxid, want string
	}{
		{"alice", 1, false, "@alice:localhost", "!alice:localhost"},
		{"bob", 1, false, "@bob:localhost", "!bob:localhost"},
		{"alice", 2, false, "@alice:localhost", ""},
		{"alice", 1, false, "@other:localhost", ""},
		{"admin", 1, true, "@admin:localhost", "!system:localhost"},
	} {
		t.Run(tc.name+tc.mxid+tc.want, func(t *testing.T) {
			human := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "default", Labels: map[string]string{}},
				Spec: v1beta1.HumanSpec{PermissionLevel: tc.level}, Status: v1beta1.HumanStatus{MatrixUserID: "@" + tc.name + ":localhost", ManagerRooms: map[string]string{"default": "!" + tc.name + ":localhost"}}}
			if tc.system {
				human.Labels[v1beta1.LabelSystemAdmin] = "true"
			}
			c := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(manager, human).Build()
			h := NewResourceHandler(c, "default", nil, "", nil)
			for _, list := range []bool{false, true} {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/managers/default", nil)
				r.SetPathValue("name", "default")
				r = r.WithContext(context.WithValue(r.Context(), auth.CallerKeyForTest(), &auth.CallerIdentity{Role: auth.RoleAdmin, Username: tc.name, MatrixUserID: tc.mxid, HumanPermissionLevel: 1}))
				w := httptest.NewRecorder()
				var got ManagerResponse
				if list {
					h.ListManagers(w, r)
					var response ManagerListResponse
					if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if len(response.Managers) != 1 {
						t.Fatalf("response=%s", w.Body.String())
					}
					got = response.Managers[0]
				} else {
					h.GetManager(w, r)
					if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
				}
				if w.Code != 200 || got.RoomID != tc.want {
					t.Fatalf("list=%v room=%q want=%q status=%d", list, got.RoomID, tc.want, w.Code)
				}
			}
		})
	}
}
