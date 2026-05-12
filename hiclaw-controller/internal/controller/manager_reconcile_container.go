package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileManagerContainer ensures the manager container/pod matches the desired
// lifecycle state (Running/Sleeping/Stopped). Idempotent: checks actual backend
// state before acting.
func (r *ManagerReconciler) reconcileManagerContainer(ctx context.Context, s *managerScope) (reconcile.Result, error) {
	if s.provResult == nil {
		return reconcile.Result{}, nil
	}

	m := s.manager
	desired := m.Spec.DesiredState()

	switch desired {
	case "Stopped":
		return r.ensureManagerContainerAbsent(ctx, s, true)
	case "Sleeping":
		return r.ensureManagerContainerAbsent(ctx, s, false)
	default:
		return r.ensureManagerContainerPresent(ctx, s)
	}
}

// ensureManagerContainerPresent ensures the manager container is running. If the
// container does not exist or was deleted, it is (re)created. If the spec has
// changed (Generation != ObservedGeneration) while the container is running, the
// old container is deleted and a new one created.
func (r *ManagerReconciler) ensureManagerContainerPresent(ctx context.Context, s *managerScope) (reconcile.Result, error) {
	m := s.manager
	wb := r.managerBackend(ctx)
	if wb == nil {
		log.FromContext(ctx).Info("no worker backend available, manager needs manual start")
		return reconcile.Result{}, nil
	}

	logger := log.FromContext(ctx)
	containerName := r.managerContainerName(m.Name)
	result, err := wb.Status(ctx, containerName)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("query container status: %w", err)
	}

	// TODO(hot-reload): All spec changes trigger container recreation because
	// agents only load config at startup (no hot-reload). When agent-side config
	// hot-reload is implemented (file watcher / Matrix reload command / webhook),
	// introduce a per-field hash annotation to distinguish pod-affecting fields
	// (Image, Runtime, Model) from config-only fields (Skills, McpServers, Soul,
	// Agents, Package) and skip recreation for config-only changes.
	// Docker/K8s drive recreate decisions off Generation drift; the
	// sandbox backend instead compares the desired pod-spec hash against
	// the AppliedSpecHash recorded on the CR's hiclaw.io/last-applied-spec-hash
	// annotation, which is resilient to controller restart and
	// Status-only patches.
	specChanged := m.Generation != m.Status.ObservedGeneration
	desiredHash := hashAppliedManagerSpec(m.Spec)

	switch result.Status {
	case backend.StatusRunning, backend.StatusStarting, backend.StatusReady:
		if wb.Name() == "sandbox" {
			// Empty AppliedSpecHash means a legacy CR created before
			// annotation support landed; treat as up-to-date so we do
			// not churn Running pods. Hash equality is the only signal
			// for sandbox; Generation drift is ignored.
			if result.AppliedSpecHash == "" || result.AppliedSpecHash == desiredHash {
				return reconcile.Result{}, nil
			}
			logger.Info("sandbox pod-spec hash drift, recreating manager container",
				"appliedSpecHash", result.AppliedSpecHash,
				"desiredSpecHash", desiredHash)
			if err := wb.Delete(ctx, containerName); err != nil && !errors.Is(err, backend.ErrNotFound) {
				return reconcile.Result{}, fmt.Errorf("delete container for recreate: %w", err)
			}
			return r.createManagerContainer(ctx, s, wb)
		}
		if !specChanged {
			return reconcile.Result{}, nil
		}
		logger.Info("spec changed, recreating manager container",
			"generation", m.Generation,
			"observedGeneration", m.Status.ObservedGeneration)
		if err := wb.Delete(ctx, containerName); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete container for recreate: %w", err)
		}
		return r.createManagerContainer(ctx, s, wb)

	case backend.StatusStopped:
		// Docker: keep the historical "no spec change → start, otherwise
		// recreate" semantics verbatim.
		if wb.Name() == "docker" {
			if !specChanged {
				if err := wb.Start(ctx, containerName); err != nil {
					return reconcile.Result{}, fmt.Errorf("start container: %w", err)
				}
				return reconcile.Result{}, nil
			}
			if err := wb.Delete(ctx, containerName); err != nil && !errors.Is(err, backend.ErrNotFound) {
				return reconcile.Result{}, fmt.Errorf("delete stale container: %w", err)
			}
			return r.createManagerContainer(ctx, s, wb)
		}
		// Sandbox: Resume only when AppliedSpecHash matches desired hash;
		// otherwise delete + recreate. Empty AppliedSpecHash means legacy
		// CR — force recreate so the new CR carries the annotation.
		if wb.Name() == "sandbox" {
			if result.AppliedSpecHash != "" && result.AppliedSpecHash == desiredHash {
				if err := wb.Start(ctx, containerName); err != nil {
					return reconcile.Result{}, fmt.Errorf("resume sandbox: %w", err)
				}
				return reconcile.Result{}, nil
			}
			logger.Info("sandbox pod-spec hash drift or legacy CR, recreating manager container",
				"appliedSpecHash", result.AppliedSpecHash,
				"desiredSpecHash", desiredHash)
			if err := wb.Delete(ctx, containerName); err != nil && !errors.Is(err, backend.ErrNotFound) {
				return reconcile.Result{}, fmt.Errorf("delete stale sandbox: %w", err)
			}
			return r.createManagerContainer(ctx, s, wb)
		}
		// Other backends (k8s) keep the historical delete+create path.
		if err := wb.Delete(ctx, containerName); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete stale container: %w", err)
		}
		return r.createManagerContainer(ctx, s, wb)

	case backend.StatusNotFound:
		return r.createManagerContainer(ctx, s, wb)

	default:
		logger.Info("container in unexpected state, recreating", "status", result.Status)
		if err := wb.Delete(ctx, containerName); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete container in unknown state: %w", err)
		}
		return r.createManagerContainer(ctx, s, wb)
	}
}

