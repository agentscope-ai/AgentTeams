package v1beta1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStatusConditionsSerializeForAllResources(t *testing.T) {
	cases := []struct {
		name   string
		status any
	}{
		{
			name: "worker",
			status: WorkerStatus{
				ObservedGeneration: 7,
				Conditions: []metav1.Condition{{
					Type:               ConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             "Ready",
					Message:            "Worker is ready.",
					ObservedGeneration: 7,
				}},
			},
		},
		{
			name: "manager",
			status: ManagerStatus{
				ObservedGeneration: 7,
				Conditions: []metav1.Condition{{
					Type:               ConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             "Ready",
					Message:            "Manager is ready.",
					ObservedGeneration: 7,
				}},
			},
		},
		{
			name: "team",
			status: TeamStatus{
				ObservedGeneration: 7,
				Conditions: []metav1.Condition{{
					Type:               ConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             "Ready",
					Message:            "Team is ready.",
					ObservedGeneration: 7,
				}},
			},
		},
		{
			name: "human",
			status: HumanStatus{
				ObservedGeneration: 7,
				Conditions: []metav1.Condition{{
					Type:               ConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             "Ready",
					Message:            "Human is ready.",
					ObservedGeneration: 7,
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.status)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if _, ok := decoded["conditions"]; !ok {
				t.Fatalf("status JSON missing conditions: %s", raw)
			}
			if _, ok := decoded["observedGeneration"]; !ok {
				t.Fatalf("status JSON missing observedGeneration: %s", raw)
			}
		})
	}
}
