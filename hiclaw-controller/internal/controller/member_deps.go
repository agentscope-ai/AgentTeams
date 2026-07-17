package controller

import (
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"k8s.io/client-go/dynamic"
)

// MemberDepsConfig carries the shared dependencies required to reconcile a
// Worker or inline Team member through the member phase pipeline.
type MemberDepsConfig struct {
	Provisioner                 service.WorkerProvisioner
	Deployer                    service.WorkerDeployer
	Backend                     *backend.Registry
	EnvBuilder                  service.WorkerEnvBuilderI
	ResourcePrefix              auth.ResourcePrefix
	DefaultRuntime              string
	GatewayClient               gateway.Client
	DynamicClient               dynamic.Interface
	RemoteDynamicClientProvider backend.RemoteDynamicClientProvider
	AuthTokenExpirationSeconds  int64
	ControllerName              string
	WorkerDepsStorageBucket     string
	WorkerDepsStorageEndpoint   string
	MountAuthType               string
	MountRoleName               string
}

// NewMemberDeps constructs MemberDeps from a MemberDepsConfig snapshot.
func NewMemberDeps(cfg MemberDepsConfig) MemberDeps {
	return MemberDeps{
		Provisioner:                 cfg.Provisioner,
		Deployer:                    cfg.Deployer,
		Backend:                     cfg.Backend,
		EnvBuilder:                  cfg.EnvBuilder,
		ResourcePrefix:              cfg.ResourcePrefix,
		DefaultRuntime:              cfg.DefaultRuntime,
		GatewayClient:               cfg.GatewayClient,
		DynamicClient:               cfg.DynamicClient,
		RemoteDynamicClientProvider: cfg.RemoteDynamicClientProvider,
		AuthTokenExpirationSeconds:  cfg.AuthTokenExpirationSeconds,
		ControllerName:              cfg.ControllerName,
		WorkerDepsStorageBucket:     cfg.WorkerDepsStorageBucket,
		WorkerDepsStorageEndpoint:   cfg.WorkerDepsStorageEndpoint,
		MountAuthType:               cfg.MountAuthType,
		MountRoleName:               cfg.MountRoleName,
	}
}

func (r *TeamReconciler) memberDepsConfig() MemberDepsConfig {
	return MemberDepsConfig{
		Provisioner:                 r.Provisioner,
		Deployer:                    r.Deployer,
		Backend:                     r.Backend,
		EnvBuilder:                  r.EnvBuilder,
		ResourcePrefix:              r.ResourcePrefix,
		DefaultRuntime:              r.DefaultRuntime,
		GatewayClient:               r.GatewayClient,
		DynamicClient:               r.DynamicClient,
		RemoteDynamicClientProvider: r.RemoteDynamicClientProvider,
		AuthTokenExpirationSeconds:  r.AuthTokenExpirationSeconds,
		ControllerName:              r.ControllerName,
		WorkerDepsStorageBucket:     r.WorkerDepsStorageBucket,
		WorkerDepsStorageEndpoint:   r.WorkerDepsStorageEndpoint,
		MountAuthType:               r.MountAuthType,
		MountRoleName:               r.MountRoleName,
	}
}

func (r *TeamReconciler) memberDeps() MemberDeps {
	return NewMemberDeps(r.memberDepsConfig())
}

func (r *WorkerReconciler) memberDepsConfig() MemberDepsConfig {
	return MemberDepsConfig{
		Provisioner:                 r.Provisioner,
		Deployer:                    r.Deployer,
		Backend:                     r.Backend,
		EnvBuilder:                  r.EnvBuilder,
		ResourcePrefix:              r.ResourcePrefix,
		DefaultRuntime:              r.DefaultRuntime,
		GatewayClient:               r.GatewayClient,
		DynamicClient:               r.DynamicClient,
		RemoteDynamicClientProvider: r.RemoteDynamicClientProvider,
		AuthTokenExpirationSeconds:  r.AuthTokenExpirationSeconds,
		ControllerName:              r.ControllerName,
		WorkerDepsStorageBucket:     r.WorkerDepsStorageBucket,
		WorkerDepsStorageEndpoint:   r.WorkerDepsStorageEndpoint,
		MountAuthType:               r.MountAuthType,
		MountRoleName:               r.MountRoleName,
	}
}

func (r *WorkerReconciler) memberDeps() MemberDeps {
	return NewMemberDeps(r.memberDepsConfig())
}