// ensureManagerContainerAbsent ensures the manager container is not running.
// If remove is true (Stopped), the container is deleted entirely.
// If remove is false (Sleeping), the container is stopped but kept (Docker)
// or deleted (K8s, which has no stop-without-delete).
func (r *ManagerReconciler) ensureManagerContainerAbsent(ctx context.Context, s *managerScope, remove bool) (reconcile.Result, error) {
	wb := r.managerBackend(ctx)
	if wb == nil {
		return reconcile.Result{}, nil
	}

	containerName := r.managerContainerName(s.manager.Name)
	if remove {
		_ = wb.Stop(ctx, containerName)
		if err := wb.Delete(ctx, containerName); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete container: %w", err)
		}
	} else {
		if err := wb.Stop(ctx, containerName); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("stop container: %w", err)
		}
	}

	return reconcile.Result{}, nil
}

// createManagerContainer builds and issues a backend Create request for the manager.
// ErrConflict (container already exists) is treated as success for idempotency.
func (r *ManagerReconciler) createManagerContainer(ctx context.Context, s *managerScope, wb backend.WorkerBackend) (reconcile.Result, error) {
	m := s.manager
	logger := log.FromContext(ctx)

	prov := s.provResult
	if prov.MatrixToken == "" {
		refreshResult, err := r.Provisioner.RefreshManagerCredentials(ctx, m.Name)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("refresh credentials for container: %w", err)
		}
		prov = &service.ManagerProvisionResult{
			MatrixUserID:   m.Status.MatrixUserID,
			MatrixToken:    refreshResult.MatrixToken,
			RoomID:         m.Status.RoomID,
			GatewayKey:     refreshResult.GatewayKey,
			MinIOPassword:  refreshResult.MinIOPassword,
			MatrixPassword: refreshResult.MatrixPassword,
		}
	}

	managerEnv := r.EnvBuilder.BuildManager(m.Name, prov, m.Spec)
	if s.modelProviderInfo != nil && s.modelProviderInfo.IntranetURL != "" {
		managerEnv["HICLAW_AI_GATEWAY_URL"] = s.modelProviderInfo.IntranetURL
	}
	mergeUserEnv(managerEnv, m.Spec.Env, logger, "manager/"+m.Name)
	containerName := r.managerContainerName(m.Name)
	saName := r.ResourcePrefix.SAName(authpkg.RoleManager, m.Name)
	resources := mergeAgentResourcesWithBackendDefaults(r.ManagerResources, m.Spec.Resources)
	// Pod labels are layered low-to-high: CR metadata.labels, CR
	// spec.labels, then controller-forced system labels. The last layer
	// wins on collision so a user-supplied `hiclaw.io/controller` (or
	// any other reserved key) cannot spoof the controller identity.
	createReq := backend.CreateRequest{
		Name:               m.Name,
		ContainerName:      containerName,
		Image:              m.Spec.Image,
		Runtime:            m.Spec.Runtime,
		RuntimeFallback:    r.DefaultRuntime,
		Env:                managerEnv,
		ServiceAccountName: saName,
		Resources:          resources,
		// Sandbox backend stamps AppliedSpecHash onto the new CR's
		// hiclaw.io/last-applied-spec-hash annotation so subsequent reconciles
		// can detect workload-relevant drift via AppliedSpecHash. Other
		// backends ignore this field.
		AppliedSpecHash: hashAppliedManagerSpec(m.Spec),
		Labels: mergeLabels(
			m.ObjectMeta.Labels,
			m.Spec.Labels,
			map[string]string{
				"app":                   r.ResourcePrefix.ManagerAppLabel(),
				"hiclaw.io/manager":     m.Name,
				"hiclaw.io/role":        "manager",
				"hiclaw.io/runtime":     backend.ResolveRuntime(m.Spec.Runtime, r.DefaultRuntime),
				v1beta1.LabelController: r.ControllerName,
			},
		),
		Owner: m,
	}
	if wb.Name() != "k8s" {
		token, err := r.Provisioner.RequestManagerSAToken(ctx, m.Name)
		if err != nil {
			logger.Error(err, "SA token request failed (non-fatal, manager auth will fail)")
		}
		createReq.AuthToken = token
	}

	r.applyEmbeddedConfig(&createReq, wb)

	if _, err := wb.Create(ctx, createReq); err != nil {
		if errors.Is(err, backend.ErrConflict) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("create container: %w", err)
	}

	return reconcile.Result{}, nil
}

