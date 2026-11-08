package server

import (
	"context"
	"net/http"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestModelGrantRevocationRejectsHistoryAndRetainsCurrent(t *testing.T) {
	for _, removeScope := range []bool{false, true} {
		h := grantRig(t)
		ctx := context.Background()
		var worker v1beta1.Worker
		key := client.ObjectKey{Name: "alpha-dev", Namespace: "default"}
		if err := h.client.Get(ctx, key, &worker); err != nil {
			t.Fatal(err)
		}
		worker.Spec.Model, worker.Spec.ModelProvider = "current", "gw"
		worker.Annotations = map[string]string{modelGrantAnnotation: `[{"provider":"gw","model":"granted-m"},{"provider":"gw","model":"current"}]`}
		if err := h.client.Update(ctx, &worker); err != nil {
			t.Fatal(err)
		}
		var human v1beta1.Human
		if err := h.client.Get(ctx, client.ObjectKey{Name: "alice", Namespace: "default"}, &human); err != nil {
			t.Fatal(err)
		}
		if removeScope {
			human.Spec.ModelAccess = nil
		} else {
			human.Spec.ModelAccess.Scopes[0].Main.Entries = nil
		}
		if err := h.client.Update(ctx, &human); err != nil {
			t.Fatal(err)
		}
		if rec := l2UpdateRequest(t, h, "alpha-dev", `{"model":"current","modelProvider":"gw"}`, grantCaller()); rec.Code != http.StatusOK {
			t.Fatalf("current must remain usable: %d %s", rec.Code, rec.Body)
		}
		if rec := l2UpdateRequest(t, h, "alpha-dev", `{"model":"granted-m","modelProvider":"gw"}`, grantCaller()); rec.Code != http.StatusForbidden {
			t.Fatalf("removedScope=%v revoked history accepted: %d %s", removeScope, rec.Code, rec.Body)
		}
		if err := h.client.Get(ctx, key, &worker); err != nil {
			t.Fatal(err)
		}
		if worker.Spec.Model != "current" {
			t.Fatal("denied historical selection changed Worker")
		}
		set, err := buildModelGrantSet(&v1beta1.ModelAccessScope{CodingCli: []v1beta1.ModelAccessCli{{CLI: "opencode"}}}, nil, &worker)
		if err != nil {
			t.Fatal(err)
		}
		if grantAllows(set.main, "gw", "granted-m") || grantAllows(set.spawn, "gw", "granted-m") || grantAllows(set.cliSet("opencode").entries, "gw", "granted-m") {
			t.Fatal("revoked annotation leaked into an effective surface")
		}
	}
}

func TestModelGrantRevocationCodingCLIDoesNotResurrectHistory(t *testing.T) {
	h, eb := cliGrantRig(t, v1beta1.ModelAccessCli{CLI: "qwen-code"})
	var worker v1beta1.Worker
	ctx := context.Background()
	if err := h.client.Get(ctx, client.ObjectKey{Name: "daily-carol", Namespace: "default"}, &worker); err != nil {
		t.Fatal(err)
	}
	worker.Annotations = map[string]string{modelGrantAnnotation: `[{"provider":"gw","model":"revoked"}]`}
	if err := h.client.Update(ctx, &worker); err != nil {
		t.Fatal(err)
	}
	if rec := cliPut(t, h, `{"model":"revoked"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("revoked CLI model accepted: %d %s", rec.Code, rec.Body)
	}
	if len(eb.fileWrites) != 0 {
		t.Fatal("denied CLI model changed settings")
	}
	if rec := cliPut(t, h, `{"model":"cur-m"}`); rec.Code != http.StatusOK {
		t.Fatalf("current CLI model locked: %d %s", rec.Code, rec.Body)
	}
}

func TestModelGrantProviderChangeRechecksExistingSubagent(t *testing.T) {
	h := grantRig(t)
	ctx := context.Background()
	var worker v1beta1.Worker
	key := client.ObjectKey{Name: "alpha-dev", Namespace: "default"}
	if err := h.client.Get(ctx, key, &worker); err != nil {
		t.Fatal(err)
	}
	worker.Spec.SubagentModel = "spawn-current"
	if err := h.client.Update(ctx, &worker); err != nil {
		t.Fatal(err)
	}
	// Main gw/granted-m is granted, but the existing subagent was authorized
	// only on the default gateway. It cannot silently move to gw with main.
	if rec := l2UpdateRequest(t, h, "alpha-dev", `{"model":"granted-m","modelProvider":"gw"}`, grantCaller()); rec.Code != http.StatusForbidden {
		t.Fatalf("subagent provider bypass: %d %s", rec.Code, rec.Body)
	}
	if err := h.client.Get(ctx, key, &worker); err != nil {
		t.Fatal(err)
	}
	if worker.Spec.ModelProvider != "" {
		t.Fatal("denied provider change mutated worker")
	}
}

func TestModelGrantNamedProviderDoesNotAuthorizeDefaultGateway(t *testing.T) {
	h := grantRig(t)
	if rec := l2UpdateRequest(t, h, "alpha-dev", `{"model":"granted-m"}`, grantCaller()); rec.Code != http.StatusForbidden {
		t.Fatalf("named provider grant admitted default gateway: %d %s", rec.Code, rec.Body)
	}
}
