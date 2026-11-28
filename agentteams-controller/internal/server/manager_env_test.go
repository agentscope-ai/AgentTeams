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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestManagerEnvRoundTrip(t *testing.T) {
	mgr := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(mgr).Build()
	h := NewResourceHandler(c, "default", nil, "", nil)
	for _, body := range []string{`{"env":{"API_KEY":"a b\n$literal","EMPTY":""}}`, `{"model":"other"}`, `{"env":{}}`} {
		req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
		req.SetPathValue("name", "default")
		rec := httptest.NewRecorder()
		h.UpdateManager(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
		}
		var saved v1beta1.Manager
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(mgr), &saved); err != nil {
			t.Fatal(err)
		}
		var returned map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &returned); err != nil {
			t.Fatal(err)
		}
		if returned["envEditable"] != true {
			t.Fatal("missing environment editing capability")
		}
		if returned["env"] == nil {
			t.Fatal("editable environment must return an explicit object, including when empty")
		}
		if body == `{"env":{}}` {
			if len(saved.Spec.Env) != 0 {
				t.Fatal("empty env did not clear variables")
			}
		} else if saved.Spec.Env["API_KEY"] != "a b\n$literal" {
			t.Fatal("env was ignored or lost during unrelated update")
		}
		get := httptest.NewRequest(http.MethodGet, "/", nil)
		get.SetPathValue("name", "default")
		read := httptest.NewRecorder()
		h.GetManager(read, get)
		if read.Code != http.StatusOK {
			t.Fatalf("get: %d", read.Code)
		}
		var response struct {
			Env      map[string]string `json:"env"`
			Editable bool              `json:"envEditable"`
		}
		if err := json.Unmarshal(read.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if !response.Editable || response.Env == nil || len(response.Env) != len(saved.Spec.Env) || response.Env["API_KEY"] != saved.Spec.Env["API_KEY"] {
			t.Fatal("saved env did not read back")
		}
	}
}

func TestManagerEnvRejectsInvalidAndScopedWrites(t *testing.T) {
	for _, tc := range []struct {
		body, role string
		status     int
	}{
		{`{"env":{"api-key":"private-value"}}`, authpkg.RoleAdmin, 400},
		{`{"env":{"OK":"a\u0000b"}}`, authpkg.RoleAdmin, 400},
		{`{"env":{"AGENTTEAMS_MANAGER_MATRIX_TOKEN":"private-value"}}`, authpkg.RoleAdmin, 400},
		{`{"env":{"HOME":"private-value"}}`, authpkg.RoleAdmin, 400},
		{`{"env":{"AGENTTEAMS_MANAGER_NOTIFY_CHANNEL":"private-value"}}`, authpkg.RoleAdmin, 400},
		{`{"env":{}}`, authpkg.RoleTeamLeader, 403},
	} {
		h := NewResourceHandler(fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).Build(), "default", nil, "", nil)
		req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(tc.body))
		req.SetPathValue("name", "default")
		req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{Role: tc.role}))
		rec := httptest.NewRecorder()
		h.UpdateManager(rec, req)
		if rec.Code != tc.status || strings.Contains(rec.Body.String(), "private-value") {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
}

func TestManagerEnvIsNotIncludedInListsOrScopedReads(t *testing.T) {
	mgr := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: v1beta1.ManagerSpec{Env: map[string]string{"API_KEY": "private-value"}}}
	h := NewResourceHandler(fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(mgr).Build(), "default", nil, "", nil)
	list := httptest.NewRecorder()
	h.ListManagers(list, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(list.Body.String(), "private-value") {
		t.Fatal("environment values leaked through list/export")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("name", "default")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{Role: authpkg.RoleHuman}))
	read := httptest.NewRecorder()
	h.GetManager(read, req)
	if strings.Contains(read.Body.String(), "private-value") || strings.Contains(read.Body.String(), `"envEditable":true`) {
		t.Fatal("environment values exposed to scoped caller")
	}
}

func TestManagerRuntimeChangeUsesSharedHarnessImageUnlessExplicit(t *testing.T) {
	for _, image := range []string{"", "custom-codex:local"} {
		t.Run(image, func(t *testing.T) {
			mgr := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: v1beta1.ManagerSpec{Runtime: "qwenpaw", Image: "old-qwenpaw:local"}}
			c := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(mgr).Build()
			h := NewResourceHandler(c, "default", nil, "", nil)
			body, _ := json.Marshal(map[string]string{"runtime": "codex", "image": image})
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(string(body)))
			req.SetPathValue("name", "default")
			rec := httptest.NewRecorder()
			h.UpdateManager(rec, req)
			if rec.Code != 200 {
				t.Fatalf("runtime update: %d %s", rec.Code, rec.Body.String())
			}
			var saved v1beta1.Manager
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(mgr), &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Spec.Runtime != "codex" || saved.Spec.Image != image {
				t.Fatalf("wrong runtime/image: %s %s", saved.Spec.Runtime, saved.Spec.Image)
			}
		})
	}
}
