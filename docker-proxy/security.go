package main

import (
	"fmt"
	"os"
	"strings"
)

// Higress registry pattern: higress-registry.{region}.cr.aliyuncs.com
const higressRegistrySuffix = ".cr.aliyuncs.com/"

func isHigressRegistry(image string) bool {
	// Match higress-registry-*.cr.aliyuncs.com/* or higress-registry.*.cr.aliyuncs.com/*
	idx := strings.Index(image, higressRegistrySuffix)
	if idx < 0 {
		return false
	}
	prefix := image[:idx]
	return strings.HasPrefix(prefix, "higress-registry")
}

func isLocalImage(image string) bool {
	// Local images have no dots before the first slash: e.g. "hiclaw/worker-agent:latest"
	// Registry images have dots: e.g. "registry.example.com/repo/image:tag"
	slashIdx := strings.Index(image, "/")
	if slashIdx < 0 {
		// No slash at all (e.g. "ubuntu:latest") — treat as local
		return true
	}
	firstPart := image[:slashIdx]
	return !strings.Contains(firstPart, ".")
}

func isLocalhostImage(image string) bool {
	return strings.HasPrefix(image, "localhost/") || strings.HasPrefix(image, "localhost:") ||
		strings.HasPrefix(image, "127.0.0.1/") || strings.HasPrefix(image, "127.0.0.1:")
}

// ContainerCreateRequest is a minimal representation of Docker's container create payload.
// Only fields relevant to security validation are included.
type ContainerCreateRequest struct {
	Image      string      `json:"Image"`
	HostConfig *HostConfig `json:"HostConfig,omitempty"`
}

type HostConfig struct {
	Binds       []string `json:"Binds,omitempty"`
	Mounts      []Mount  `json:"Mounts,omitempty"`
	Privileged  bool     `json:"Privileged,omitempty"`
	NetworkMode string   `json:"NetworkMode,omitempty"`
	PidMode     string   `json:"PidMode,omitempty"`
	CapAdd      []string `json:"CapAdd,omitempty"`
}

type Mount struct {
	Type string `json:"Type,omitempty"`
}

// SecurityValidator enforces container creation policies.
type SecurityValidator struct {
	AllowedRegistries []string
	ContainerPrefix   string
	DangerousCaps     map[string]bool
}

// NewSecurityValidator creates a validator from environment variables.
func NewSecurityValidator() *SecurityValidator {
	// Additional allowed image sources — can be a registry (e.g. "ghcr.io")
	// or registry+path (e.g. "ghcr.io/myorg", "registry.example.com/team/workers")
	var allowedRegistries []string
	if env := os.Getenv("HICLAW_PROXY_ALLOWED_REGISTRIES"); env != "" {
		for _, r := range strings.Split(env, ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				allowedRegistries = append(allowedRegistries, r)
			}
		}
	}

	prefix := "hiclaw-worker-"
	if env := os.Getenv("HICLAW_PROXY_CONTAINER_PREFIX"); env != "" {
		prefix = env
	}

	return &SecurityValidator{
		AllowedRegistries: allowedRegistries,
		ContainerPrefix:   prefix,
		DangerousCaps: map[string]bool{
			"SYS_ADMIN":    true,
			"SYS_PTRACE":   true,
			"DAC_OVERRIDE":  true,
			"NET_ADMIN":    true,
			"SYS_RAWIO":    true,
			"SYS_MODULE":   true,
		},
	}
}

// ValidateContainerCreate checks a container creation request against security policies.
func (v *SecurityValidator) ValidateContainerCreate(req ContainerCreateRequest, containerName string) error {
	// 1. Container name prefix
	if containerName != "" && !strings.HasPrefix(containerName, v.ContainerPrefix) {
		return fmt.Errorf("container name %q must start with %q", containerName, v.ContainerPrefix)
	}
	if strings.Contains(containerName, "/") || strings.Contains(containerName, "..") {
		return fmt.Errorf("container name %q contains invalid characters", containerName)
	}

	// 2. Image allowlist
	if !v.isImageAllowed(req.Image) {
		return fmt.Errorf("image %q is not allowed (not a local image, localhost, Higress registry, or configured registry)", req.Image)
	}

	if req.HostConfig == nil {
		return nil
	}

	// 3. No bind mounts (workers use MinIO, not host volumes)
	if len(req.HostConfig.Binds) > 0 {
		return fmt.Errorf("bind mounts are not allowed (got %d bind(s))", len(req.HostConfig.Binds))
	}
	for _, m := range req.HostConfig.Mounts {
		if strings.EqualFold(m.Type, "bind") {
			return fmt.Errorf("bind-type mounts are not allowed")
		}
	}

	// 4. No privileged mode
	if req.HostConfig.Privileged {
		return fmt.Errorf("privileged mode is not allowed")
	}

	// 5. No host network
	if req.HostConfig.NetworkMode == "host" {
		return fmt.Errorf("host network mode is not allowed")
	}

	// 6. No host PID
	if req.HostConfig.PidMode == "host" {
		return fmt.Errorf("host PID mode is not allowed")
	}

	// 7. No dangerous capabilities
	for _, cap := range req.HostConfig.CapAdd {
		if v.DangerousCaps[strings.ToUpper(cap)] {
			return fmt.Errorf("capability %q is not allowed", cap)
		}
	}

	return nil
}

func (v *SecurityValidator) isImageAllowed(image string) bool {
	// Allow all images from Higress registries (any region)
	if isHigressRegistry(image) {
		return true
	}
	// Allow local images (no registry prefix, e.g. "hiclaw/worker-agent:latest")
	if isLocalImage(image) {
		return true
	}
	// Allow localhost images
	if isLocalhostImage(image) {
		return true
	}
	// Check configured allowed image sources (registry or registry/path prefix)
	for _, reg := range v.AllowedRegistries {
		if strings.HasPrefix(image, reg+"/") {
			return true
		}
	}
	return false
}
