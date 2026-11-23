package controller

import (
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

func TestManagerEnvReachesContainerAndChangesRuntimeHash(t *testing.T) {
	mgr := &v1beta1.Manager{Spec: v1beta1.ManagerSpec{Env: map[string]string{"CUSTOM_API_KEY": "literal value"}}}
	mgr.Name = "default"
	before := hashAppliedManagerSpec(mgr.Spec)
	req := captureManagerCreateRequest(t, mgr, nil)
	if req.Env["CUSTOM_API_KEY"] != "literal value" {
		t.Fatal("Manager custom environment did not reach backend create request")
	}
	mgr.Spec.Env["CUSTOM_API_KEY"] = "changed"
	if before == hashAppliedManagerSpec(mgr.Spec) {
		t.Fatal("Manager environment updates must cause runtime reconciliation")
	}
}
