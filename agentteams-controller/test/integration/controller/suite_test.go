//go:build integration

package controller_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
	"go.uber.org/zap/zapcore"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	timeout  = 30 * time.Second
	interval = 250 * time.Millisecond
)

var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config // shared with leaderelection_test.go
	k8sClient client.Client
	ctx       context.Context
	cancel    context.CancelFunc

	// Worker mocks
	mockProv    *mocks.MockProvisioner
	mockDeploy  *mocks.MockDeployer
	mockBackend *mocks.MockWorkerBackend
	mockEnv     *mocks.MockEnvBuilder

	// Manager mocks
	mockMgrProv    *mocks.MockManagerProvisioner
	mockMgrDeploy  *mocks.MockManagerDeployer
	mockMgrBackend *mocks.MockWorkerBackend
	mockMgrEnv     *mocks.MockManagerEnvBuilder

	// Legacy wiring — real LegacyCompat against an in-memory OSS so tests can
	// assert workers-registry.json / teams-registry.json side effects.
	testOSS    *ossfake.Memory
	testLegacy *service.LegacyCompat
)

// testManagerName and testMatrixDomain mirror the values used by the Provisioner
// mock so that manually constructed MatrixUserIDs line up with what handlers
// see in production.
const (
	testManagerName   = "manager"
	testMatrixDomain  = "localhost"
	testAgentFSSubdir = "hiclaw-envtest"
)

func TestMain(m *testing.M) {
	testEnv = testutil.NewTestEnv()
	scheme := testutil.Scheme()

	var err error
	restCfg, err = testEnv.Start()
	if err != nil {
		panic(fmt.Sprintf("failed to start envtest: %v", err))
	}

	ctx, cancel = context.WithCancel(context.Background())
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.Level(zapcore.InfoLevel)))

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0", // disable metrics server in tests
		},
	})
	if err != nil {
		panic(fmt.Sprintf("failed to create manager: %v", err))
	}

	// Create a cacheless client so tests always read the latest state.
	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(fmt.Sprintf("failed to create k8s client: %v", err))
	}

	// Wire up Worker mocks
	mockProv = mocks.NewMockProvisioner()
	mockDeploy = mocks.NewMockDeployer()
	mockBackend = mocks.NewMockWorkerBackend()
	mockEnv = mocks.NewMockEnvBuilder()

	workerBackendRegistry := backend.NewRegistry(
		[]backend.WorkerBackend{mockBackend},
	)

	// Real LegacyCompat backed by an in-memory OSS so tests can assert
	// registry side effects (workers-registry.json / teams-registry.json).
	testOSS = ossfake.NewMemory()
	agentFSDir := os.TempDir()
	testLegacy = service.NewLegacyCompat(service.LegacyConfig{
		OSS:          testOSS,
		MatrixDomain: testMatrixDomain,
		ManagerName:  testManagerName,
		AgentFSDir:   agentFSDir,
	})

	workerReconciler := &controller.WorkerReconciler{
		Client:         mgr.GetClient(),
		Provisioner:    mockProv,
		Deployer:       mockDeploy,
		Backend:        workerBackendRegistry,
		EnvBuilder:     mockEnv,
		Legacy:         testLegacy,
		ControllerName: "test-ctl",
	}
	if _, err := workerReconciler.SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup WorkerReconciler: %v", err))
	}

	teamReconciler := &controller.TeamReconciler{
		Client:         mgr.GetClient(),
		Provisioner:    mockProv,
		Deployer:       mockDeploy,
		Backend:        workerBackendRegistry,
		EnvBuilder:     mockEnv,
		Legacy:         testLegacy,
		AgentFSDir:     agentFSDir,
		ControllerName: "test-ctl",
	}
	if _, err := teamReconciler.SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup TeamReconciler: %v", err))
	}

	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1beta1.Team{}, controller.TeamLeaderNameField,
		func(obj client.Object) []string {
			team, ok := obj.(*v1beta1.Team)
			if !ok || team.Spec.Leader.Name == "" {
				return nil
			}
			return []string{team.Spec.Leader.Name}
		}); err != nil {
		panic(fmt.Sprintf("failed to index team leader name: %v", err))
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1beta1.Team{}, controller.TeamWorkerNameField,
		func(obj client.Object) []string {
			team, ok := obj.(*v1beta1.Team)
			if !ok {
				return nil
			}
			names := make([]string, 0, len(team.Spec.Workers))
			for _, w := range team.Spec.Workers {
				if w.Name != "" {
					names = append(names, w.Name)
				}
			}
			return names
		}); err != nil {
		panic(fmt.Sprintf("failed to index team worker names: %v", err))
	}

	// Wire up Manager mocks
	mockMgrProv = mocks.NewMockManagerProvisioner()
	mockMgrDeploy = mocks.NewMockManagerDeployer()
	mockMgrBackend = mocks.NewMockWorkerBackend()
	mockMgrEnv = mocks.NewMockManagerEnvBuilder()

	mgrBackendRegistry := backend.NewRegistry(
		[]backend.WorkerBackend{mockMgrBackend},
	)

	managerReconciler := &controller.ManagerReconciler{
		Client:         mgr.GetClient(),
		Provisioner:    mockMgrProv,
		Deployer:       mockMgrDeploy,
		Backend:        mgrBackendRegistry,
		EnvBuilder:     mockMgrEnv,
		ControllerName: "test-ctl",
		UserLanguage:   "en",
		UserTimezone:   "America/Los_Angeles",
	}
	if err := managerReconciler.SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup ManagerReconciler: %v", err))
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(fmt.Sprintf("failed to start manager: %v", err))
		}
	}()

	// Wait for manager cache to sync
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		panic("cache sync failed")
	}

	code := m.Run()

	cancel()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
	}

	os.Exit(code)
}

// resetMocks resets all mock call records and Fn overrides between tests.
func resetMocks() {
	mockProv.Reset()
	mockDeploy.Reset()
	mockBackend.Reset()
	mockEnv.Reset()
}

// resetManagerMocks resets all Manager mock call records and Fn overrides.
func resetManagerMocks() {
	mockMgrProv.Reset()
	mockMgrDeploy.Reset()
	mockMgrBackend.Reset()
	mockMgrEnv.Reset()
}

// suppress unused import for v1beta1
var _ = v1beta1.GroupName
