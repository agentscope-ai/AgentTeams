package controller

import (
	"context"
	"errors"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
)

func TestManagerHarnessCutoverWaitsForObservedAbsence(t *testing.T) {
	for _, name := range []string{"docker", "k8s", "sandbox"} {
		for _, status := range []backend.WorkerStatus{backend.StatusRunning, backend.StatusStarting, backend.StatusReady, backend.StatusStopped, backend.StatusUnknown} {
			t.Run(name+"/"+string(status), func(t *testing.T) {
				b := mocks.NewMockWorkerBackend()
				b.NameOverride = name
				present := true
				b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
					if present {
						return &backend.WorkerResult{Status: status}, nil
					}
					return &backend.WorkerResult{Status: backend.StatusNotFound}, nil
				}
				// Even Delete's not-found response cannot replace an observed absence:
				// backends can have asynchronous deletion/visibility boundaries.
				b.DeleteFn = func(context.Context, string) error { return backend.ErrNotFound }
				r := &ManagerReconciler{Backend: backend.NewRegistry([]backend.WorkerBackend{b}), Provisioner: mocks.NewMockManagerProvisioner(), EnvBuilder: mocks.NewMockManagerEnvBuilder(), ResourcePrefix: auth.DefaultResourcePrefix}
				m := &v1beta1.Manager{Spec: v1beta1.ManagerSpec{Runtime: "openclaw"}}
				m.Status.SpecHash = hashAppliedManagerSpec(m.Spec)
				oldHash := m.Status.SpecHash
				m.Spec.Runtime = "hermes"
				s := &managerScope{manager: m, provResult: &service.ManagerProvisionResult{MatrixToken: "test"}}
				for i := 0; i < 2; i++ {
					m.Status.Ready = true
					result, err := r.ensureManagerContainerPresent(context.Background(), s)
					if err != nil {
						t.Fatal(err)
					}
					if result.RequeueAfter <= 0 || m.Status.Ready || m.Status.SpecHash != oldHash || len(b.Calls.Create) != 0 || len(b.Calls.Start) != 0 {
						t.Fatalf("cutover did not wait safely: result=%+v ready=%v hash=%s creates=%d", result, m.Status.Ready, m.Status.SpecHash, len(b.Calls.Create))
					}
				}
				present = false
				if _, err := r.ensureManagerContainerPresent(context.Background(), s); err != nil {
					t.Fatal(err)
				}
				if len(b.Calls.CreateReqs) != 1 || b.Calls.CreateReqs[0].Runtime != "hermes" || m.Status.SpecHash != hashAppliedManagerSpec(m.Spec) {
					t.Fatal("new Harness not created after absence")
				}
			})
		}
	}
}

func TestManagerHarnessCutoverFailsClosed(t *testing.T) {
	for _, scenario := range []string{"status_error", "nil_status", "delete_error"} {
		t.Run(scenario, func(t *testing.T) {
			b := mocks.NewMockWorkerBackend()
			b.NameOverride = "docker"
			b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
				if scenario == "status_error" {
					return nil, errors.New("unavailable")
				}
				if scenario == "nil_status" {
					return nil, nil
				}
				return &backend.WorkerResult{Status: backend.StatusRunning}, nil
			}
			b.DeleteFn = func(context.Context, string) error { return errors.New("rejected") }
			r := &ManagerReconciler{Backend: backend.NewRegistry([]backend.WorkerBackend{b})}
			m := &v1beta1.Manager{}
			m.Status.SpecHash = "old"
			if _, err := r.ensureManagerContainerPresent(context.Background(), &managerScope{manager: m}); err == nil {
				t.Fatal("expected error")
			}
			if len(b.Calls.Create) != 0 || m.Status.SpecHash != "old" {
				t.Fatal("advanced failed cutover")
			}
		})
	}
}

func TestManagerUnchangedHarnessStillResumes(t *testing.T) {
	for _, name := range []string{"docker", "sandbox"} {
		for _, status := range []backend.WorkerStatus{backend.StatusRunning, backend.StatusStopped} {
			t.Run(name+"/"+string(status), func(t *testing.T) {
				b := mocks.NewMockWorkerBackend()
				b.NameOverride = name
				b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
					return &backend.WorkerResult{Status: status}, nil
				}
				r := &ManagerReconciler{Backend: backend.NewRegistry([]backend.WorkerBackend{b})}
				m := &v1beta1.Manager{Spec: v1beta1.ManagerSpec{Runtime: "hermes"}}
				m.Status.SpecHash = hashAppliedManagerSpec(m.Spec)
				result, err := r.ensureManagerContainerPresent(context.Background(), &managerScope{manager: m})
				if err != nil || result.RequeueAfter != 0 || len(b.Calls.Delete) != 0 || len(b.Calls.Create) != 0 {
					t.Fatalf("unchanged Harness disrupted: %+v %v", result, err)
				}
				if status == backend.StatusStopped && len(b.Calls.Start) != 1 {
					t.Fatal("compatible sleeping runtime not resumed")
				}
			})
		}
	}
}