// applyEmbeddedConfig injects Docker-mode host volume mounts, port mapping,
// restart policy, and extra env into the CreateRequest when running in embedded mode.
func (r *ManagerReconciler) applyEmbeddedConfig(req *backend.CreateRequest, wb backend.WorkerBackend) {
	if wb.Name() != "docker" || r.EmbeddedConfig == nil {
		return
	}

	if r.EmbeddedConfig.WorkspaceDir != "" {
		req.Volumes = append(req.Volumes, backend.VolumeMount{
			HostPath:      r.EmbeddedConfig.WorkspaceDir,
			ContainerPath: "/root/manager-workspace",
		})
	}
	if r.EmbeddedConfig.HostShareDir != "" {
		req.Volumes = append(req.Volumes, backend.VolumeMount{
			HostPath:      r.EmbeddedConfig.HostShareDir,
			ContainerPath: "/host-share",
		})
	}

	req.RestartPolicy = "unless-stopped"

	consoleHostPort := r.EmbeddedConfig.ManagerConsolePort
	if consoleHostPort == "" {
		consoleHostPort = "18888"
	}
	req.Ports = append(req.Ports, backend.PortMapping{
		HostIP:        "127.0.0.1",
		HostPort:      consoleHostPort,
		ContainerPort: "18799",
	})

	for k, v := range r.EmbeddedConfig.ExtraEnv {
		if _, exists := req.Env[k]; !exists {
			req.Env[k] = v
		}
	}
}

// hashAppliedManagerSpec computes a fnv64a hash of the ManagerSpec with State
// zeroed out. This captures all spec fields that should trigger sandbox
// recreation when changed.
//
// Current coverage (fnv64a over json.Marshal with State=nil):
//
//	Model, Runtime, Image, Soul, Agents, Skills, McpServers, Package, Config,
//	AccessEntries, Labels, Env.
//
// Consumed by ensureManagerContainerPresent / createManagerContainer to
// populate CreateRequest.AppliedSpecHash, which the sandbox backend stamps
// onto the underlying CR via the hiclaw.io/last-applied-spec-hash annotation.
//
// TODO: When Agent-side hot-reload lands, narrow to pod-affecting fields
// only (Image, Runtime, Model, Env, Labels, AccessEntries) and handle
// config-only changes via the reload channel.
func hashAppliedManagerSpec(spec v1beta1.ManagerSpec) string {
	spec.State = nil // exclude lifecycle state from hash
	buf, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return fmt.Sprintf("%x", h.Sum64())
}

// managerBackend returns the WorkerBackend with the container prefix cleared.
// Manager containers use explicit full names (e.g. "hiclaw-manager") rather than
// the default worker prefix ("hiclaw-worker-"), so we need WithPrefix("") to
// ensure Status/Stop/Delete/Start operate on the correct container/pod name.
func (r *ManagerReconciler) managerBackend(ctx context.Context) backend.WorkerBackend {
	if r.Backend == nil {
		return nil
	}
	wb := r.Backend.DetectWorkerBackend(ctx)
	if wb == nil {
		return nil
	}
	switch b := wb.(type) {
	case *backend.DockerBackend:
		return b.WithPrefix("")
	case *backend.K8sBackend:
		return b.WithPrefix("")
	case *backend.SandboxBackend:
		return b.WithPrefix("")
	default:
		return wb
	}
}
