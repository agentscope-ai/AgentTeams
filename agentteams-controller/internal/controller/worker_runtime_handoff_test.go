package controller

import (
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRuntimeHandoffReconcileRecoversInterruptedPreflightBeforeReplacement(t *testing.T) {
	store := &service.WorkerRuntimeStore{Storage: ossfake.NewMemory()}
	source := service.WorkerStart{InstanceID: "docker/one", ProcessID: "source-process", Runtime: "hermes"}
	if _, _, err := store.Preflight(t.Context(), "stable", "op", "codex", source, ""); err != nil {
		t.Fatal(err)
	}
	w := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "default", Annotations: map[string]string{service.WorkerRuntimeHandoffAnnotation: "true"}}, Spec: v1beta1.WorkerSpec{WorkerName: "stable", Runtime: "codex"}}
	// A fresh reconciler must leave the old container alone while draining.
	for i := 0; i < 2; i++ {
		r := &WorkerReconciler{RuntimeStore: store}
		result, err := r.reconcileNormal(t.Context(), w, &MemberState{})
		if err != nil || result.RequeueAfter <= 0 {
			t.Fatalf("missing durable requeue %v %v", result, err)
		}
	}
	state, _, err := store.Load(t.Context(), "stable")
	if err != nil || state.Handoff.Phase != "draining" || state.Handoff.Source != source {
		t.Fatalf("lost drain %v %v", state, err)
	}
}
