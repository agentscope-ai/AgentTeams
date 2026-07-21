package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestSandboxLifecyclePredicates locks in the update-event semantics for
// Sandbox CR watches: the predicate must trigger reconcile when either
// .status.phase changes OR the Ready condition status flips, while
// remaining inert for unrelated changes (other condition types, identical
// objects, missing label).
func TestSandboxLifecyclePredicates(t *testing.T) {
	const (
		labelKey       = "agentteams.io/worker"
		controllerName = ""
	)

	// mk builds an unstructured Sandbox-like object with the given label,
	// phase and conditions. Pass labelValue="" to simulate a CR without
	// the worker label (used by the "label not matching" case).
	mk := func(labelValue, phase string, conditions []interface{}) *unstructured.Unstructured {
		labels := map[string]interface{}{}
		if labelValue != "" {
			labels[labelKey] = labelValue
		}
		status := map[string]interface{}{}
		if phase != "" {
			status["phase"] = phase
		}
		if conditions != nil {
			status["conditions"] = conditions
		}
		return &unstructured.Unstructured{
			Object: map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": labels,
				},
				"status": status,
			},
		}
	}

	cond := func(condType, condStatus string) map[string]interface{} {
		return map[string]interface{}{
			"type":   condType,
			"status": condStatus,
		}
	}

	readyTrue := []interface{}{cond("Ready", "True")}
	readyFalse := []interface{}{cond("Ready", "False")}

	tests := []struct {
		name string
		old  *unstructured.Unstructured
		new  *unstructured.Unstructured
		want bool
	}{
		{
			name: "phase change Running->Failed triggers reconcile",
			old:  mk("alice", "Running", readyTrue),
			new:  mk("alice", "Failed", readyTrue),
			want: true,
		},
		{
			name: "phase unchanged, Ready True->False triggers reconcile",
			old:  mk("alice", "Running", readyTrue),
			new:  mk("alice", "Running", readyFalse),
			want: true,
		},
		{
			name: "phase unchanged, Ready False->True triggers reconcile",
			old:  mk("alice", "Running", readyFalse),
			new:  mk("alice", "Running", readyTrue),
			want: true,
		},
		{
			name: "phase unchanged, Ready unchanged, other condition changes -> no reconcile",
			old: mk("alice", "Running", []interface{}{
				cond("Ready", "True"),
				cond("Initialized", "False"),
			}),
			new: mk("alice", "Running", []interface{}{
				cond("Ready", "True"),
				cond("Initialized", "True"),
			}),
			want: false,
		},
		{
			name: "phase and Ready both unchanged -> no reconcile",
			old:  mk("alice", "Running", readyTrue),
			new:  mk("alice", "Running", readyTrue),
			want: false,
		},
		{
			name: "no conditions on either side -> no reconcile",
			old:  mk("alice", "Running", nil),
			new:  mk("alice", "Running", nil),
			want: false,
		},
		{
			name: "label not matching -> no reconcile even if phase changes",
			old:  mk("", "Running", readyTrue),
			new:  mk("", "Failed", readyFalse),
			want: false,
		},
	}

	pred := SandboxLifecyclePredicates(labelKey, controllerName)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pred.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			if got != tc.want {
				t.Errorf("Update: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPodLifecyclePredicates(t *testing.T) {
	const (
		labelKey       = "agentteams.io/worker"
		controllerName = ""
	)

	mk := func(labelValue string, phase corev1.PodPhase, ready corev1.ConditionStatus, waitingReason, waitingMessage string) *corev1.Pod {
		labels := map[string]string{}
		if labelValue != "" {
			labels[labelKey] = labelValue
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Status: corev1.PodStatus{
				Phase: phase,
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodReady,
					Status: ready,
				}},
			},
		}
		if waitingReason != "" || waitingMessage != "" {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "worker",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  waitingReason,
						Message: waitingMessage,
					},
				},
			}}
		}
		return pod
	}

	tests := []struct {
		name string
		old  *corev1.Pod
		new  *corev1.Pod
		want bool
	}{
		{
			name: "phase unchanged, waiting reason changes trigger reconcile",
			old:  mk("alice", corev1.PodPending, corev1.ConditionFalse, "ContainerCreating", ""),
			new:  mk("alice", corev1.PodPending, corev1.ConditionFalse, "ImagePullBackOff", "image not found"),
			want: true,
		},
		{
			name: "phase unchanged, Ready status changes trigger reconcile",
			old:  mk("alice", corev1.PodRunning, corev1.ConditionTrue, "", ""),
			new:  mk("alice", corev1.PodRunning, corev1.ConditionFalse, "", ""),
			want: true,
		},
		{
			name: "status signal unchanged does not trigger reconcile",
			old:  mk("alice", corev1.PodPending, corev1.ConditionFalse, "ImagePullBackOff", "image not found"),
			new:  mk("alice", corev1.PodPending, corev1.ConditionFalse, "ImagePullBackOff", "image not found"),
			want: false,
		},
		{
			name: "label not matching blocks reconcile",
			old:  mk("", corev1.PodPending, corev1.ConditionFalse, "ContainerCreating", ""),
			new:  mk("", corev1.PodPending, corev1.ConditionFalse, "ImagePullBackOff", "image not found"),
			want: false,
		},
	}

	pred := PodLifecyclePredicates(labelKey, controllerName)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pred.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			if got != tc.want {
				t.Errorf("Update: got %v, want %v", got, tc.want)
			}
		})
	}
}
