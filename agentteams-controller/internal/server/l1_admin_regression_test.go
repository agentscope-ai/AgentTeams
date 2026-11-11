package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/skillscan"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type regressionWhoami struct{}

func (regressionWhoami) Whoami(_ context.Context, token string) (string, error) {
	switch token {
	case "alice", "bob", "carol":
		return "@" + token + ":matrix.local", nil
	}
	return "", fmt.Errorf("invalid token")
}

// Exercise real route registration, token authentication, authorization and
// handlers. K8s and object storage are local test implementations, not clusters.
func TestL1AdminRegression(t *testing.T) {
	newRig := func(t *testing.T) (*HTTPServer, *ossfake.Memory) {
		t.Helper()
		alice := &v1.Human{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}, Spec: v1.HumanSpec{PermissionLevel: 1}, Status: v1.HumanStatus{MatrixUserID: "@alice:matrix.local"}}
		bob := &v1.Human{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "default"}, Spec: v1.HumanSpec{PermissionLevel: 2, AccessibleTeams: []string{"team-a"}}}
		carol := &v1.Human{ObjectMeta: metav1.ObjectMeta{Name: "carol", Namespace: "default"}, Spec: v1.HumanSpec{PermissionLevel: 3, AccessibleWorkers: []string{"lead-a"}}}
		a := &v1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"}, Spec: v1.TeamSpec{WorkerMembers: []v1.TeamWorkerRef{{Name: "lead-a", Role: "team_leader"}}}}
		b := &v1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Namespace: "default"}, Spec: v1.TeamSpec{WorkerMembers: []v1.TeamWorkerRef{{Name: "lead-b", Role: "team_leader"}}}}
		wa := &v1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "lead-a", Namespace: "default"}, Spec: v1.WorkerSpec{Model: "test-model"}}
		wb := &v1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "lead-b", Namespace: "default"}, Spec: v1.WorkerSpec{Model: "test-model"}}
		k8s := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(alice, bob, carol, a, b, wa, wb).Build()
		store := ossfake.NewMemory()
		authenticator := auth.NewMatrixTokenAuthenticator(k8s, "default", regressionWhoami{})
		deps := ServerDeps{Client: k8s, Namespace: "default", OSS: &mcLikeOSS{Memory: store}, AuthMw: auth.NewMiddleware(authenticator, auth.NewCREnricher(k8s, "default"), auth.NewAuthorizer(), k8s, "default")}
		deps.SkillScanner = skillscan.New(skillscan.Config{KubeMode: "k8s"})
		return NewHTTPServer(":0", deps), store
	}
	request := func(t *testing.T, s *HTTPServer, token, method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s as %s: got %d want %d: %s", method, path, token, w.Code, want, w.Body)
		}
		return w
	}
	t.Run("resource CRUD and permission assignment", func(t *testing.T) {
		s, _ := newRig(t)
		cases := []struct{ kind, name, create, update string }{
			{"humans", "target", `{"name":"target","permissionLevel":2}`, `{"permissionLevel":3,"accessibleWorkers":["lead-b"],"displayName":"Updated"}`},
			{"workers", "new-worker", `{"name":"new-worker","model":"test-model","containerManaged":false}`, `{"model":"updated-model","state":"Sleeping"}`},
			{"managers", "new-manager", `{"name":"new-manager","model":"test-model","runtime":"qwenpaw"}`, `{"model":"updated-model","state":"Sleeping"}`},
			{"teams", "new-team", `{"name":"new-team","workerMembers":[{"name":"new-lead","role":"team_leader"}]}`, `{"heartbeatEvery":"10m"}`},
		}
		for _, tc := range cases {
			t.Run(tc.kind, func(t *testing.T) {
				base := "/api/v1/" + tc.kind
				path := base + "/" + tc.name
				if tc.kind == "teams" {
					request(t, s, "alice", "POST", "/api/v1/workers", `{"name":"new-lead","model":"test-model"}`, 201)
				}
				for _, token := range []string{"bob", "carol"} {
					request(t, s, token, "POST", base, tc.create, 403)
				}
				request(t, s, "alice", "POST", base, tc.create, 201)
				request(t, s, "alice", "GET", base, "", 200)
				request(t, s, "alice", "GET", path, "", 200)
				request(t, s, "alice", "PUT", path, tc.update, 200)
				got := request(t, s, "alice", "GET", path, "", 200)
				if tc.kind == "humans" && !strings.Contains(got.Body.String(), `"permissionLevel":3`) {
					t.Fatal("permission update not persisted")
				}
				if tc.kind == "workers" || tc.kind == "managers" {
					if !strings.Contains(got.Body.String(), "updated-model") {
						t.Fatal("model update not persisted")
					}
				}
				for _, token := range []string{"bob", "carol"} {
					request(t, s, token, "DELETE", path, "", 403)
				}
				request(t, s, "alice", "DELETE", path, "", 204)
				request(t, s, "alice", "GET", path, "", 404)
			})
		}
	})
	t.Run("cross-team Worker and Human reads", func(t *testing.T) {
		s, _ := newRig(t)
		request(t, s, "alice", "GET", "/api/v1/workers/lead-b", "", 200)
		request(t, s, "alice", "PUT", "/api/v1/workers/lead-b", `{"model":"admin-updated"}`, 200)
		request(t, s, "bob", "GET", "/api/v1/workers/lead-b", "", 404)
		request(t, s, "carol", "GET", "/api/v1/workers/lead-b", "", 404)
		request(t, s, "carol", "GET", "/api/v1/workers/lead-a", "", 200)
		request(t, s, "alice", "GET", "/api/v1/humans/bob", "", 200)
		request(t, s, "bob", "GET", "/api/v1/humans/alice", "", 403)
	})
	t.Run("cross-user files and recycle bin", func(t *testing.T) {
		s, _ := newRig(t)
		path := "/api/v1/humans/bob/files/regression.txt"
		r := httptest.NewRequest("PUT", path, strings.NewReader("admin-file-content"))
		r.Header.Set("Authorization", "Bearer alice")
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		s.Mux.ServeHTTP(w, r)
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("write: %d %s", w.Code, w.Body)
		}
		got := request(t, s, "alice", "GET", path, "", 200)
		if got.Body.String() != "admin-file-content" {
			t.Fatal("file content mismatch")
		}
		request(t, s, "carol", "GET", path, "", 404)
		request(t, s, "alice", "DELETE", path, "", 200)
		trash := request(t, s, "alice", "GET", "/api/v1/humans/bob/trash", "", 200)
		var listing struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(trash.Body.Bytes(), &listing); err != nil {
			t.Fatal(err)
		}
		if len(listing.Items) != 1 {
			t.Fatalf("trash listing: %s", trash.Body)
		}
		id := listing.Items[0].ID
		request(t, s, "alice", "POST", "/api/v1/humans/bob/trash/"+id+"/restore", `{}`, 200)
		request(t, s, "alice", "GET", path, "", 200)
		request(t, s, "alice", "DELETE", path, "", 200)
		trash = request(t, s, "alice", "GET", "/api/v1/humans/bob/trash", "", 200)
		json.Unmarshal(trash.Body.Bytes(), &listing)
		request(t, s, "alice", "DELETE", "/api/v1/humans/bob/trash/"+listing.Items[0].ID, "", 200)
	})
	t.Run("cross-team project controls and audit attribution", func(t *testing.T) {
		s, store := newRig(t)
		putProject(store, "teams/team-b/shared/projects/p-b/meta.json", map[string]any{"project_id": "p-b", "team_id": "team-b", "title": "Regression", "status": "active", "plan_type": "dag", "tasks": []any{}})
		request(t, s, "alice", "GET", "/api/v1/projects/p-b/workflow", "", 200)
		request(t, s, "bob", "GET", "/api/v1/projects/p-b/workflow", "", 404)
		request(t, s, "alice", "POST", "/api/v1/projects/p-b/pause", `{"reason":"admin regression"}`, 200)
		data, _ := store.GetObject(context.Background(), "teams/team-b/shared/projects/p-b/meta.json")
		if !bytes.Contains(data, []byte(`"actor":"alice"`)) {
			t.Fatalf("canonical actor missing: %s", data)
		}
		request(t, s, "alice", "POST", "/api/v1/projects/p-b/resume", `{}`, 200)
		request(t, s, "alice", "GET", "/api/v1/audit", "", 200)
		request(t, s, "bob", "GET", "/api/v1/audit", "", 400)
	})
	t.Run("cross-team skill publishing and catalog", func(t *testing.T) {
		s, store := newRig(t)
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		form.WriteField("scope", "team")
		form.WriteField("team", "team-b")
		file, _ := form.CreateFormFile("file", "regression.zip")
		file.Write(skillZip("regression-skill", nil))
		form.Close()
		r := httptest.NewRequest("POST", "/api/v1/skills", &body)
		r.Header.Set("Content-Type", form.FormDataContentType())
		r.Header.Set("Authorization", "Bearer alice")
		w := httptest.NewRecorder()
		s.Mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("upload: %d %s", w.Code, w.Body)
		}
		if _, err := store.GetObject(context.Background(), "teams/team-b/skills/regression-skill/SKILL.md"); err != nil {
			t.Fatal(err)
		}
		request(t, s, "alice", "GET", "/api/v1/skills?team=team-b", "", 200)
		request(t, s, "bob", "GET", "/api/v1/skills?team=team-b", "", 404)
		request(t, s, "alice", "GET", "/api/v1/skills/regression-skill/download?scope=team&team=team-b", "", 200)
	})
	t.Run("credential and rotation authorization boundaries", func(t *testing.T) {
		s, _ := newRig(t)
		for _, path := range []string{"/api/v1/credentials/sts", "/api/v1/credentials/matrix-token"} {
			request(t, s, "alice", "POST", path, `{}`, 503)
			request(t, s, "bob", "POST", path, `{}`, 403)
			request(t, s, "carol", "POST", path, `{}`, 403)
		}
		request(t, s, "alice", "POST", "/api/v1/appservice/rotate-token", `{}`, 400)
		request(t, s, "bob", "POST", "/api/v1/appservice/rotate-token", `{}`, 403)
		request(t, s, "carol", "POST", "/api/v1/appservice/rotate-token", `{}`, 403)
	})
}
