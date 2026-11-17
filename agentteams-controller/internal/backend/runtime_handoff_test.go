package backend

import (
	"context"
	"encoding/json"
	"fmt"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRuntimeHandoffConflictNeverDeletesForeignDockerContainer(t *testing.T) {
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = true
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)
	if _, err := b.doCreate(t.Context(), "member", []byte("{}"), false); err == nil || deleted {
		t.Fatalf("name collision deleted a container: delete=%v err=%v", deleted, err)
	}
	labels := b.buildCreatePayload(CreateRequest{Runtime: "hermes", Labels: map[string]string{RuntimeHandoffLabel: "op"}}, "", 0).Labels
	if labels[RuntimeHandoffLabel] != "op" {
		t.Fatal("replacement missing handoff label")
	}
}

func TestRuntimeHandoffPodNeedsTerminalWorkerAndChildren(t *testing.T) {
	now := metav1.Now()
	uid := "source"
	id := uid + "/containerd://one/" + now.UTC().Format(time.RFC3339Nano)
	exited := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: now, FinishedAt: now, Reason: "Completed"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "source"}, Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{Name: "worker", ContainerID: "containerd://one", State: exited}, {Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now}}}}}}
	if handoffPodTerminated(pod, id) {
		t.Fatal("running sidecar admitted stop confirmation")
	}
	pod.Status.ContainerStatuses[1].State = exited
	if !handoffPodTerminated(pod, id) || handoffPodTerminated(pod, uid+"/containerd://other/start") {
		t.Fatal("confirmation did not bind the original process")
	}
	pod.Status.ContainerStatuses[0].State.Terminated.Reason = "ContainerStatusUnknown"
	if handoffPodTerminated(pod, id) {
		t.Fatal("node loss declared a stopped process")
	}
}

func TestRuntimeHandoffLiveDockerStopsToolChildBeforeNameReuse(t *testing.T) {
	socket, image := os.Getenv("AGENTTEAMS_HANDOFF_TEST_SOCKET"), os.Getenv("AGENTTEAMS_HANDOFF_TEST_IMAGE")
	if socket == "" || image == "" {
		t.Skip("set AGENTTEAMS_HANDOFF_TEST_SOCKET and AGENTTEAMS_HANDOFF_TEST_IMAGE to run with a disposable local container")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	name := fmt.Sprintf("at-test-1631-stop-%d", time.Now().UnixNano())
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", append([]string{"--host", "unix://" + socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %s %v", args[0], out, err)
		}
		return strings.TrimSpace(string(out))
	}
	start := func(runtime string) string {
		t.Helper()
		id := run("run", "--pull=never", "--detach", "--name", name, "--label", "agentteams.io/runtime="+runtime, "--entrypoint", "sh", image, "-c", "trap 'exit 0' TERM; sleep 120 & wait")
		t.Cleanup(func() { _ = exec.Command("docker", "--host", "unix://"+socket, "rm", "--force", id).Run() })
		return id
	}
	first := start("hermes")
	b := NewDockerBackend(DockerConfig{SocketPath: socket}, "")
	before, err := b.Status(ctx, name)
	if err != nil || before.ContainerID != first {
		t.Fatalf("source %v %v", before, err)
	}
	if top := run("top", first, "-eo", "pid,args"); !regexp.MustCompile(`(?m)^\s*[0-9]+\s+sleep 120\s*$`).MatchString(top) {
		t.Fatalf("tool child missing: %s", top)
	}
	if stopped, err := b.StopRuntimeInstance(ctx, name, before.InstanceID); err != nil || stopped {
		t.Fatalf("stop request %v %v", stopped, err)
	}
	if stopped, err := b.StopRuntimeInstance(ctx, name, before.InstanceID); err != nil || !stopped {
		t.Fatalf("stop observation %v %v", stopped, err)
	}
	if tasks := run("inspect", "--format", "{{.State.Pid}} {{.State.Status}}", first); tasks != "0 exited" {
		t.Fatal(tasks)
	}
	if gone, err := b.RemoveStoppedRuntime(ctx, name, before.InstanceID); err != nil || gone {
		t.Fatalf("delete request %v %v", gone, err)
	}
	if gone, err := b.RemoveStoppedRuntime(ctx, name, before.InstanceID); err != nil || !gone {
		t.Fatalf("deletion observation %v %v", gone, err)
	}
	second := start("codex")
	if _, err := b.RemoveStoppedRuntime(ctx, name, before.InstanceID); err == nil {
		t.Fatal("stale deletion accepted after name reuse")
	}
	after, err := b.Status(ctx, name)
	if err != nil || after.ContainerID != second || after.Runtime != "codex" || after.Status != StatusRunning {
		t.Fatalf("target %v %v", after, err)
	}
	t.Logf("source=%s source_pid_after_stop=0 target=%s target_runtime=%s", before.InstanceID, after.InstanceID, after.Runtime)
}

