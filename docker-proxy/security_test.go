package main

import (
	"testing"
)

func newTestValidator() *SecurityValidator {
	return &SecurityValidator{
		AllowedRegistries: []string{},
		ContainerPrefix:   "hiclaw-worker-",
		DangerousCaps: map[string]bool{
			"SYS_ADMIN":   true,
			"SYS_PTRACE":  true,
			"DAC_OVERRIDE": true,
			"NET_ADMIN":   true,
			"SYS_RAWIO":   true,
			"SYS_MODULE":  true,
		},
	}
}

func TestValidWorkerCreate(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image:      "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{},
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-alice"); err != nil {
		t.Errorf("expected valid request to pass, got: %v", err)
	}
}

func TestValidCopawWorkerCreate(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image:      "hiclaw/copaw-worker:latest",
		HostConfig: &HostConfig{},
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-bob"); err != nil {
		t.Errorf("expected valid copaw request to pass, got: %v", err)
	}
}

func TestNoHostConfig(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image: "hiclaw/worker-agent:latest",
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-alice"); err != nil {
		t.Errorf("expected nil HostConfig to pass, got: %v", err)
	}
}

func TestEmptyContainerName(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image:      "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{},
	}
	// Empty name should pass (Docker generates one)
	if err := v.ValidateContainerCreate(req, ""); err != nil {
		t.Errorf("expected empty name to pass, got: %v", err)
	}
}

// --- Rejection tests ---

func TestRejectBadContainerName(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image:      "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{},
	}
	cases := []string{
		"evil-container",
		"my-worker-alice",
		"hiclaw-manager-backdoor",
	}
	for _, name := range cases {
		if err := v.ValidateContainerCreate(req, name); err == nil {
			t.Errorf("expected name %q to be rejected", name)
		}
	}
}

func TestRejectContainerNameTraversal(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image:      "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{},
	}
	cases := []string{
		"hiclaw-worker-../escape",
		"hiclaw-worker-foo/bar",
	}
	for _, name := range cases {
		if err := v.ValidateContainerCreate(req, name); err == nil {
			t.Errorf("expected name %q to be rejected", name)
		}
	}
}

func TestRejectUnallowedImage(t *testing.T) {
	v := newTestValidator()
	// Only external registry images not in higress should be rejected
	cases := []string{
		"docker.io/library/ubuntu:latest",
		"gcr.io/my-project/evil:latest",
		"registry.example.com/repo/image:tag",
	}
	for _, img := range cases {
		req := ContainerCreateRequest{
			Image:      img,
			HostConfig: &HostConfig{},
		}
		if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err == nil {
			t.Errorf("expected image %q to be rejected", img)
		}
	}
}

func TestAllowLocalImages(t *testing.T) {
	v := newTestValidator()
	cases := []string{
		"hiclaw/worker-agent:latest",
		"hiclaw/copaw-worker:latest",
		"hiclaw/manager-agent:latest",
		"myteam/custom-worker:v1",
		"ubuntu:latest",
	}
	for _, img := range cases {
		req := ContainerCreateRequest{
			Image:      img,
			HostConfig: &HostConfig{},
		}
		if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err != nil {
			t.Errorf("expected local image %q to be allowed, got: %v", img, err)
		}
	}
}

func TestAllowLocalhostImages(t *testing.T) {
	v := newTestValidator()
	cases := []string{
		"localhost/myimage:latest",
		"localhost:5000/myimage:latest",
		"127.0.0.1/myimage:latest",
		"127.0.0.1:5000/myimage:latest",
	}
	for _, img := range cases {
		req := ContainerCreateRequest{
			Image:      img,
			HostConfig: &HostConfig{},
		}
		if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err != nil {
			t.Errorf("expected localhost image %q to be allowed, got: %v", img, err)
		}
	}
}

func TestAllowHigressRegistryImages(t *testing.T) {
	v := newTestValidator()
	cases := []string{
		"higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/hiclaw-worker:latest",
		"higress-registry.us-west-1.cr.aliyuncs.com/higress/hiclaw-worker:v1.0",
		"higress-registry.ap-southeast-7.cr.aliyuncs.com/higress/custom-image:latest",
	}
	for _, img := range cases {
		req := ContainerCreateRequest{
			Image:      img,
			HostConfig: &HostConfig{},
		}
		if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err != nil {
			t.Errorf("expected higress image %q to be allowed, got: %v", img, err)
		}
	}
}

func TestAllowConfiguredRegistries(t *testing.T) {
	v := newTestValidator()
	v.AllowedRegistries = []string{"ghcr.io/myorg", "registry.example.com"}

	cases := []string{
		"ghcr.io/myorg/custom-worker:latest",
		"ghcr.io/myorg/tools/builder:v2",
		"registry.example.com/team/worker:latest",
	}
	for _, img := range cases {
		req := ContainerCreateRequest{
			Image:      img,
			HostConfig: &HostConfig{},
		}
		if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err != nil {
			t.Errorf("expected configured registry image %q to be allowed, got: %v", img, err)
		}
	}

	// Should still block other registries
	blocked := []string{
		"ghcr.io/otherorg/evil:latest",
		"gcr.io/myorg/image:latest",
	}
	for _, img := range blocked {
		req := ContainerCreateRequest{
			Image:      img,
			HostConfig: &HostConfig{},
		}
		if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err == nil {
			t.Errorf("expected image %q to be rejected", img)
		}
	}
}

func TestRejectBindMounts(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image: "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{
			Binds: []string{"/root:/hostroot"},
		},
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err == nil {
		t.Error("expected bind mount to be rejected")
	}
}

func TestRejectBindTypeMounts(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image: "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{
			Mounts: []Mount{{Type: "bind"}},
		},
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err == nil {
		t.Error("expected bind-type mount to be rejected")
	}
}

func TestRejectPrivileged(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image: "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{
			Privileged: true,
		},
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err == nil {
		t.Error("expected privileged to be rejected")
	}
}

func TestRejectHostNetwork(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image: "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{
			NetworkMode: "host",
		},
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err == nil {
		t.Error("expected host network to be rejected")
	}
}

func TestRejectHostPID(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image: "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{
			PidMode: "host",
		},
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err == nil {
		t.Error("expected host PID to be rejected")
	}
}

func TestRejectDangerousCaps(t *testing.T) {
	v := newTestValidator()
	caps := []string{"SYS_ADMIN", "SYS_PTRACE", "DAC_OVERRIDE", "NET_ADMIN", "sys_admin"}
	for _, cap := range caps {
		req := ContainerCreateRequest{
			Image: "hiclaw/worker-agent:latest",
			HostConfig: &HostConfig{
				CapAdd: []string{cap},
			},
		}
		if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err == nil {
			t.Errorf("expected capability %q to be rejected", cap)
		}
	}
}

func TestAllowBridgeNetwork(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image: "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{
			NetworkMode: "bridge",
		},
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err != nil {
		t.Errorf("expected bridge network to pass, got: %v", err)
	}
}

func TestAllowSafeCaps(t *testing.T) {
	v := newTestValidator()
	req := ContainerCreateRequest{
		Image: "hiclaw/worker-agent:latest",
		HostConfig: &HostConfig{
			CapAdd: []string{"NET_BIND_SERVICE"},
		},
	}
	if err := v.ValidateContainerCreate(req, "hiclaw-worker-test"); err != nil {
		t.Errorf("expected safe cap to pass, got: %v", err)
	}
}
