package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// QwenPaw 2.2.1 ignores active_model in model-settings, even with HTTP 200.
// Only its scoped models/active endpoint persists the primary model.
func TestActiveModelHotUsesNativeContractAndReadback(t *testing.T) {
	for _, staleRead := range []bool{false, true} {
		name := "applied"
		if staleRead {
			name = "ignored_write"
		}
		t.Run(name, func(t *testing.T) {
			desired := map[string]string{"provider_id": "agentteams-gateway", "model": "new-model"}
			current := map[string]string{"provider_id": "old-gw", "model": "old-model"}
			writes, reads := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPatch && r.URL.Path == "/api/agents/default/model-settings":
					_ = json.NewEncoder(w).Encode(map[string]any{"active_model": current})
				case r.Method == http.MethodPut && r.URL.Path == "/api/models/active":
					writes++
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["scope"] != "agent" || body["agent_id"] != "default" || body["provider_id"] != desired["provider_id"] || body["model"] != desired["model"] {
						t.Errorf("wrong scoped write: %v", body)
					}
					if !staleRead {
						current = desired
					}
					// A successful write response alone is not proof of persistence.
					_ = json.NewEncoder(w).Encode(map[string]any{"active_llm": desired})
				case r.Method == http.MethodGet && r.URL.Path == "/api/models/active":
					reads++
					if r.URL.Query().Get("scope") != "agent" || r.URL.Query().Get("agent_id") != "default" {
						t.Errorf("wrong read scope: %s", r.URL.RawQuery)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"active_llm": current})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			scheme := newControllerTestScheme(t)
			worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "native-primary", Namespace: "default"}}
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker.DeepCopy()).Build()
			reconciler := &WorkerReconciler{Client: k8sClient, KubeMode: "embedded", subagentModelURLFor: func(*v1beta1.Worker, v1beta1.WorkerSpec) string { return srv.URL }}
			reconciler.applyActiveModelHot(context.Background(), worker, v1beta1.WorkerSpec{Model: desired["model"], ModelProvider: "custom-gw"}, &MemberState{ContainerState: "running"})
			var got v1beta1.Worker
			if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: worker.Name, Namespace: worker.Namespace}, &got); err != nil {
				t.Fatal(err)
			}
			if writes != 1 || reads != 1 {
				t.Errorf("native writes=%d reads=%d, want 1 each", writes, reads)
			}
			if staleRead {
				if got.Annotations[annotationActiveModelApplied] != "" {
					t.Fatalf("ignored update falsely marked applied: %v", got.Annotations)
				}
			} else {
				if current["model"] != desired["model"] || got.Annotations[annotationActiveModelApplied] != desired["model"] {
					t.Fatalf("model not applied: current=%v annotations=%v", current, got.Annotations)
				}
			}
		})
	}
}

func TestActiveModelHotReappliesProviderChange(t *testing.T) {
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"active_llm": map[string]string{"provider_id": "agentteams-gateway", "model": "same-model"}})
	}))
	defer srv.Close()
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "changed-provider", Namespace: "default", Annotations: map[string]string{annotationActiveModelApplied: "same-model", annotationActiveModelProviderApplied: "old-gw"}}}
	reconciler := &WorkerReconciler{Client: fake.NewClientBuilder().WithScheme(newControllerTestScheme(t)).WithObjects(worker.DeepCopy()).Build(), KubeMode: "embedded", subagentModelURLFor: func(*v1beta1.Worker, v1beta1.WorkerSpec) string { return srv.URL }}
	spec := v1beta1.WorkerSpec{Model: "same-model", ModelProvider: "new-gw"}
	state := &MemberState{ContainerState: "running"}
	reconciler.applyActiveModelHot(context.Background(), worker, spec, state)
	if writes != 1 || worker.Annotations[annotationActiveModelProviderApplied] != "agentteams-gateway" {
		t.Fatalf("legacy provider annotation was not normalized: writes=%d annotations=%v", writes, worker.Annotations)
	}
	reconciler.applyActiveModelHot(context.Background(), worker, spec, state)
	if writes != 1 {
		t.Fatalf("unchanged provider/model reapplied: writes=%d", writes)
	}
}

func TestSubagentModelHotReappliesProviderChange(t *testing.T) {
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes++
		var body struct {
			SubagentModel struct {
				ProviderID string `json:"provider_id"`
			} `json:"subagent_model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.SubagentModel.ProviderID != "agentteams-gateway" {
			t.Errorf("wrong provider: %s", body.SubagentModel.ProviderID)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "changed-subagent-provider", Namespace: "default", Annotations: map[string]string{annotationSubagentModelApplied: "same-model", "agentteams.io/subagent-model-provider-applied": "old-gw"}}}
	reconciler := &WorkerReconciler{Client: fake.NewClientBuilder().WithScheme(newControllerTestScheme(t)).WithObjects(worker.DeepCopy()).Build(), KubeMode: "embedded", subagentModelURLFor: func(*v1beta1.Worker, v1beta1.WorkerSpec) string { return srv.URL }}
	spec := v1beta1.WorkerSpec{SubagentModel: "same-model", ModelProvider: "new-gw"}
	state := &MemberState{ContainerState: "running"}
	reconciler.applySubagentModelHot(context.Background(), worker, spec, MemberContext{}, state)
	if writes != 1 {
		t.Fatalf("provider-only subagent change skipped: writes=%d", writes)
	}
	reconciler.applySubagentModelHot(context.Background(), worker, spec, MemberContext{}, state)
	if writes != 1 {
		t.Fatalf("unchanged subagent slot reapplied: writes=%d", writes)
	}
}
