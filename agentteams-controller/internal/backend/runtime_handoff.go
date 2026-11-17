package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const RuntimeHandoffLabel = "agentteams.io/runtime-handoff-id"
const RuntimeHandoffSpecLabel = "agentteams.io/runtime-handoff-spec"
const runtimeStopFinalizer = "agentteams.io/confirm-runtime-stop"

// RuntimeHandoffSource stops the original backend resource and observes its
// termination before removing it. Both calls can be retried after a crash.
type RuntimeHandoffSource interface {
	HandoffTargetImage(runtime string) string
	StopRuntimeInstance(ctx context.Context, name, instance string) (stopped bool, err error)
	RemoveStoppedRuntime(ctx context.Context, name, instance string) (absent bool, err error)
}

func (d *DockerBackend) StopRuntimeInstance(ctx context.Context, name, instance string) (bool, error) {
	id, started, ok := strings.Cut(instance, "/")
	if !ok || id == "" || started == "" {
		return false, errors.New("invalid Docker start identity")
	}
	status, err := d.Status(ctx, name)
	if err != nil {
		return false, err
	}
	if status.Status == StatusNotFound {
		return d.dockerInstanceGone(ctx, id)
	}
	if status.InstanceID != instance || status.ContainerID != id {
		return false, errors.New("Docker Worker start changed before stop")
	}
	if status.RawStatus == "exited" || status.RawStatus == "dead" {
		return true, nil
	}
	code, err := d.dockerInstanceRequest(ctx, http.MethodPost, id, "/stop?t=10")
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusNoContent, http.StatusNotModified, http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("stop source container %s: HTTP %d", id, code)
	}
}

func (d *DockerBackend) dockerInstanceGone(ctx context.Context, id string) (bool, error) {
	code, err := d.dockerInstanceRequest(ctx, http.MethodGet, id, "/json")
	if err != nil {
		return false, err
	}
	if code == http.StatusNotFound {
		return true, nil
	}
	if code != http.StatusOK {
		return false, fmt.Errorf("inspect source container %s: HTTP %d", id, code)
	}
	return false, errors.New("source container still exists under another name")
}

func (d *DockerBackend) RemoveStoppedRuntime(ctx context.Context, name, instance string) (bool, error) {
	id, started, ok := strings.Cut(instance, "/")
	if !ok || id == "" || started == "" {
		return false, errors.New("invalid Docker start identity")
	}
	status, err := d.Status(ctx, name)
	if err != nil {
		return false, err
	}
	if status.Status == StatusNotFound {
		return d.dockerInstanceGone(ctx, id)
	}
	if status.InstanceID != instance || status.ContainerID != id || (status.RawStatus != "exited" && status.RawStatus != "dead") {
		return false, errors.New("source changed or resumed before removal")
	}
	// Docker refuses a non-force removal if the source restarted since inspect.
	code, err := d.dockerInstanceRequest(ctx, http.MethodDelete, id, "")
	if err != nil {
		return false, err
	}
	if code != http.StatusNoContent && code != http.StatusNotFound {
		return false, fmt.Errorf("remove stopped container: HTTP %d", code)
	}
	return false, nil
}

