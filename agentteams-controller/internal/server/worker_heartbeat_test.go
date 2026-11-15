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

func TestWorkerHeartbeatRoundTrip(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).Build()
	h := NewResourceHandler(c, "default", nil, "", nil)
	for _, tc := range []struct {
		method, body string
		want         bool
	}{
		{"POST", `{"name":"alice","runtime":"openclaw","llmHeartbeatEnabled":false}`, false},
		{"PUT", `{"llmHeartbeatEnabled":true}`, true},
		{"PUT", `{"model":"other"}`, true},
		{"PUT", `{"llmHeartbeatEnabled":false}`, false},
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
		if worker.Spec.LlmHeartbeatEnabled == nil || *worker.Spec.LlmHeartbeatEnabled != tc.want {
			t.Fatalf("persisted switch = %v", worker.Spec.LlmHeartbeatEnabled)
		}
		read := httptest.NewRequest("GET", "/", nil)
		read.SetPathValue("name", "alice")
		result := httptest.NewRecorder()
		h.GetWorker(result, read)
		var got WorkerResponse
		if err := json.Unmarshal(result.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.LlmHeartbeatEnabled == nil || *got.LlmHeartbeatEnabled != tc.want {
			t.Fatalf("readback = %s", result.Body.String())
		}
	}
}

func TestWorkerHeartbeatRejectsScopedWrites(t *testing.T) {
	for _, role := range []string{authpkg.RoleHuman, authpkg.RoleTeamLeader, authpkg.RoleWorker} {
		h := NewResourceHandler(fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).Build(), "default", nil, "", nil)
		req := httptest.NewRequest("PUT", "/", strings.NewReader(`{"llmHeartbeatEnabled":false}`))
		req.SetPathValue("name", "alice")
		req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{Role: role}))
		rec := httptest.NewRecorder()
		h.UpdateWorker(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role=%s: %d %s", role, rec.Code, rec.Body.String())
		}
	}
}

func TestWorkerHeartbeatTeamReadback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override *bool
		want     bool
	}{
		{"inherit", nil, true}, {"disable", boolPtr(false), false}, {"enable", boolPtr(true), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := &v1beta1.Worker{Spec: v1beta1.WorkerSpec{Runtime: "openclaw", LlmHeartbeatEnabled: tc.override}}
			team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team"}, Spec: v1beta1.TeamSpec{HeartbeatEvery: "10m", WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "lead", Role: "team_leader"}}}}
			resp := workerToResponse(worker)
			applyTeamMember(&resp, team, "lead", tc.override)
			if resp.LlmHeartbeatEnabled == nil || *resp.LlmHeartbeatEnabled != tc.want {
				t.Fatalf("readback = %#v", resp.LlmHeartbeatEnabled)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func TestWorkerHeartbeatRejectsUnsupportedRuntime(t *testing.T) {
	h := NewResourceHandler(fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).Build(), "default", nil, "", nil)
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"alice","runtime":"qwenpaw","llmHeartbeatEnabled":true}`))
	rec := httptest.NewRecorder()
	h.CreateWorker(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestNativeConfigCannotOverrideHeartbeatSwitch(t *testing.T) {
	h := &MemberConfigHandler{}
	current := map[string]any{"agents": map[string]any{"defaults": map[string]any{"heartbeat": map[string]any{"every": "0m"}}}}
	read := func(context.Context) (map[string]any, error) { return current, nil }
	next := map[string]any{"agents": map[string]any{"defaults": map[string]any{"heartbeat": map[string]any{"every": "30m"}}}}
	for _, caller := range []*authpkg.CallerIdentity{nil, {Role: authpkg.RoleAdmin}, {Role: authpkg.RoleManager}} {
		if err := h.CheckNativeWrite(context.Background(), caller, "alice", "openclaw", next, read); err == nil {
			t.Fatal("native config bypassed the fixed switch")
		}
	}
	if err := h.CheckNativeWrite(context.Background(), nil, "alice", "openclaw", current, read); err != nil {
		t.Fatal(err)
	}
}
