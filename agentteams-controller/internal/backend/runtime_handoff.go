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
		if !status.dockerProcessExited {
			return false, errors.New("Docker has not confirmed zero source processes")
		}
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
	if status.InstanceID != instance || status.ContainerID != id || !status.dockerProcessExited || (status.RawStatus != "exited" && status.RawStatus != "dead") {
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
	reported := make(map[string]bool)
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses, pod.Status.EphemeralContainerStatuses} {
		for _, container := range statuses {
			terminated := container.State.Terminated
			if terminated == nil || terminated.FinishedAt.IsZero() || terminated.Reason == "ContainerStatusUnknown" {
				return false
			}
			reported[container.Name] = true
			if container.Name == "worker" {
				workerExited = string(pod.UID)+"/"+container.ContainerID+"/"+terminated.StartedAt.UTC().Format(time.RFC3339Nano) == instance
			}
		}
	}
	for _, group := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, container := range group {
			if !reported[container.Name] {
				return false
			}
		}
	}
	for _, container := range pod.Spec.EphemeralContainers {
		if !reported[container.Name] {
			return false
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
	if !ok || string(pod.UID) != uid || !handoffPodTerminated(pod, instance) {
		return false, errors.New("source Pod changed or lacks terminal status")
	}
	if pod.DeletionTimestamp == nil {
		err := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}})
		return false, err
	}
	if slices.Contains(pod.Finalizers, runtimeStopFinalizer) {
		_, err = patchStopFinalizer(ctx, pods, pod, slices.DeleteFunc(pod.Finalizers, func(s string) bool { return s == runtimeStopFinalizer }))
	}
	return false, err
}

// The switch only selects an image explicitly associated with the target
// Harness. A missing image must be detected before stopping the source.
func (d *DockerBackend) HandoffTargetImage(runtime string) string {
	return map[string]string{RuntimeOpenClaw: d.config.WorkerImage, RuntimeHermes: d.config.HermesWorkerImage, RuntimeCodex: d.config.CodexWorkerImage, RuntimeOpenCode: d.config.OpenCodeWorkerImage, RuntimeClaudeCode: d.config.ClaudeCodeWorkerImage, RuntimeQwenPaw: d.config.QwenPawWorkerImage, RuntimeDeepSeekHarness: d.config.DeepSeekHarnessWorkerImage, RuntimeOpenHuman: d.config.OpenHumanWorkerImage}[runtime]
}
func (k *K8sBackend) HandoffTargetImage(runtime string) string {
	return map[string]string{RuntimeOpenClaw: k.config.WorkerImage, RuntimeHermes: k.config.HermesWorkerImage, RuntimeCodex: k.config.CodexWorkerImage, RuntimeOpenCode: k.config.OpenCodeWorkerImage, RuntimeClaudeCode: k.config.ClaudeCodeWorkerImage, RuntimeQwenPaw: k.config.QwenPawWorkerImage, RuntimeDeepSeekHarness: k.config.DeepSeekHarnessWorkerImage, RuntimeOpenHuman: k.config.OpenHumanWorkerImage}[runtime]
}

// RuntimeExitObservation describes read-only evidence about a source that
// disappeared before acknowledging drain. An API-level disappearance without
// process evidence keeps the operation fenced.
type RuntimeExitObservation struct {
	Exited      bool
	Unconfirmed bool
	Successor   string // another start of the same backend resource
}

type RuntimeExitObserver interface {
	ObserveRuntimeExit(context.Context, string, string) (RuntimeExitObservation, error)
}

func (d *DockerBackend) ObserveRuntimeExit(ctx context.Context, name, instance string) (RuntimeExitObservation, error) {
	var obs RuntimeExitObservation
	id, _, ok := strings.Cut(instance, "/")
	if !ok || id == "" {
		return obs, errors.New("invalid Docker start identity")
	}
	status, err := d.Status(ctx, name)
	if err != nil {
		return obs, err
	}
	if status.ContainerID != id {
		code, err := d.dockerInstanceRequest(ctx, http.MethodGet, id, "/json")
		if err != nil {
			return obs, err
		}
		switch code {
		case http.StatusNotFound:
			if status.Status == StatusNotFound {
				obs.Exited = true
			} else {
				obs.Unconfirmed = true
			} // same name now belongs to a different resource
		case http.StatusOK:
			obs.Unconfirmed = true // source was renamed; never touch the new occupant
		default:
			return obs, fmt.Errorf("inspect original Docker container: HTTP %d", code)
		}
		return obs, nil
	}
	if status.InstanceID == instance {
		obs.Exited = status.dockerProcessExited && (status.RawStatus == "exited" || status.RawStatus == "dead")
		return obs, nil
	}
	if status.InstanceID != "" && status.Status == StatusRunning {
		obs.Exited, obs.Successor = true, status.InstanceID
	} else {
		obs.Unconfirmed = true
	}
	return obs, nil
}

func (k *K8sBackend) ObserveRuntimeExit(ctx context.Context, name, instance string) (RuntimeExitObservation, error) {
	var obs RuntimeExitObservation
	target, ns, err := k.resolveClient(ctx)
	if err != nil {
		return obs, err
	}
	pod, err := target.Pods(ns).Get(ctx, k.workerPodName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		obs.Unconfirmed = true
		return obs, nil
	}
	if err != nil {
		return obs, err
	}
	uid, _, ok := strings.Cut(instance, "/")
	if !ok || string(pod.UID) != uid {
		obs.Unconfirmed = true
		return obs, nil
	}
	if handoffPodTerminated(pod, instance) {
		obs.Exited = true
		return obs, nil
	}
	current := podInstanceID(pod)
	if current == instance {
		return obs, nil
	}
	if current != "" {
		// A worker-container restart proves that container's process tree
		// exited. A surviving sidecar could still own tools from the old turn.
		if len(pod.Spec.Containers) > 1 || len(pod.Spec.EphemeralContainers) > 0 {
			obs.Unconfirmed = true
			return obs, nil
		}
		for _, init := range pod.Spec.InitContainers {
			if init.RestartPolicy != nil && *init.RestartPolicy == corev1.ContainerRestartPolicyAlways {
				obs.Unconfirmed = true
				return obs, nil
			}
		}
		for _, c := range pod.Status.ContainerStatuses {
			last := c.LastTerminationState.Terminated
			if c.Name == "worker" && last != nil && !last.FinishedAt.IsZero() && last.Reason != "ContainerStatusUnknown" && uid+"/"+last.ContainerID+"/"+last.StartedAt.UTC().Format(time.RFC3339Nano) == instance {
				obs.Exited, obs.Successor = true, current
				return obs, nil
			}
		}
	}
	obs.Unconfirmed = true
	return obs, nil
}
