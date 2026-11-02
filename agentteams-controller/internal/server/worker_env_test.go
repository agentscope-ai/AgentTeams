package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWorkerEnvRoundTrip(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).Build()
	h := NewResourceHandler(c, "default", nil, "", nil)
	for _, tc := range []struct{ method, body, want string }{
		{"POST", `{"name":"alice","env":{"TOKEN":"a b\n$literal","EMPTY":""}}`, "a b\n$literal"},
		{"PUT", `{"model":"other"}`, "a b\n$literal"},
		{"PUT", `{"env":{"TOKEN":"new"}}`, "new"},
		{"PUT", `{"env":{}}`, ""},
	} {
		req := httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
		req.SetPathValue("name", "alice")
		rec := httptest.NewRecorder()
		if tc.method == "POST" {
			h.CreateWorker(rec, req)
		} else {
			h.UpdateWorker(rec, req)
		}
		if rec.Code >= 300 {
			t.Fatalf("%d: %s", rec.Code, rec.Body.String())
		}
		var worker v1beta1.Worker
		if err := c.Get(context.Background(), client.ObjectKey{Name: "alice", Namespace: "default"}, &worker); err != nil {
			t.Fatal(err)
		}
		if worker.Spec.Env["TOKEN"] != tc.want {
			t.Fatalf("env mismatch")
		}
	}
}

func TestWorkerEnvRejectsInvalidAndScopedWrites(t *testing.T) {
	for _, tc := range []struct {
		body, role string
		status     int
	}{
		{`{"env":{"BAD=NAME":"secret"}}`, authpkg.RoleAdmin, 400},
		{`{"env":{"OK":"a\u0000b"}}`, authpkg.RoleAdmin, 400},
		{`{"env":{}}`, authpkg.RoleTeamLeader, 403},
		{`{"env":{"TOKEN":"secret"}}`, authpkg.RoleHuman, 403},
	} {
		h := NewResourceHandler(fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).Build(), "default", nil, "", nil)
		req := httptest.NewRequest("PUT", "/", strings.NewReader(tc.body))
		req.SetPathValue("name", "alice")
		req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{Role: tc.role}))
		rec := httptest.NewRecorder()
		h.UpdateWorker(rec, req)
		if rec.Code != tc.status || strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestWorkerEnvHiddenFromNonAdmin(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "alice"
	worker.Namespace = "default"
	worker.Spec.Env = map[string]string{"TOKEN": "private-value"}
	c := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(worker).Build()
	h := NewResourceHandler(c, "default", nil, "", nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("name", "alice")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice"}))
	rec := httptest.NewRecorder()
	h.GetWorker(rec, req)
	if strings.Contains(rec.Body.String(), "private-value") {
		t.Fatal("environment leaked")
	}
}

func TestWorkerEnvSystemCollisionAndCustomPrefix(t *testing.T) {
	h := NewResourceHandler(fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).Build(), "default", nil, "", nil)
	for _, tc := range []struct {
		env   map[string]string
		valid bool
	}{
		{map[string]string{"HOME": "/tmp"}, false},
		{map[string]string{"AGENTTEAMS_CONSOLE_PORT": "9"}, false},
		{map[string]string{"AGENTTEAMS_MY_CUSTOM_FEATURE": "yes"}, true},
	} {
		rec := httptest.NewRecorder()
		if got := h.validateWorkerEnvRequest(rec, httptest.NewRequest("PUT", "/", nil), tc.env); got != tc.valid {
			t.Fatalf("valid=%v: %s", got, rec.Body.String())
		}
	}
}