func (d *DockerBackend) dockerInstanceRequest(ctx context.Context, method, id, suffix string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost/containers/"+url.PathEscape(id)+suffix, nil)
	if err != nil {
		return 0, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

type handoffPodPatcher interface {
	Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*corev1.Pod, error)
}

func patchStopFinalizer(ctx context.Context, pods K8sPodClient, pod *corev1.Pod, finalizers []string) (*corev1.Pod, error) {
	patcher, ok := pods.(handoffPodPatcher)
	if !ok {
		return nil, errors.New("Pod client does not support conditional finalizer updates")
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": pod.UID, "resourceVersion": pod.ResourceVersion, "finalizers": finalizers}})
	return patcher.Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{})
}

func (k *K8sBackend) StopRuntimeInstance(ctx context.Context, name, instance string) (bool, error) {
	target, ns, err := k.resolveClient(ctx)
	if err != nil {
		return false, err
	}
	pods := target.Pods(ns)
	pod, err := pods.Get(ctx, k.workerPodName(name), metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("no recorded kubelet termination for source Pod: %w", err)
	}
	uid, _, ok := strings.Cut(instance, "/")
	if !ok || uid == "" || string(pod.UID) != uid {
		return false, errors.New("source Pod identity changed")
	}
	if pod.DeletionTimestamp != nil {
		if !slices.Contains(pod.Finalizers, runtimeStopFinalizer) {
			return false, errors.New("source Pod is deleting without termination observation")
		}
		return handoffPodTerminated(pod, instance), nil
	}
	if podInstanceID(pod) != instance {
		return false, errors.New("source Pod container start changed")
	}
	if !slices.Contains(pod.Finalizers, runtimeStopFinalizer) {
		pod, err = patchStopFinalizer(ctx, pods, pod, append(pod.Finalizers, runtimeStopFinalizer))
		if err != nil {
			return false, err
		}
	}
	// Neither forced deletion nor the disappearance of an unreachable Pod
	// proves its container processes have exited. Hold the API object until
	// kubelet publishes terminal container state.
	err = pods.Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}})
	return false, err
}

func handoffPodTerminated(pod *corev1.Pod, instance string) bool {
	if pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
		return false
	}
	workerExited := false
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses, pod.Status.EphemeralContainerStatuses} {
		for _, container := range statuses {
			terminated := container.State.Terminated
			if terminated == nil || terminated.FinishedAt.IsZero() || terminated.Reason == "ContainerStatusUnknown" {
				return false
			}
			if container.Name == "worker" {
				workerExited = string(pod.UID)+"/"+container.ContainerID+"/"+terminated.StartedAt.UTC().Format(time.RFC3339Nano) == instance
			}
		}
	}
	return workerExited
}

func (k *K8sBackend) RemoveStoppedRuntime(ctx context.Context, name, instance string) (bool, error) {
	target, ns, err := k.resolveClient(ctx)
	if err != nil {
		return false, err
	}
	pods := target.Pods(ns)
	pod, err := pods.Get(ctx, k.workerPodName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	uid, _, ok := strings.Cut(instance, "/")
	if !ok || string(pod.UID) != uid || pod.DeletionTimestamp == nil || !handoffPodTerminated(pod, instance) {
		return false, errors.New("source Pod changed or lacks terminal status")
	}
	if slices.Contains(pod.Finalizers, runtimeStopFinalizer) {
		_, err = patchStopFinalizer(ctx, pods, pod, slices.DeleteFunc(pod.Finalizers, func(s string) bool { return s == runtimeStopFinalizer }))
	}
	return false, err
}

// The switch only selects an image explicitly associated with the target
// Harness. A missing image must be detected before stopping the source.
func (d *DockerBackend) HandoffTargetImage(runtime string) string {
	return map[string]string{RuntimeOpenClaw: d.config.WorkerImage, RuntimeHermes: d.config.HermesWorkerImage, RuntimeCodex: d.config.CodexWorkerImage, RuntimeQwenPaw: d.config.QwenPawWorkerImage, RuntimeDeepSeekHarness: d.config.DeepSeekHarnessWorkerImage, RuntimeOpenHuman: d.config.OpenHumanWorkerImage}[runtime]
}
func (k *K8sBackend) HandoffTargetImage(runtime string) string {
	return map[string]string{RuntimeOpenClaw: k.config.WorkerImage, RuntimeHermes: k.config.HermesWorkerImage, RuntimeCodex: k.config.CodexWorkerImage, RuntimeQwenPaw: k.config.QwenPawWorkerImage, RuntimeDeepSeekHarness: k.config.DeepSeekHarnessWorkerImage, RuntimeOpenHuman: k.config.OpenHumanWorkerImage}[runtime]
}
