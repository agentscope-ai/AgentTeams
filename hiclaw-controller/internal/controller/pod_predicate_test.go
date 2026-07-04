package controller

import (
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestPodLifecyclePredicates_FiltersByKindAndControllerLabel locks in the
// two-key gate on Pod events: a pod must carry both the kind label (e.g.
// agentteams.io/worker) AND agentteams.io/controller matching this reconciler's
// ControllerName. Pods missing either label (or carrying a different
// controller name) must NOT trigger reconciliation — this prevents two
// hiclaw-controller instances sharing a namespace from cross-reconciling
// each other's Pods when the informer cache scoping is bypassed (defense-
// in-depth against a future watch source added without
// opts.Cache.ByObject scoping).
func TestPodLifecyclePredicates_FiltersByKindAndControllerLabel(t *testing.T) {
	const (
		kindKey  = "agentteams.io/worker"
		ctlName  = "ctl-a"
		otherCtl = "ctl-b"
	)

	pod := func(labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
	}

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "matching kind + matching controller -> accept",
			labels: map[string]string{kindKey: "alice", v1beta1.LabelController: ctlName},
			want:   true,
		},
		{
			name:   "matching kind + different controller -> reject",
			labels: map[string]string{kindKey: "alice", v1beta1.LabelController: otherCtl},
			want:   false,
		},
		{
			name:   "matching kind + missing controller label -> reject",
			labels: map[string]string{kindKey: "alice"},
			want:   false,
		},
		{
			name:   "missing kind label + matching controller -> reject",
			labels: map[string]string{v1beta1.LabelController: ctlName},
			want:   false,
		},
		{
			name:   "empty labels -> reject",
			labels: nil,
			want:   false,
		},
	}

	pred := podLifecyclePredicates(kindKey, ctlName)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := pod(tc.labels)
			if got := pred.Create(event.CreateEvent{Object: p}); got != tc.want {
				t.Errorf("Create: got %v, want %v", got, tc.want)
			}
			if got := pred.Delete(event.DeleteEvent{Object: p}); got != tc.want {
				t.Errorf("Delete: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPodLifecyclePredicates_UpdateRequiresPhaseChange verifies that Update
// events on a matching Pod only enqueue when Status.Phase changed, and
// still reject updates on pods belonging to another controller regardless
// of phase transitions.
func TestPodLifecyclePredicates_UpdateRequiresPhaseChange(t *testing.T) {
	const (
		kindKey = "agentteams.io/worker"
		ctlName = "ctl-a"
	)

	mine := map[string]string{kindKey: "alice", v1beta1.LabelController: ctlName}
	other := map[string]string{kindKey: "alice", v1beta1.LabelController: "ctl-b"}

	mk := func(labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Status:     corev1.PodStatus{Phase: phase},
		}
	}

	pred := podLifecyclePredicates(kindKey, ctlName)

	tests := []struct {
		name string
		old  *corev1.Pod
		new  *corev1.Pod
		want bool
	}{
		{
			name: "mine, phase changed -> accept",
			old:  mk(mine, corev1.PodPending),
			new:  mk(mine, corev1.PodRunning),
			want: true,
		},
		{
			name: "mine, phase unchanged -> reject",
			old:  mk(mine, corev1.PodRunning),
			new:  mk(mine, corev1.PodRunning),
			want: false,
		},
		{
			name: "other controller, phase changed -> reject",
			old:  mk(other, corev1.PodPending),
			new:  mk(other, corev1.PodRunning),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pred.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			if got != tc.want {
				t.Errorf("Update: got %v, want %v", got, tc.want)
			}
		})
	}
}
