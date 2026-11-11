package server

import (
	"context"
	"net/http"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"k8s.io/apimachinery/pkg/types"
)

func TestUpdateWorkerModelProviderSetOmitClear(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	for _, tc := range []struct {
		name, body, want string
	}{
		{"set", `{"modelProvider":"deepseek"}`, "deepseek"},
		{"omit", `{}`, "deepseek"},
		{"null", `{"modelProvider":null}`, "deepseek"},
		{"clear", `{"modelProvider":""}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := l2UpdateRequest(t, handler, "alpha-dev", tc.body, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
			}
			var worker v1beta1.Worker
			if err := handler.client.Get(context.Background(), types.NamespacedName{Name: "alpha-dev", Namespace: "default"}, &worker); err != nil {
				t.Fatal(err)
			}
			if worker.Spec.ModelProvider != tc.want {
				t.Fatalf("modelProvider=%q, want %q", worker.Spec.ModelProvider, tc.want)
			}
		})
	}
}

func TestUpdateWorkerModelProviderClearRequiresModelPair(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}
	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"modelProvider":""}`, caller)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateWorkerModelProviderClearChecksGrants(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		status      int
	}{
		{"granted", "granted-m", http.StatusOK},
		{"ungranted", "ungranted-m", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := grantRig(t)
			ctx := context.Background()
			var human v1beta1.Human
			humanKey := types.NamespacedName{Name: "alice", Namespace: "default"}
			if err := handler.client.Get(ctx, humanKey, &human); err != nil {
				t.Fatal(err)
			}
			human.Spec.ModelAccess.Scopes[0].Main.Entries[0].Provider = modelGrantProviderOr("")
			if err := handler.client.Update(ctx, &human); err != nil {
				t.Fatal(err)
			}
			key := types.NamespacedName{Name: "alpha-dev", Namespace: "default"}
			var worker v1beta1.Worker
			if err := handler.client.Get(ctx, key, &worker); err != nil {
				t.Fatal(err)
			}
			worker.Spec.ModelProvider = "deleted-provider"
			if err := handler.client.Update(ctx, &worker); err != nil {
				t.Fatal(err)
			}
			rec := l2UpdateRequest(t, handler, "alpha-dev", `{"modelProvider":"","model":"`+tc.model+`"}`, grantCaller())
			if rec.Code != tc.status {
				t.Fatalf("status=%d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if err := handler.client.Get(ctx, key, &worker); err != nil {
				t.Fatal(err)
			}
			want := "deleted-provider"
			if tc.status == http.StatusOK {
				want = ""
			}
			if worker.Spec.ModelProvider != want {
				t.Fatalf("modelProvider=%q, want %q", worker.Spec.ModelProvider, want)
			}
		})
	}
}