func TestRuntimeHandoffK8sUsesUIDAndWaitsForKubeletBeforeRemovingFinalizer(t *testing.T) {
	ctx := t.Context()
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	pod := &corev1.Pod{TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "worker-alice", Namespace: "default", UID: "source", ResourceVersion: "9", Finalizers: []string{"example.test/keep"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "worker", ContainerID: "containerd://source", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now}}}}}}
	id := podInstanceID(pod)
	var deleted, absent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/namespaces/default/pods/worker-alice" {
			t.Errorf("wrong pod path %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if absent {
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
				return
			}
		case http.MethodPatch:
			var patch struct {
				Metadata metav1.ObjectMeta `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil || patch.Metadata.UID != pod.UID || patch.Metadata.ResourceVersion != pod.ResourceVersion {
				t.Errorf("unconditional finalizer patch %+v %v", patch, err)
				w.WriteHeader(409)
				return
			}
			pod.Finalizers = patch.Metadata.Finalizers
			pod.ResourceVersion = "10"
		case http.MethodDelete:
			var options metav1.DeleteOptions
			if err := json.NewDecoder(r.Body).Decode(&options); err != nil || options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != pod.UID || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != pod.ResourceVersion || options.GracePeriodSeconds != nil {
				t.Errorf("unconditional/forced delete %+v %v", options, err)
				w.WriteHeader(409)
				return
			}
			pod.DeletionTimestamp = &now
			deleted = true
		default:
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(pod)
	}))
	defer srv.Close()
	client, err := coreclient.NewForConfig(&rest.Config{Host: srv.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	b := NewK8sBackendWithClient(&k8sCoreClientWrapper{client: client}, K8sConfig{Namespace: "default"}, "worker-", nil)
	if _, err = b.StopRuntimeInstance(ctx, "alice", "other/instance"); err == nil || deleted {
		t.Fatal("deleted a different Pod UID")
	}
	if stopped, err := b.StopRuntimeInstance(ctx, "alice", id); err != nil || stopped || !deleted || !slices.Contains(pod.Finalizers, runtimeStopFinalizer) {
		t.Fatalf("source stop order %v %+v %v", stopped, pod, err)
	}
	if stopped, err := b.StopRuntimeInstance(ctx, "alice", id); err != nil || stopped {
		t.Fatalf("API deletion was counted as exit %v %v", stopped, err)
	}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: now, FinishedAt: now, Reason: "Error"}}
	if stopped, err := b.StopRuntimeInstance(ctx, "alice", id); err != nil || !stopped {
		t.Fatalf("kubelet exit %v %v", stopped, err)
	}
	if gone, err := b.RemoveStoppedRuntime(ctx, "alice", id); err != nil || gone || !slices.Equal(pod.Finalizers, []string{"example.test/keep"}) {
		t.Fatalf("premature name reuse or foreign finalizer removal %v %+v %v", gone, pod.Finalizers, err)
	}
	absent = true
	if gone, err := b.RemoveStoppedRuntime(ctx, "alice", id); err != nil || !gone {
		t.Fatalf("expected confirmed name absence %v %v", gone, err)
	}
	if stopped, err := b.StopRuntimeInstance(ctx, "alice", id); err == nil || stopped {
		t.Fatal("a disappearing Pod without observed termination was treated as stopped")
	}
}
