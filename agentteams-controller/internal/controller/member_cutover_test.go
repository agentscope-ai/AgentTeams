package controller

import (
	"context"
	"errors"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
)

func TestMemberBackendCutoverWaitsForObservedAbsence(t *testing.T) {
	old := mocks.NewMockWorkerBackend()
	old.NameOverride = "sandbox"
	target := mocks.NewMockWorkerBackend()
	target.NameOverride = "k8s"
	present := true
	// Deletion acceptance is asynchronous. An unchanged Status must keep the
	// old backend authoritative, including across controller reconciliation.
	old.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		status := backend.StatusRunning
		if !present {
			status = backend.StatusNotFound
		}
		return &backend.WorkerResult{Status: status}, nil
	}
	d := MemberDeps{Backend: backend.NewRegistry([]backend.WorkerBackend{old, target}),
		EnvBuilder: mocks.NewMockEnvBuilder(), Provisioner: mocks.NewMockProvisioner()}
	m := MemberContext{Name: "alice", RuntimeName: "alice", Spec: v1beta1.WorkerSpec{Runtime: "openclaw", Image: "test:latest"},
		BackendRuntime: v1beta1.BackendRuntimePod, StatusBackendRuntime: v1beta1.BackendRuntimeSandbox}
	for i := 0; i < 2; i++ {
		state := &MemberState{ProvResult: &service.WorkerProvisionResult{}}
		result, err := ensureMemberContainerPresent(context.Background(), d, m, state)
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter <= 0 || state.ContainerState != "stopping" {
			t.Fatalf("not waiting: %+v %+v", result, state)
		}
		if state.BackendRuntime != v1beta1.BackendRuntimeSandbox {
			t.Fatalf("lost old backend: %q", state.BackendRuntime)
		}
		if len(target.Calls.Status) != 0 || len(target.Calls.Create) != 0 {
			t.Fatal("accessed target before old resource disappeared")
		}
		m.StatusBackendRuntime = state.BackendRuntime
	}
	present = false
	state := &MemberState{ProvResult: &service.WorkerProvisionResult{}}
	if _, err := ensureMemberContainerPresent(context.Background(), d, m, state); err != nil {
		t.Fatal(err)
	}
	if state.BackendRuntime != v1beta1.BackendRuntimePod || len(target.Calls.Create) != 1 {
		t.Fatalf("target not created after absence: %+v", state)
	}
}

func TestMemberBackendCutoverFailsClosed(t *testing.T) {
	for _, scenario := range []string{"unregistered", "status_error", "nil_status", "delete_error", "unknown_status"} {
		t.Run(scenario, func(t *testing.T) {
			old := mocks.NewMockWorkerBackend()
			old.NameOverride = "sandbox"
			target := mocks.NewMockWorkerBackend()
			target.NameOverride = "k8s"
			old.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
				switch scenario {
				case "status_error":
					return nil, errors.New("old backend unavailable")
				case "nil_status":
					return nil, nil
				case "unknown_status":
					return &backend.WorkerResult{}, nil
				default:
					return &backend.WorkerResult{Status: backend.StatusRunning}, nil
				}
			}
			if scenario == "delete_error" {
				old.DeleteFn = func(context.Context, string) error { return errors.New("delete rejected") }
			}
			backends := []backend.WorkerBackend{old, target}
			if scenario == "unregistered" {
				backends = []backend.WorkerBackend{target}
			}
			d := MemberDeps{Backend: backend.NewRegistry(backends), EnvBuilder: mocks.NewMockEnvBuilder(), Provisioner: mocks.NewMockProvisioner()}
			m := MemberContext{Name: "alice", BackendRuntime: v1beta1.BackendRuntimePod, StatusBackendRuntime: v1beta1.BackendRuntimeSandbox}
			state := &MemberState{ProvResult: &service.WorkerProvisionResult{}}
			result, err := ensureMemberContainerPresent(context.Background(), d, m, state)
			if scenario != "unknown_status" && err == nil {
				t.Fatal("expected fail-closed error")
			}
			if scenario == "unknown_status" && (err != nil || result.RequeueAfter <= 0) {
				t.Fatalf("unknown state did not wait: %+v %v", result, err)
			}
			if len(target.Calls.Status) != 0 || len(target.Calls.Create) != 0 {
				t.Fatal("target accessed without old resource absence")
			}
			if state.BackendRuntime == v1beta1.BackendRuntimePod {
				t.Fatal("advanced recorded backend on failure")
			}
		})
	}
}

func TestMemberBackendCutoverDeleteNotFoundStillRequiresStatus(t *testing.T) {
	old := mocks.NewMockWorkerBackend()
	old.NameOverride = "sandbox"
	old.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Status: backend.StatusRunning}, nil
	}
	old.DeleteFn = func(context.Context, string) error { return backend.ErrNotFound }
	target := mocks.NewMockWorkerBackend()
	target.NameOverride = "k8s"
	d := MemberDeps{Backend: backend.NewRegistry([]backend.WorkerBackend{old, target}), EnvBuilder: mocks.NewMockEnvBuilder(), Provisioner: mocks.NewMockProvisioner()}
	m := MemberContext{Name: "alice", BackendRuntime: v1beta1.BackendRuntimePod, StatusBackendRuntime: v1beta1.BackendRuntimeSandbox}
	state := &MemberState{ProvResult: &service.WorkerProvisionResult{}}
	result, err := ensureMemberContainerPresent(context.Background(), d, m, state)
	if err != nil || result.RequeueAfter <= 0 || len(target.Calls.Create) != 0 {
		t.Fatalf("delete response bypassed status barrier: %+v %v", result, err)
	}
}

func TestMemberBackendCutoverDoesNotResolveMissingPodToTarget(t *testing.T) {
	target := mocks.NewMockWorkerBackend()
	target.NameOverride = "sandbox"
	d := MemberDeps{Backend: backend.NewRegistry([]backend.WorkerBackend{target})}
	m := MemberContext{Name: "alice", BackendRuntime: v1beta1.BackendRuntimeSandbox, StatusBackendRuntime: v1beta1.BackendRuntimePod}
	state := &MemberState{ProvResult: &service.WorkerProvisionResult{}}
	if _, err := ensureMemberContainerPresent(context.Background(), d, m, state); err == nil {
		t.Fatal("missing previous Pod backend was silently mapped to target")
	}
	if len(target.Calls.Delete) != 0 || len(target.Calls.Status) != 0 || len(target.Calls.Create) != 0 {
		t.Fatal("target was mistaken for old backend")
	}
}

func TestPreviousMemberBackendPreservesEmbeddedDockerAlias(t *testing.T) {
	docker := mocks.NewMockWorkerBackend()
	docker.NameOverride = "docker"
	got, err := resolvePreviousMemberBackend(context.Background(), backend.NewRegistry([]backend.WorkerBackend{docker}), v1beta1.BackendRuntimePod)
	if err != nil || got != docker {
		t.Fatalf("embedded pod alias: %v %v", got, err)
	}
	k8s := mocks.NewMockWorkerBackend()
	k8s.NameOverride = "k8s"
	got, err = resolvePreviousMemberBackend(context.Background(), backend.NewRegistry([]backend.WorkerBackend{docker, k8s}), v1beta1.BackendRuntimePod)
	if err != nil || got != k8s {
		t.Fatalf("Kubernetes identity lost to Docker auto-detection: %v %v", got, err)
	}
}
