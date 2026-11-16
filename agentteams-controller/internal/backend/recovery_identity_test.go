package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDockerRecoveryIdentityChangesOnSameContainerRestart(t *testing.T) {
	started := "2026-11-16T10:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "same-container", "State": map[string]any{"Status": "running", "StartedAt": started}})
	}))
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)
	first, err := b.Status(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	started = "2026-11-16T10:01:00Z"
	second, err := b.Status(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if first.InstanceID == "" || first.InstanceID == second.InstanceID || first.ContainerID != second.ContainerID {
		t.Fatalf("restart identity did not change: %+v %+v", first, second)
	}
	started = ""
	unknown, err := b.Status(context.Background(), "alice")
	if err != nil || unknown.InstanceID != "" {
		t.Fatalf("missing start proof: %+v %v", unknown, err)
	}
}

func TestPodRecoveryIdentityRequiresLiveContainerStart(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "stable-pod"}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "worker", ContainerID: "containerd://one", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Unix(1, 0))}}},
	}}}
	first := podInstanceID(pod)
	pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(time.Unix(2, 0))
	if first == "" || first == podInstanceID(pod) {
		t.Fatal("Pod container restart did not change execution identity")
	}
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	if podInstanceID(pod) != "" {
		t.Fatal("terminating Pod advertised a claimable execution identity")
	}
}
