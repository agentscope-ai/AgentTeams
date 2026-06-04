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
