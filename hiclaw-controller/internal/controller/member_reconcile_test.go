package controller

import (
	"context"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
)

func TestCreateMemberContainerAddsDockerHostGateway(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "docker"
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "token"},
	}

	_, err := createMemberContainer(context.Background(), MemberDeps{
		Provisioner: mocks.NewMockProvisioner(),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, MemberContext{
		Name: "alice",
		Spec: v1beta1.WorkerSpec{Image: "img:latest"},
	}, state, wb)
	if err != nil {
		t.Fatalf("createMemberContainer failed: %v", err)
	}

	req, ok := wb.LastCreateReq()
	if !ok {
		t.Fatal("expected backend Create to be called")
	}
	if got, want := req.ExtraHosts, []string{dockerHostInternalExtraHost}; !equalStringSlices(got, want) {
		t.Fatalf("ExtraHosts=%v, want %v", got, want)
	}
}

func TestCreateMemberContainerDoesNotAddDockerHostGatewayForK8s(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "k8s"
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "token"},
	}

	_, err := createMemberContainer(context.Background(), MemberDeps{
		Provisioner: mocks.NewMockProvisioner(),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, MemberContext{
		Name: "alice",
		Spec: v1beta1.WorkerSpec{Image: "img:latest"},
	}, state, wb)
	if err != nil {
		t.Fatalf("createMemberContainer failed: %v", err)
	}

	req, ok := wb.LastCreateReq()
	if !ok {
		t.Fatal("expected backend Create to be called")
	}
	if len(req.ExtraHosts) != 0 {
		t.Fatalf("ExtraHosts=%v, want empty for k8s backend", req.ExtraHosts)
	}
}

func TestCreateMemberContainerPassesSpecResources(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "k8s"
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "token"},
	}

	_, err := createMemberContainer(context.Background(), MemberDeps{
		Provisioner: mocks.NewMockProvisioner(),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, MemberContext{
		Name: "alice",
		Spec: v1beta1.WorkerSpec{
			Image: "img:latest",
			Resources: &v1beta1.AgentResourceRequirements{
				Requests: v1beta1.AgentResourceValues{CPU: "250m", Memory: "512Mi"},
				Limits:   v1beta1.AgentResourceValues{CPU: "2", Memory: "4Gi"},
			},
		},
	}, state, wb)
	if err != nil {
		t.Fatalf("createMemberContainer failed: %v", err)
	}

	req, ok := wb.LastCreateReq()
	if !ok {
		t.Fatal("expected backend Create to be called")
	}
	if req.Resources == nil {
		t.Fatal("CreateRequest.Resources = nil, want spec resources")
	}
	if req.Resources.CPURequest != "250m" || req.Resources.MemoryRequest != "512Mi" ||
		req.Resources.CPULimit != "2" || req.Resources.MemoryLimit != "4Gi" {
		t.Fatalf("CreateRequest.Resources = %+v", req.Resources)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
