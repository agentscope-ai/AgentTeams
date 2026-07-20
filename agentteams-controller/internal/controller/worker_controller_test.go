package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type workerTestRig struct {
	t           *testing.T
	client      client.Client
	backend     *mocks.MockWorkerBackend
	deployer    *mocks.MockDeployer
	provisioner *mocks.MockProvisioner
	envBuilder  *mocks.MockEnvBuilder
	remote      dynamic.Interface
	r           *WorkerReconciler
}

type fakeRemoteDynamicProvider struct {
	client    dynamic.Interface
	onResolve func()
}

func (f fakeRemoteDynamicProvider) ResolveDynamicClient(context.Context, string) (dynamic.Interface, error) {
	if f.onResolve != nil {
		f.onResolve()
	}
	return f.client, nil
}

func newWorkerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return s
}

func newWorkerRig(t *testing.T, objs ...client.Object) *workerTestRig {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(newWorkerScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1beta1.Worker{}).
		Build()
	wb := mocks.NewMockWorkerBackend()
	deployer := mocks.NewMockDeployer()
	provisioner := mocks.NewMockProvisioner()
	envBuilder := mocks.NewMockEnvBuilder()
	localDynamic := newWorkerTestDynamicClient(testWorkerDepsSecret("default"))
	remoteDynamic := newWorkerTestDynamicClient(testWorkerDepsSecret("agents"))
	return &workerTestRig{
		t:           t,
		client:      c,
		backend:     wb,
		deployer:    deployer,
		provisioner: provisioner,
		envBuilder:  envBuilder,
		remote:      remoteDynamic,
		r: &WorkerReconciler{
			Client:                      c,
			Provisioner:                 provisioner,
			Deployer:                    deployer,
			Backend:                     backend.NewRegistry([]backend.WorkerBackend{wb}),
			EnvBuilder:                  envBuilder,
			ControllerName:              "ctl-x",
			MountRoleName:               "rrsa-role-a",
			WorkerDepsStorageBucket:     "agentteams-workers-deps",
			WorkerDepsStorageEndpoint:   "https://oss-cn-hangzhou.aliyuncs.com",
			DynamicClient:               localDynamic,
			RemoteDynamicClientProvider: fakeRemoteDynamicProvider{client: remoteDynamic},
		},
	}
}

type workerTestGateway struct {
	modelInfo      *gateway.ModelProviderInfo
	modelErr       error
	authorized     []string
	authorizedAPIs []string
}

func (g *workerTestGateway) EnsureConsumer(context.Context, gateway.ConsumerRequest) (*gateway.ConsumerResult, error) {
	return &gateway.ConsumerResult{}, nil
}
func (g *workerTestGateway) DeleteConsumer(context.Context, string) error { return nil }
func (g *workerTestGateway) AuthorizeAIRoutes(_ context.Context, consumerName string, modelAPIID string) error {
	g.authorized = append(g.authorized, consumerName)
	g.authorizedAPIs = append(g.authorizedAPIs, modelAPIID)
	return nil
}
func (g *workerTestGateway) DeauthorizeAIRoutes(context.Context, string, string) error { return nil }
func (g *workerTestGateway) ExposePort(context.Context, gateway.PortExposeRequest) error {
	return nil
}
func (g *workerTestGateway) UnexposePort(context.Context, gateway.PortExposeRequest) error {
	return nil
}
func (g *workerTestGateway) EnsureServiceSource(context.Context, string, string, int, string) error {
	return nil
}
func (g *workerTestGateway) EnsureStaticServiceSource(context.Context, string, string, int) error {
	return nil
}
func (g *workerTestGateway) EnsureRoute(context.Context, string, []string, string, int, string) error {
	return nil
}
func (g *workerTestGateway) DeleteRoute(context.Context, string) error { return nil }
func (g *workerTestGateway) EnsureAIProvider(context.Context, gateway.AIProviderRequest) error {
	return nil
}
func (g *workerTestGateway) EnsureStreamIdleTimeout(context.Context, int) error { return nil }
func (g *workerTestGateway) EnsureAIRoute(context.Context, gateway.AIRouteRequest) error {
	return nil
}
func (g *workerTestGateway) ResolveModelProvider(context.Context, string) (*gateway.ModelProviderInfo, error) {
	return g.modelInfo, g.modelErr
}
func (g *workerTestGateway) Healthy(context.Context) error { return nil }

type workerTestAuthCacheInvalidator struct {
	calls int
}

func (i *workerTestAuthCacheInvalidator) InvalidateCache() {
	i.calls++
}

func testWorkerDepsSecret(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      "oss-cred",
			"namespace": namespace,
		},
		"data": map[string]interface{}{
			"akId":     base64.StdEncoding.EncodeToString([]byte("ak")),
			"akSecret": base64.StdEncoding.EncodeToString([]byte("sk")),
		},
	}}
}

func newWorkerTestDynamicClient(objs ...runtime.Object) dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		workerDepsCredentialProviderGVR: "CredentialProviderList",
		workerDepsAgentIdentityGVR:      "AgentIdentityList",
		workerDepsAgentRoleGVR:          "AgentRoleList",
		workerDepsAgentRoleBindingGVR:   "AgentRoleBindingList",
		workerDepsPVGVR:                 "PersistentVolumeList",
		workerDepsPVCGVR:                "PersistentVolumeClaimList",
		workerDepsStorageClassGVR:       "StorageClassList",
	}, objs...)
}

func (rig *workerTestRig) reconcile(name string) (*v1beta1.Worker, reconcile.Result, error) {
	rig.t.Helper()
	key := types.NamespacedName{Name: name, Namespace: "default"}
	res, err := rig.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	var out v1beta1.Worker
	if getErr := rig.client.Get(context.Background(), key, &out); getErr != nil {
		rig.t.Fatalf("get worker after reconcile: %v", getErr)
	}
	return &out, res, err
}

func newWorker(name string, spec v1beta1.WorkerSpec) *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
}

func testWorkerDepsVolumes() []v1beta1.WorkerVolumeSpec {
	return []v1beta1.WorkerVolumeSpec{{
		Name: "agentteams-prod-001",
		Type: v1beta1.WorkerVolumeTypeOSS,
		OSS: &v1beta1.WorkerOSSVolumeSpec{
			Bucket:   "agentteams-workers-deps",
			Endpoint: "https://oss-cn-hangzhou.aliyuncs.com",
			Auth: v1beta1.WorkerOSSAuthSpec{
				Type: "AccessKey",
				AccessKey: &v1beta1.WorkerAccessKeyAuthSpec{
					SecretRef: v1beta1.NamespacedSecretRef{Name: "oss-cred"},
				},
			},
		},
	}}
}

func testWorkerDepsMounts() []v1beta1.WorkerMountSpec {
	return []v1beta1.WorkerMountSpec{{
		Name:      "token",
		VolumeRef: "agentteams-prod-001",
		SubPath:   "instances/alice/token",
		MountPath: "/var/run/secrets/agentteams",
		ReadOnly:  true,
	}, {
		Name:      "env",
		VolumeRef: "agentteams-prod-001",
		SubPath:   "instances/alice/env",
		MountPath: "/mnt/agentteams/env",
		ReadOnly:  true,
	}, {
		Name:      "data",
		VolumeRef: "agentteams-prod-001",
		SubPath:   "instances/alice/data",
		MountPath: "/mnt/agentteams/data",
		ReadOnly:  false,
	}}
}

func TestPodBackendRejectsWorkerDepsMounts(t *testing.T) {
	_, _, _, err := prepareMemberWorkerDeps(
		context.Background(),
		MemberDeps{},
		MemberContext{
			Name:           "alice",
			RuntimeName:    "alice",
			BackendRuntime: v1beta1.BackendRuntimePod,
			Spec: v1beta1.WorkerSpec{
				Volumes: testWorkerDepsVolumes(),
				Mounts:  testWorkerDepsMounts(),
			},
		},
		map[string]string{},
		true,
	)
	if err == nil {
		t.Fatal("prepareMemberWorkerDeps succeeded with mounts in pod mode")
	}
	if !strings.Contains(err.Error(), "only supported when backendRuntime is sandbox") {
		t.Fatalf("error=%q, want pod unsupported error", err)
	}
}

// TestWorkerMemberContext_StampsControllerAndRoleLabels verifies that a
// standalone Worker CR's derived MemberContext carries agentteams.io/controller
// and agentteams.io/role=standalone so the resulting Pod is symmetric with
// Team-managed members and filterable by controller instance.
func TestWorkerMemberContext_StampsControllerAndRoleLabels(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}
	w := &v1beta1.Worker{}
	w.Name = "solo"
	w.Namespace = "hiclaw"

	mctx := r.workerMemberContext(w)

	if got := mctx.PodLabels[v1beta1.LabelController]; got != "ctl-x" {
		t.Fatalf("expected controller label ctl-x, got %q (labels=%v)", got, mctx.PodLabels)
	}
	if got := mctx.PodLabels["agentteams.io/role"]; got != RoleStandalone.String() {
		t.Fatalf("expected role %q, got %q", RoleStandalone.String(), got)
	}
	if _, ok := mctx.PodLabels["agentteams.io/team"]; ok {
		t.Fatalf("standalone worker must not carry agentteams.io/team, got %v", mctx.PodLabels)
	}
}

// TestWorkerMemberContext_MergesMetadataAndSpecLabels verifies the
// three-layer merge: CR metadata.labels, CR spec.labels, and the
// controller-forced system labels. spec.labels wins over metadata.labels
// on collision (per project decision — per-CR spec beats per-CR
// metadata) while non-conflicting entries from both layers survive.
func TestWorkerMemberContext_MergesMetadataAndSpecLabels(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}
	w := &v1beta1.Worker{}
	w.Name = "solo"
	w.Namespace = "hiclaw"
	w.ObjectMeta.Labels = map[string]string{
		"owner": "alice",
		"team":  "a",
	}
	w.Spec.Labels = map[string]string{
		"env":  "prod",
		"team": "b", // overrides metadata.labels["team"]
	}

	mctx := r.workerMemberContext(w)

	if got := mctx.PodLabels["owner"]; got != "alice" {
		t.Fatalf("metadata.labels[owner] not propagated: %v", mctx.PodLabels)
	}
	if got := mctx.PodLabels["env"]; got != "prod" {
		t.Fatalf("spec.labels[env] not propagated: %v", mctx.PodLabels)
	}
	if got := mctx.PodLabels["team"]; got != "b" {
		t.Fatalf("spec.labels must override metadata.labels on key collision, got team=%q", got)
	}
}

// TestWorkerMemberContext_SystemLabelsOverrideUser verifies reserved
// keys are silently overridden by controller system labels. Users
// cannot spoof agentteams.io/controller or agentteams.io/role by stuffing them
// into metadata.labels or spec.labels — this is the "reserved-override"
// contract.
func TestWorkerMemberContext_SystemLabelsOverrideUser(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "real-ctl"}
	w := &v1beta1.Worker{}
	w.Name = "solo"
	w.ObjectMeta.Labels = map[string]string{
		v1beta1.LabelController: "metadata-attacker",
	}
	w.Spec.Labels = map[string]string{
		v1beta1.LabelController: "spec-attacker",
		"agentteams.io/role":    "evil",
	}

	mctx := r.workerMemberContext(w)

	if got := mctx.PodLabels[v1beta1.LabelController]; got != "real-ctl" {
		t.Fatalf("system controller label must win over user, got %q (labels=%v)", got, mctx.PodLabels)
	}
	if got := mctx.PodLabels["agentteams.io/role"]; got != RoleStandalone.String() {
		t.Fatalf("system role label must win over user, got %q", got)
	}
}

// TestWorkerMemberContext_NilLabelsSafe ensures the merge helper
// handles the common case of a Worker CR that has neither
// metadata.labels nor spec.labels without panicking or emitting stray
// empty-map entries.
func TestWorkerMemberContext_NilLabelsSafe(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}
	w := &v1beta1.Worker{}
	w.Name = "solo"

	mctx := r.workerMemberContext(w)

	if mctx.PodLabels[v1beta1.LabelController] != "ctl-x" {
		t.Fatalf("controller label missing on nil-labels Worker: %v", mctx.PodLabels)
	}
	if len(mctx.PodLabels) != 2 {
		t.Fatalf("expected exactly the 2 system labels on nil-labels Worker, got %v", mctx.PodLabels)
	}
}

func TestSandboxWorkerRoutesToUnifiedSandboxClaim(t *testing.T) {
	backendRuntime := v1beta1.BackendRuntimeSandbox
	worker := newWorker("alice", v1beta1.WorkerSpec{
		Model:          "qwen-plus",
		Image:          "worker:set",
		WorkerName:     "alice-runtime",
		BackendRuntime: &backendRuntime,
	})

	rig := newWorkerRig(t, worker)
	rig.backend.NameOverride = "sandbox"
	gotWorker, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	req := rig.backend.Calls.CreateReqs[0]
	if gotWorker.Status.BackendRuntime != v1beta1.BackendRuntimeSandbox {
		t.Fatalf("status.backendRuntime=%q, want sandbox", gotWorker.Status.BackendRuntime)
	}
	if req.BackendRuntime != v1beta1.BackendRuntimeSandbox || req.SandboxSetName != backend.BuiltinSandboxInstanceName {
		t.Fatalf("create request backendRuntime/sandboxSetName=%q/%q", req.BackendRuntime, req.SandboxSetName)
	}
	if len(rig.r.Provisioner.(*mocks.MockProvisioner).Calls.EnsureRemoteSA) != 0 {
		t.Fatalf("sandbox claim remote path must not create target-cluster SA: %v", rig.r.Provisioner.(*mocks.MockProvisioner).Calls.EnsureRemoteSA)
	}
	if len(rig.provisioner.Calls.EnsureRemoteNamespace) != 0 {
		t.Fatalf("local sandbox path must not ensure remote namespace, calls=%v", rig.provisioner.Calls.EnsureRemoteNamespace)
	}
	tokenCalls := rig.r.Provisioner.(*mocks.MockProvisioner).Calls.RequestSATokenWithExpiration
	if len(tokenCalls) != 1 || tokenCalls[0].WorkerName != "alice" || tokenCalls[0].ExpirationSeconds != 3600 {
		t.Fatalf("short-lived token requests=%v", tokenCalls)
	}
	if len(rig.deployer.Calls.PrepareWorkerDeps) != 1 {
		t.Fatalf("PrepareWorkerDeps calls=%v", rig.deployer.Calls.PrepareWorkerDeps)
	}
	depsReq := rig.deployer.Calls.PrepareWorkerDeps[0]
	if depsReq.WorkerName != "alice-runtime" || !depsReq.UseToken || !depsReq.UseEnv {
		t.Fatalf("PrepareWorkerDeps request=%+v", depsReq)
	}
	if _, ok := depsReq.Env["AGENTTEAMS_CLUSTER_ID"]; ok {
		t.Fatalf("PrepareWorkerDeps AGENTTEAMS_CLUSTER_ID must not be set, got %q", depsReq.Env["AGENTTEAMS_CLUSTER_ID"])
	}
	if _, ok := depsReq.Env["AGENTTEAMS_AUTH_MODE"]; ok {
		t.Fatalf("PrepareWorkerDeps AGENTTEAMS_AUTH_MODE must not be set, got %q", depsReq.Env["AGENTTEAMS_AUTH_MODE"])
	}
	if depsReq.TokenSubPath != "workers-deps/alice-runtime/token" ||
		depsReq.EnvSubPath != "workers-deps/alice-runtime/env" ||
		depsReq.DataSubPath != "workers-deps/alice-runtime/data" {
		t.Fatalf("PrepareWorkerDeps subPaths=%+v", depsReq)
	}
	if depsReq.Storage != nil {
		t.Fatalf("PrepareWorkerDeps Storage=%T, want nil so main workspace OSS is used", depsReq.Storage)
	}
	if req.WorkersDeps == nil || len(req.WorkersDeps.DynamicVolumeMounts) != 3 {
		t.Fatalf("WorkersDeps=%+v", req.WorkersDeps)
	}
	wantSubPaths := map[string]string{
		"/var/run/secrets/agentteams": "workers-deps/alice-runtime/token",
		"/mnt/agentteams/env":         "workers-deps/alice-runtime/env",
		"/mnt/agentteams/data":        "workers-deps/alice-runtime/data",
	}
	wantCredProviders := map[string]string{
		"/var/run/secrets/agentteams": "agentteams-token",
		"/mnt/agentteams/env":         "agentteams-env",
		"/mnt/agentteams/data":        "agentteams-data",
	}
	for _, mount := range req.WorkersDeps.DynamicVolumeMounts {
		if mount.PVName != backend.BuiltinSandboxInstanceName {
			t.Fatalf("dynamic mount PVName=%q, want shared instance PV", mount.PVName)
		}
		if want := wantSubPaths[mount.MountPath]; mount.SubPath != want {
			t.Fatalf("dynamic mount subPath=%q, want %q for %+v", mount.SubPath, want, mount)
		}
		if got := mount.Attributes["credentialProviderName"]; got != wantCredProviders[mount.MountPath] {
			t.Fatalf("dynamic mount attributes=%v, want credentialProviderName=%s", mount.Attributes, wantCredProviders[mount.MountPath])
		}
		if _, ok := mount.Attributes["credProviderName"]; ok {
			t.Fatalf("legacy credProviderName should be omitted: %v", mount.Attributes)
		}
		switch mount.MountPath {
		case "/var/run/secrets/agentteams", "/mnt/agentteams/env":
			if !mount.ReadOnly {
				t.Fatalf("%s mount should be read-only: %+v", mount.MountPath, mount)
			}
		case "/mnt/agentteams/data":
			if mount.ReadOnly {
				t.Fatalf("data mount should be read-write: %+v", mount)
			}
		default:
			t.Fatalf("unexpected dynamic volume mount: %+v", mount)
		}
	}
	if req.WorkersDeps.InplaceUpdateImage != "worker:set" {
		t.Fatalf("inplace image=%q", req.WorkersDeps.InplaceUpdateImage)
	}
	pv, err := rig.r.DynamicClient.Resource(workerDepsPVGVR).Get(context.Background(), backend.BuiltinSandboxInstanceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get workers-deps PV: %v", err)
	}
	if pv.GetNamespace() != "" {
		t.Fatalf("PV namespace=%q, want cluster-scoped resource", pv.GetNamespace())
	}
	if labels := pv.GetLabels(); labels["alicloud-pvname"] != backend.BuiltinSandboxInstanceName {
		t.Fatalf("PV alicloud-pvname label=%q, want volume name", labels["alicloud-pvname"])
	}
	if storage, _, _ := unstructured.NestedString(pv.Object, "spec", "capacity", "storage"); storage != "50Gi" {
		t.Fatalf("PV storage=%q, want 50Gi", storage)
	}
	if storageClass, _, _ := unstructured.NestedString(pv.Object, "spec", "storageClassName"); storageClass != "test" {
		t.Fatalf("PV storageClassName=%q, want test", storageClass)
	}
	if volumeMode, _, _ := unstructured.NestedString(pv.Object, "spec", "volumeMode"); volumeMode != "Filesystem" {
		t.Fatalf("PV volumeMode=%q, want Filesystem", volumeMode)
	}
	if _, ok, _ := unstructured.NestedMap(pv.Object, "spec", "csi", "nodePublishSecretRef"); ok {
		t.Fatalf("RRSA PV should not set nodePublishSecretRef: %+v", pv.Object["spec"])
	}
	if driver, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "driver"); driver != ackOSSCSIProvisioner {
		t.Fatalf("PV csi.driver=%q, want %s", driver, ackOSSCSIProvisioner)
	}
	if handle, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeHandle"); handle != backend.BuiltinSandboxInstanceName {
		t.Fatalf("PV volumeHandle=%q, want volume name", handle)
	}
	if bucket, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeAttributes", "bucket"); bucket != "agentteams-workers-deps" {
		t.Fatalf("PV bucket=%q, want agentteams-workers-deps", bucket)
	}
	if endpoint, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeAttributes", "url"); endpoint != "https://oss-cn-hangzhou.aliyuncs.com" {
		t.Fatalf("PV endpoint=%q", endpoint)
	}
	if opts, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeAttributes", "otherOpts"); opts != "-o umask=022 -o allow_other" {
		t.Fatalf("PV otherOpts=%q", opts)
	}
	if authType, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeAttributes", "authType"); authType != "agent-identity" {
		t.Fatalf("PV authType=%q, want agent-identity", authType)
	}
	for _, name := range []string{"agentteams-env", "agentteams-token", "agentteams-data"} {
		cp, err := rig.r.DynamicClient.Resource(workerDepsCredentialProviderGVR).Namespace("default").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get worker-deps CredentialProvider %s: %v", name, err)
		}
		if cp.GetNamespace() != "default" {
			t.Fatalf("CredentialProvider %s namespace=%q, want namespace default", name, cp.GetNamespace())
		}
		if roleName, _, _ := unstructured.NestedString(cp.Object, "spec", "ram", "source", "rrsa", "roleName"); roleName != "rrsa-role-a" {
			t.Fatalf("CredentialProvider %s roleName=%q, want rrsa-role-a", name, roleName)
		}
		policy, _, _ := unstructured.NestedString(cp.Object, "spec", "ram", "source", "rrsa", "policy")
		assertWorkerDepsCredentialProviderPolicy(t, policy)
	}
	assertWorkerDepsAgentIdentityResources(t, rig.r.DynamicClient, "default")
	if _, err := rig.r.DynamicClient.Resource(workerDepsStorageClassGVR).Get(context.Background(), backend.BuiltinSandboxInstanceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("built-in worker-deps should not create StorageClass, err=%v", err)
	}
	if _, err := rig.r.DynamicClient.Resource(workerDepsPVCGVR).Namespace("default").Get(context.Background(), backend.BuiltinSandboxInstanceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("built-in worker-deps should not create PVC, err=%v", err)
	}
}

func TestSandboxWorkerAccessKeyMountAuthUsesSecretPV(t *testing.T) {
	backendRuntime := v1beta1.BackendRuntimeSandbox
	worker := newWorker("alice", v1beta1.WorkerSpec{
		Model:          "qwen-plus",
		Image:          "worker:set",
		WorkerName:     "alice-runtime",
		BackendRuntime: &backendRuntime,
	})

	rig := newWorkerRig(t, worker)
	rig.backend.NameOverride = "sandbox"
	rig.r.MountAuthType = "AccessKey"
	rig.r.MountRoleName = ""
	if _, _, err := rig.reconcile("alice"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	req := rig.backend.Calls.CreateReqs[0]
	if req.WorkersDeps == nil || len(req.WorkersDeps.DynamicVolumeMounts) != 3 {
		t.Fatalf("WorkersDeps=%+v", req.WorkersDeps)
	}
	for _, mount := range req.WorkersDeps.DynamicVolumeMounts {
		if len(mount.Attributes) != 0 {
			t.Fatalf("AccessKey dynamic mount should not have attributes: %+v", mount)
		}
	}
	pv, err := rig.r.DynamicClient.Resource(workerDepsPVGVR).Get(context.Background(), backend.BuiltinSandboxInstanceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get workers-deps PV: %v", err)
	}
	if pv.GetNamespace() != "" {
		t.Fatalf("PV namespace=%q, want cluster-scoped resource", pv.GetNamespace())
	}
	if secretNamespace, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "nodePublishSecretRef", "namespace"); secretNamespace != "default" {
		t.Fatalf("PV secret namespace=%q, want namespace default", secretNamespace)
	}
	if secretName, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "nodePublishSecretRef", "name"); secretName != backend.BuiltinSandboxInstanceName {
		t.Fatalf("PV secret name=%q, want %s", secretName, backend.BuiltinSandboxInstanceName)
	}
	if authType, ok, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeAttributes", "authType"); ok || authType != "" {
		t.Fatalf("AccessKey PV should not set RRSA authType, got %q", authType)
	}
	if _, err := rig.r.DynamicClient.Resource(workerDepsCredentialProviderGVR).Namespace("default").Get(context.Background(), "agentteams-token", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("AccessKey worker-deps should not create CredentialProvider, err=%v", err)
	}
	if _, err := rig.r.DynamicClient.Resource(workerDepsAgentIdentityGVR).Namespace("default").Get(context.Background(), backend.BuiltinSandboxInstanceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("AccessKey worker-deps should not create AgentIdentity, err=%v", err)
	}
}

func assertWorkerDepsCredentialProviderPolicy(t *testing.T, raw string) {
	t.Helper()
	type policyStatement struct {
		Action    []string                       `json:"Action"`
		Effect    string                         `json:"Effect"`
		Resource  []string                       `json:"Resource"`
		Condition map[string]map[string][]string `json:"Condition,omitempty"`
	}
	var policy struct {
		Statement []policyStatement `json:"Statement"`
		Version   string            `json:"Version"`
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		t.Fatalf("CredentialProvider policy is not valid JSON: %v\n%s", err, raw)
	}
	if policy.Version != "1" {
		t.Fatalf("policy Version=%q, want 1", policy.Version)
	}
	if len(policy.Statement) != 2 {
		t.Fatalf("policy statements=%+v, want 2", policy.Statement)
	}
	rw := policy.Statement[0]
	if rw.Effect != "Allow" {
		t.Fatalf("rw statement effect=%q, want Allow", rw.Effect)
	}
	if !equalStringSlices(rw.Action, []string{
		"oss:GetObject",
		"oss:PutObject",
		"oss:DeleteObject",
		"oss:AbortMultipartUpload",
		"oss:ListMultipartUploads",
	}) {
		t.Fatalf("rw actions=%v", rw.Action)
	}
	if !equalStringSlices(rw.Resource, []string{
		"acs:oss:*:*:${ack:agent-identity/storage-auth/bucket-name}/${ack:agent-identity/storage-auth/sub-path}",
		"acs:oss:*:*:${ack:agent-identity/storage-auth/bucket-name}/${ack:agent-identity/storage-auth/sub-path}/*",
	}) {
		t.Fatalf("rw resources=%v", rw.Resource)
	}
	list := policy.Statement[1]
	if !equalStringSlices(list.Action, []string{"oss:ListObjects"}) {
		t.Fatalf("list actions=%v", list.Action)
	}
	if !equalStringSlices(list.Resource, []string{"acs:oss:*:*:${ack:agent-identity/storage-auth/bucket-name}"}) {
		t.Fatalf("list resources=%v", list.Resource)
	}
	prefixes := list.Condition["StringLike"]["oss:Prefix"]
	if !equalStringSlices(prefixes, []string{"${ack:agent-identity/storage-auth/sub-path}/*"}) {
		t.Fatalf("list condition=%v", list.Condition)
	}
}

func sortedObjectNames(items []unstructured.Unstructured) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.GetName())
	}
	sort.Strings(names)
	return names
}

func pathBase(p string) string {
	return path.Base(p)
}

func assertWorkerDepsAgentIdentityResources(t *testing.T, dyn dynamic.Interface, namespace string) {
	t.Helper()
	identity, err := dyn.Resource(workerDepsAgentIdentityGVR).Namespace(namespace).Get(context.Background(), backend.BuiltinSandboxInstanceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get AgentIdentity: %v", err)
	}
	if desc, _, _ := unstructured.NestedString(identity.Object, "spec", "description"); desc != "this is for agentteams" {
		t.Fatalf("AgentIdentity description=%q", desc)
	}
	role, err := dyn.Resource(workerDepsAgentRoleGVR).Namespace(namespace).Get(context.Background(), backend.BuiltinSandboxInstanceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get AgentRole: %v", err)
	}
	rules, _, _ := unstructured.NestedSlice(role.Object, "spec", "rules")
	gotResources := make([]string, 0, len(rules))
	for _, raw := range rules {
		rule, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("AgentRole rule=%#v", raw)
		}
		if rule["effect"] != "Allow" || rule["action"] != "GetResourceCredential" {
			t.Fatalf("AgentRole rule=%#v", rule)
		}
		gotResources = append(gotResources, fmt.Sprint(rule["resource"]))
	}
	sort.Strings(gotResources)
	if !equalStringSlices(gotResources, []string{
		"CredentialProvider/agentteams-data",
		"CredentialProvider/agentteams-env",
		"CredentialProvider/agentteams-token",
	}) {
		t.Fatalf("AgentRole resources=%v", gotResources)
	}
	binding, err := dyn.Resource(workerDepsAgentRoleBindingGVR).Namespace(namespace).Get(context.Background(), backend.BuiltinSandboxInstanceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get AgentRoleBinding: %v", err)
	}
	if roleName, _, _ := unstructured.NestedString(binding.Object, "spec", "agentRoleRef", "name"); roleName != backend.BuiltinSandboxInstanceName {
		t.Fatalf("AgentRoleBinding role ref=%q", roleName)
	}
	subjects, _, _ := unstructured.NestedSlice(binding.Object, "spec", "subjects")
	if len(subjects) != 1 {
		t.Fatalf("AgentRoleBinding subjects=%v", subjects)
	}
	subject, ok := subjects[0].(map[string]interface{})
	if !ok || subject["authorizationType"] != "Agent" {
		t.Fatalf("AgentRoleBinding subject=%#v", subjects[0])
	}
	cfg, ok := subject["agentAuthorizationConfiguration"].(map[string]interface{})
	if !ok || cfg["agentName"] != backend.BuiltinSandboxInstanceName {
		t.Fatalf("AgentRoleBinding subject config=%#v", subject)
	}
}

func TestUpdateWorkerDepsCredentialProviderSkipsNoopUpdate(t *testing.T) {
	volume := v1beta1.WorkerVolumeSpec{
		Name: backend.BuiltinSandboxInstanceName,
		Type: v1beta1.WorkerVolumeTypeOSS,
		OSS: &v1beta1.WorkerOSSVolumeSpec{
			Bucket:   "agentteams-workers-deps",
			Endpoint: "https://oss-cn-hangzhou.aliyuncs.com",
			Auth: v1beta1.WorkerOSSAuthSpec{
				Type: workerDepsAuthTypeRRSA,
				RRSA: &v1beta1.WorkerOSSRRSASpec{RoleName: "rrsa-role-a"},
			},
		},
	}
	namespace := "agents"
	desired := buildWorkerDepsCredentialProvider(volume, namespace, workerDepsMountToken)
	fakeClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), desired.DeepCopy())
	res := fakeClient.Resource(workerDepsCredentialProviderGVR).Namespace(namespace)
	existing, err := res.Get(context.Background(), desired.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get existing CredentialProvider: %v", err)
	}

	fakeClient.ClearActions()
	if err := updateWorkerDepsObjectIfNeeded(context.Background(), res, existing, desired); err != nil {
		t.Fatalf("updateWorkerDepsObjectIfNeeded noop: %v", err)
	}
	if actions := fakeClient.Actions(); len(actions) != 0 {
		t.Fatalf("noop update recorded actions=%v", actions)
	}

	changedVolume := volume
	changedVolume.OSS = volume.OSS.DeepCopy()
	changedVolume.OSS.Auth.RRSA = volume.OSS.Auth.RRSA.DeepCopy()
	changedVolume.OSS.Auth.RRSA.RoleName = "rrsa-role-b"
	changedDesired := buildWorkerDepsCredentialProvider(changedVolume, namespace, workerDepsMountToken)
	existing, err = res.Get(context.Background(), desired.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get existing CredentialProvider after noop: %v", err)
	}
	fakeClient.ClearActions()
	if err := updateWorkerDepsObjectIfNeeded(context.Background(), res, existing, changedDesired); err != nil {
		t.Fatalf("updateWorkerDepsObjectIfNeeded changed: %v", err)
	}
	actions := fakeClient.Actions()
	if len(actions) != 1 || actions[0].GetVerb() != "update" || actions[0].GetResource() != workerDepsCredentialProviderGVR {
		t.Fatalf("changed update actions=%v", actions)
	}
}

func TestSandboxWorkerRRSAMountAuthRequiresRoleName(t *testing.T) {
	backendRuntime := v1beta1.BackendRuntimeSandbox
	worker := newWorker("alice", v1beta1.WorkerSpec{
		Model:          "qwen-plus",
		Image:          "worker:set",
		BackendRuntime: &backendRuntime,
	})

	rig := newWorkerRig(t, worker)
	rig.backend.NameOverride = "sandbox"
	rig.r.MountRoleName = ""
	_, _, err := rig.reconcile("alice")
	if err == nil {
		t.Fatal("Reconcile succeeded without AGENTTEAMS_MOUNT_ROLE_NAME")
	}
	if !strings.Contains(err.Error(), "AGENTTEAMS_MOUNT_ROLE_NAME is required") {
		t.Fatalf("Reconcile error=%v, want role name validation", err)
	}
}

func TestSandboxWorkerIgnoresLegacyWorkerDepsMountEntries(t *testing.T) {
	backendRuntime := v1beta1.BackendRuntimeSandbox
	worker := newWorker("alice", v1beta1.WorkerSpec{
		Model:          "qwen-plus",
		Image:          "worker:set",
		WorkerName:     "alice-runtime",
		BackendRuntime: &backendRuntime,
		Volumes:        testWorkerDepsVolumes(),
		Mounts:         testWorkerDepsMounts(),
	})

	rig := newWorkerRig(t, worker)
	rig.backend.NameOverride = "sandbox"
	_, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	req := rig.backend.Calls.CreateReqs[0]
	if req.WorkersDeps == nil || len(req.WorkersDeps.DynamicVolumeMounts) != 3 {
		t.Fatalf("WorkersDeps=%+v", req.WorkersDeps)
	}
	for _, mount := range req.WorkersDeps.DynamicVolumeMounts {
		if mount.PVName != backend.BuiltinSandboxInstanceName {
			t.Fatalf("legacy mount was not normalized to instance PV: %+v", mount)
		}
		if strings.HasPrefix(mount.SubPath, "instances/alice/") {
			t.Fatalf("legacy subPath should be ignored, got %+v", mount)
		}
	}
	depsReq := rig.deployer.Calls.PrepareWorkerDeps[0]
	if depsReq.TokenSubPath != "workers-deps/alice-runtime/token" ||
		depsReq.EnvSubPath != "workers-deps/alice-runtime/env" ||
		depsReq.DataSubPath != "workers-deps/alice-runtime/data" {
		t.Fatalf("PrepareWorkerDeps subPaths=%+v", depsReq)
	}
	if _, err := rig.r.DynamicClient.Resource(workerDepsPVGVR).Get(context.Background(), "agentteams-prod-001", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy per-worker PV should not be created, err=%v", err)
	}
}

func TestSandboxWorkersShareInstancePV(t *testing.T) {
	backendRuntime := v1beta1.BackendRuntimeSandbox
	alice := newWorker("alice", v1beta1.WorkerSpec{
		Model:          "qwen-plus",
		Image:          "worker:set",
		WorkerName:     "alice-runtime",
		BackendRuntime: &backendRuntime,
	})
	bob := newWorker("bob", v1beta1.WorkerSpec{
		Model:          "qwen-plus",
		Image:          "worker:set",
		WorkerName:     "bob-runtime",
		BackendRuntime: &backendRuntime,
	})

	rig := newWorkerRig(t, alice, bob)
	rig.backend.NameOverride = "sandbox"
	if _, _, err := rig.reconcile("alice"); err != nil {
		t.Fatalf("reconcile alice: %v", err)
	}
	if _, _, err := rig.reconcile("bob"); err != nil {
		t.Fatalf("reconcile bob: %v", err)
	}
	if _, err := rig.r.DynamicClient.Resource(workerDepsPVGVR).Get(context.Background(), backend.BuiltinSandboxInstanceName, metav1.GetOptions{}); err != nil {
		t.Fatalf("get shared workers-deps PV: %v", err)
	}
	if _, err := rig.r.DynamicClient.Resource(workerDepsPVGVR).Get(context.Background(), "bob", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("per-worker PV should not be created, err=%v", err)
	}
	cps, err := rig.r.DynamicClient.Resource(workerDepsCredentialProviderGVR).Namespace("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list CredentialProviders: %v", err)
	}
	if got := sortedObjectNames(cps.Items); !equalStringSlices(got, []string{"agentteams-data", "agentteams-env", "agentteams-token"}) {
		t.Fatalf("CredentialProviders=%v, want agentteams data/env/token", got)
	}
	if len(rig.backend.Calls.CreateReqs) != 2 {
		t.Fatalf("CreateReqs=%v", rig.backend.Calls.CreateReqs)
	}
	for _, req := range rig.backend.Calls.CreateReqs {
		if req.WorkersDeps == nil || len(req.WorkersDeps.DynamicVolumeMounts) != 3 {
			t.Fatalf("WorkersDeps for %s=%+v", req.Name, req.WorkersDeps)
		}
		runtimeName := req.Name + "-runtime"
		for _, mount := range req.WorkersDeps.DynamicVolumeMounts {
			if mount.PVName != backend.BuiltinSandboxInstanceName {
				t.Fatalf("worker %s mount PVName=%q, want shared PV", req.Name, mount.PVName)
			}
			wantCredProvider := "agentteams-" + pathBase(mount.SubPath)
			if got := mount.Attributes["credentialProviderName"]; got != wantCredProvider {
				t.Fatalf("worker %s mount attributes=%v, want %s", req.Name, mount.Attributes, wantCredProvider)
			}
			if _, ok := mount.Attributes["credProviderName"]; ok {
				t.Fatalf("worker %s legacy credProviderName should be omitted: %v", req.Name, mount.Attributes)
			}
			if !strings.HasPrefix(mount.SubPath, "workers-deps/"+runtimeName+"/") {
				t.Fatalf("worker %s mount subPath=%q, want EffectiveWorkerName-specific subPath", req.Name, mount.SubPath)
			}
		}
	}
}

func TestSandboxWorkerAddsCustomOSSMount(t *testing.T) {
	backendRuntime := v1beta1.BackendRuntimeSandbox
	worker := newWorker("alice", v1beta1.WorkerSpec{
		Model:          "qwen-plus",
		Image:          "worker:set",
		WorkerName:     "alice-runtime",
		BackendRuntime: &backendRuntime,
		Volumes: []v1beta1.WorkerVolumeSpec{{
			Name: "external-assets",
			Type: v1beta1.WorkerVolumeTypeOSS,
			OSS: &v1beta1.WorkerOSSVolumeSpec{
				Bucket:   "external-bucket",
				Endpoint: "https://oss-external.aliyuncs.com",
				Auth: v1beta1.WorkerOSSAuthSpec{
					Type: "AccessKey",
					AccessKey: &v1beta1.WorkerAccessKeyAuthSpec{
						SecretRef: v1beta1.NamespacedSecretRef{Name: "external-oss-cred"},
					},
				},
			},
		}},
		Mounts: []v1beta1.WorkerMountSpec{{
			Name:      "assets",
			VolumeRef: "external-assets",
			SubPath:   "teams/a/assets",
			MountPath: "/mnt/external/assets",
			ReadOnly:  true,
		}},
	})

	rig := newWorkerRig(t, worker)
	rig.backend.NameOverride = "sandbox"
	_, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	req := rig.backend.Calls.CreateReqs[0]
	if req.WorkersDeps == nil || len(req.WorkersDeps.DynamicVolumeMounts) != 4 {
		t.Fatalf("WorkersDeps=%+v", req.WorkersDeps)
	}
	var customMount *backend.WorkerDepsDynamicVolumeMount
	for i := range req.WorkersDeps.DynamicVolumeMounts {
		mount := &req.WorkersDeps.DynamicVolumeMounts[i]
		if mount.MountPath == "/mnt/external/assets" {
			customMount = mount
			break
		}
	}
	if customMount == nil {
		t.Fatalf("custom mount missing from WorkersDeps=%+v", req.WorkersDeps.DynamicVolumeMounts)
	}
	if customMount.PVName != "external-assets" || customMount.SubPath != "teams/a/assets" || !customMount.ReadOnly {
		t.Fatalf("custom mount=%+v", customMount)
	}
	if len(rig.deployer.Calls.PrepareWorkerDeps) != 1 {
		t.Fatalf("PrepareWorkerDeps calls=%v", rig.deployer.Calls.PrepareWorkerDeps)
	}
	pv, err := rig.r.DynamicClient.Resource(workerDepsPVGVR).Get(context.Background(), "external-assets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get custom PV: %v", err)
	}
	if pv.GetNamespace() != "" {
		t.Fatalf("custom PV namespace=%q, want cluster-scoped resource", pv.GetNamespace())
	}
	if secretNamespace, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "nodePublishSecretRef", "namespace"); secretNamespace != "default" {
		t.Fatalf("custom PV secret namespace=%q, want default", secretNamespace)
	}
	if secretName, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "nodePublishSecretRef", "name"); secretName != "external-oss-cred" {
		t.Fatalf("custom PV secret name=%q", secretName)
	}
	if bucket, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeAttributes", "bucket"); bucket != "external-bucket" {
		t.Fatalf("custom PV bucket=%q", bucket)
	}
}

func TestSandboxWorkerRejectsAccessKeySecretNamespace(t *testing.T) {
	backendRuntime := v1beta1.BackendRuntimeSandbox
	spec := v1beta1.WorkerSpec{
		Model:          "qwen-plus",
		Image:          "worker:set",
		BackendRuntime: &backendRuntime,
		Volumes:        testWorkerDepsVolumes(),
		Mounts:         testWorkerDepsMounts(),
	}
	spec.Volumes[0].OSS.Auth.AccessKey.SecretRef.Namespace = "sandbox-system"
	worker := newWorker("alice", spec)

	rig := newWorkerRig(t, worker)
	rig.backend.NameOverride = "sandbox"
	_, _, err := rig.reconcile("alice")
	if err == nil {
		t.Fatal("Reconcile succeeded with accessKey.secretRef.namespace set")
	}
	if !strings.Contains(err.Error(), "secretRef.namespace must be empty") || !strings.Contains(err.Error(), "targetNamespace") {
		t.Fatalf("Reconcile error=%v, want secretRef.namespace validation", err)
	}
}

func TestSandboxWorkerRejectsCustomMountOverBuiltinPath(t *testing.T) {
	backendRuntime := v1beta1.BackendRuntimeSandbox
	worker := newWorker("alice", v1beta1.WorkerSpec{
		Model:          "qwen-plus",
		Image:          "worker:set",
		BackendRuntime: &backendRuntime,
		Volumes: []v1beta1.WorkerVolumeSpec{{
			Name: "external-assets",
			Type: v1beta1.WorkerVolumeTypeOSS,
			OSS: &v1beta1.WorkerOSSVolumeSpec{
				Bucket:   "external-bucket",
				Endpoint: "https://oss-external.aliyuncs.com",
				Auth: v1beta1.WorkerOSSAuthSpec{
					Type: "AccessKey",
					AccessKey: &v1beta1.WorkerAccessKeyAuthSpec{
						SecretRef: v1beta1.NamespacedSecretRef{Name: "external-oss-cred"},
					},
				},
			},
		}},
		Mounts: []v1beta1.WorkerMountSpec{{
			Name:      "assets",
			VolumeRef: "external-assets",
			SubPath:   "teams/a/assets",
			MountPath: "/mnt/agentteams/env/custom",
			ReadOnly:  true,
		}},
	})

	rig := newWorkerRig(t, worker)
	rig.backend.NameOverride = "sandbox"
	_, _, err := rig.reconcile("alice")
	if err == nil {
		t.Fatal("Reconcile succeeded with custom mount over built-in path")
	}
	if !strings.Contains(err.Error(), "overlaps built-in worker-deps mount paths") {
		t.Fatalf("Reconcile error=%v, want built-in path overlap validation", err)
	}
}

func TestSandboxClaimRefreshesWorkerDepsForRunningWorker(t *testing.T) {
	sandboxBackend := mocks.NewMockWorkerBackend()
	sandboxBackend.NameOverride = "sandbox"
	if _, err := sandboxBackend.Create(context.Background(), backend.CreateRequest{Name: "alice"}); err != nil {
		t.Fatalf("seed sandbox backend: %v", err)
	}
	sandboxBackend.ClearCalls()

	provisioner := mocks.NewMockProvisioner()
	deployer := mocks.NewMockDeployer()
	mctx := MemberContext{
		Name:                 "alice",
		RuntimeName:          "alice-runtime",
		Namespace:            "default",
		Spec:                 v1beta1.WorkerSpec{Model: "qwen-plus", Image: "worker:set"},
		BackendRuntime:       v1beta1.BackendRuntimeSandbox,
		StatusBackendRuntime: v1beta1.BackendRuntimeSandbox,
		AppliedSpecHash:      "hash",
		CurrentSpecHash:      "hash",
	}
	sandboxSetTokenProjections.Delete(sandboxSetTokenProjectionKey(mctx))
	defer sandboxSetTokenProjections.Delete(sandboxSetTokenProjectionKey(mctx))
	deps := MemberDeps{
		Provisioner:                provisioner,
		Deployer:                   deployer,
		EnvBuilder:                 mocks.NewMockEnvBuilder(),
		Backend:                    backend.NewRegistry([]backend.WorkerBackend{sandboxBackend}),
		DynamicClient:              dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		AuthTokenExpirationSeconds: 900,
		ControllerName:             "ctl-x",
		MountRoleName:              "rrsa-role-a",
		WorkerDepsStorageBucket:    "agentteams-workers-deps",
		WorkerDepsStorageEndpoint:  "https://oss-cn-hangzhou.aliyuncs.com",
	}
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token", GatewayKey: "gateway-key"},
	}

	if _, err := ensureMemberContainerPresent(context.Background(), deps, mctx, state); err != nil {
		t.Fatalf("ensureMemberContainerPresent: %v", err)
	}
	if len(sandboxBackend.Calls.Create) != 0 {
		t.Fatalf("running sandbox Worker should refresh deps without recreating, create calls=%v", sandboxBackend.Calls.Create)
	}
	if len(provisioner.Calls.RequestSATokenWithExpiration) != 1 {
		t.Fatalf("token refresh calls=%v", provisioner.Calls.RequestSATokenWithExpiration)
	}
	if provisioner.Calls.RequestSATokenWithExpiration[0].ExpirationSeconds != 900 {
		t.Fatalf("token expiration=%d, want configured 900", provisioner.Calls.RequestSATokenWithExpiration[0].ExpirationSeconds)
	}
	if state.RequeueAfter <= 0 {
		t.Fatalf("state.RequeueAfter=%v, want token projection refresh schedule", state.RequeueAfter)
	}
	if len(deployer.Calls.PrepareWorkerDeps) != 1 {
		t.Fatalf("PrepareWorkerDeps calls=%v", deployer.Calls.PrepareWorkerDeps)
	}
	req := deployer.Calls.PrepareWorkerDeps[0]
	if req.WorkerName != "alice-runtime" || req.Token == "" || !req.UseToken || !req.UseEnv {
		t.Fatalf("PrepareWorkerDeps request=%+v", req)
	}
	if req.Storage != nil {
		t.Fatalf("PrepareWorkerDeps Storage=%T, want nil so main workspace OSS is used", req.Storage)
	}
}

func TestSandboxClaimRefreshWritesEnvWhenTokenNotDue(t *testing.T) {
	sandboxBackend := mocks.NewMockWorkerBackend()
	sandboxBackend.NameOverride = "sandbox"
	if _, err := sandboxBackend.Create(context.Background(), backend.CreateRequest{Name: "alice"}); err != nil {
		t.Fatalf("seed sandbox backend: %v", err)
	}
	sandboxBackend.ClearCalls()

	provisioner := mocks.NewMockProvisioner()
	deployer := mocks.NewMockDeployer()
	mctx := MemberContext{
		Name:                 "alice",
		RuntimeName:          "alice-runtime",
		Namespace:            "default",
		Spec:                 v1beta1.WorkerSpec{Model: "qwen-plus", Image: "worker:set"},
		BackendRuntime:       v1beta1.BackendRuntimeSandbox,
		StatusBackendRuntime: v1beta1.BackendRuntimeSandbox,
		AppliedSpecHash:      "hash",
		CurrentSpecHash:      "hash",
	}
	sandboxSetTokenProjections.Store(sandboxSetTokenProjectionKey(mctx), sandboxSetTokenProjectionState{
		NextRefresh: time.Now().Add(15 * time.Minute),
		Expiration:  time.Now().Add(30 * time.Minute),
	})
	defer sandboxSetTokenProjections.Delete(sandboxSetTokenProjectionKey(mctx))
	deps := MemberDeps{
		Provisioner:               provisioner,
		Deployer:                  deployer,
		EnvBuilder:                mocks.NewMockEnvBuilder(),
		Backend:                   backend.NewRegistry([]backend.WorkerBackend{sandboxBackend}),
		DynamicClient:             dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		ControllerName:            "ctl-x",
		MountRoleName:             "rrsa-role-a",
		WorkerDepsStorageBucket:   "agentteams-workers-deps",
		WorkerDepsStorageEndpoint: "https://oss-cn-hangzhou.aliyuncs.com",
	}
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token", GatewayKey: "gateway-key"},
	}

	if _, err := ensureMemberContainerPresent(context.Background(), deps, mctx, state); err != nil {
		t.Fatalf("ensureMemberContainerPresent: %v", err)
	}
	if len(provisioner.Calls.RequestSATokenWithExpiration) != 0 {
		t.Fatalf("token refresh calls=%v, want none before refresh window", provisioner.Calls.RequestSATokenWithExpiration)
	}
	if len(deployer.Calls.PrepareWorkerDeps) != 1 {
		t.Fatalf("PrepareWorkerDeps calls=%v", deployer.Calls.PrepareWorkerDeps)
	}
	req := deployer.Calls.PrepareWorkerDeps[0]
	if req.WorkerName != "alice-runtime" || !req.UseEnv || req.UseToken {
		t.Fatalf("PrepareWorkerDeps request=%+v, want env-only refresh", req)
	}
	if req.EnvSubPath == "" || req.DataSubPath == "" || req.TokenSubPath != "" {
		t.Fatalf("PrepareWorkerDeps paths=%+v", req)
	}
	if req.Env["AGENTTEAMS_AUTH_TOKEN_FILE"] != "/var/run/secrets/agentteams/token" {
		t.Fatalf("env AGENTTEAMS_AUTH_TOKEN_FILE=%q", req.Env["AGENTTEAMS_AUTH_TOKEN_FILE"])
	}
	if req.Env["AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED"] != "1" {
		t.Fatalf("env AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED=%q", req.Env["AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED"])
	}
}

func TestSandboxClaimTokenRefreshFailureKeepsValidProjectedToken(t *testing.T) {
	sandboxBackend := mocks.NewMockWorkerBackend()
	sandboxBackend.NameOverride = "sandbox"
	if _, err := sandboxBackend.Create(context.Background(), backend.CreateRequest{Name: "alice"}); err != nil {
		t.Fatalf("seed sandbox backend: %v", err)
	}
	sandboxBackend.ClearCalls()

	provisioner := mocks.NewMockProvisioner()
	provisioner.ProjectSATokenFn = func(context.Context, string, int64) (*service.SATokenProjection, error) {
		return nil, errors.New("temporary tokenrequest outage")
	}
	deployer := mocks.NewMockDeployer()
	mctx := MemberContext{
		Name:                 "alice",
		RuntimeName:          "alice-runtime",
		Namespace:            "default",
		Spec:                 v1beta1.WorkerSpec{Model: "qwen-plus", Image: "worker:set"},
		BackendRuntime:       v1beta1.BackendRuntimeSandbox,
		StatusBackendRuntime: v1beta1.BackendRuntimeSandbox,
		AppliedSpecHash:      "hash",
		CurrentSpecHash:      "hash",
	}
	sandboxSetTokenProjections.Store(sandboxSetTokenProjectionKey(mctx), sandboxSetTokenProjectionState{
		NextRefresh: time.Now().Add(-1 * time.Minute),
		Expiration:  time.Now().Add(20 * time.Minute),
	})
	defer sandboxSetTokenProjections.Delete(sandboxSetTokenProjectionKey(mctx))
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token", GatewayKey: "gateway-key"},
	}
	deps := MemberDeps{
		Provisioner:               provisioner,
		Deployer:                  deployer,
		EnvBuilder:                mocks.NewMockEnvBuilder(),
		Backend:                   backend.NewRegistry([]backend.WorkerBackend{sandboxBackend}),
		DynamicClient:             dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		ControllerName:            "ctl-x",
		MountRoleName:             "rrsa-role-a",
		WorkerDepsStorageBucket:   "agentteams-workers-deps",
		WorkerDepsStorageEndpoint: "https://oss-cn-hangzhou.aliyuncs.com",
	}

	res, err := ensureMemberContainerPresent(context.Background(), deps, mctx, state)
	if err != nil {
		t.Fatalf("ensureMemberContainerPresent: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter result=%v, want no immediate error requeue", res.RequeueAfter)
	}
	if state.RequeueAfter != sandboxSetTokenRetryAfter {
		t.Fatalf("state.RequeueAfter=%v, want %v", state.RequeueAfter, sandboxSetTokenRetryAfter)
	}
	if !strings.Contains(state.Message, "Degraded: sandbox claim token refresh failed") {
		t.Fatalf("state.Message=%q, want degraded token refresh message", state.Message)
	}
	if len(deployer.Calls.PrepareWorkerDeps) != 1 {
		t.Fatalf("PrepareWorkerDeps calls=%v, want env refresh without token rewrite", deployer.Calls.PrepareWorkerDeps)
	}
	req := deployer.Calls.PrepareWorkerDeps[0]
	if !req.UseEnv || req.UseToken || req.TokenSubPath != "" {
		t.Fatalf("PrepareWorkerDeps request=%+v, want env-only refresh", req)
	}
}

func TestSandboxWorkerRejectsStoragePrefixWithExtraObjectPath(t *testing.T) {
	sandboxBackend := mocks.NewMockWorkerBackend()
	sandboxBackend.NameOverride = "sandbox"
	provisioner := mocks.NewMockProvisioner()
	deployer := mocks.NewMockDeployer()
	envBuilder := mocks.NewMockEnvBuilder()
	envBuilder.BuildFn = func(workerName string, prov *service.WorkerProvisionResult) map[string]string {
		env := mocks.NewMockEnvBuilder().Build(workerName, prov)
		env["AGENTTEAMS_STORAGE_PREFIX"] = "agentteams/agentteams-workers-deps/custom-prefix"
		return env
	}
	mctx := MemberContext{
		Name:           "alice",
		RuntimeName:    "alice-runtime",
		Namespace:      "default",
		Spec:           v1beta1.WorkerSpec{Model: "qwen-plus", Image: "worker:set"},
		BackendRuntime: v1beta1.BackendRuntimeSandbox,
	}
	sandboxSetTokenProjections.Delete(sandboxSetTokenProjectionKey(mctx))
	defer sandboxSetTokenProjections.Delete(sandboxSetTokenProjectionKey(mctx))
	deps := MemberDeps{
		Provisioner:               provisioner,
		Deployer:                  deployer,
		EnvBuilder:                envBuilder,
		Backend:                   backend.NewRegistry([]backend.WorkerBackend{sandboxBackend}),
		DynamicClient:             dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		ControllerName:            "ctl-x",
		MountRoleName:             "rrsa-role-a",
		WorkerDepsStorageBucket:   "agentteams-workers-deps",
		WorkerDepsStorageEndpoint: "https://oss-cn-hangzhou.aliyuncs.com",
	}
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token", GatewayKey: "gateway-key"},
	}

	_, err := ensureMemberContainerPresent(context.Background(), deps, mctx, state)
	if err == nil {
		t.Fatal("expected storage prefix validation error")
	}
	if !strings.Contains(err.Error(), "AGENTTEAMS_STORAGE_PREFIX to be <alias>/<bucket>") {
		t.Fatalf("error=%v", err)
	}
	if len(deployer.Calls.PrepareWorkerDeps) != 0 {
		t.Fatalf("PrepareWorkerDeps calls=%v, want validation before writing", deployer.Calls.PrepareWorkerDeps)
	}
}

func TestWorkerDeploymentTargetImmutable(t *testing.T) {
	edge := v1beta1.DeployModeEdge
	w := newWorker("alice", v1beta1.WorkerSpec{Model: "qwen-plus"})
	w.Status.DeployMode = v1beta1.DeployModeLocal

	desired := v1beta1.WorkerSpec{
		Model:      "qwen-plus",
		DeployMode: &edge,
	}
	if err := validateWorkerDeploymentTargetImmutable(w, desired); err == nil {
		t.Fatal("deployMode change was allowed, want immutable error")
	}
}

func TestSleepingSandboxIsNotDeletedForSpecChange(t *testing.T) {
	sandboxBackend := mocks.NewMockWorkerBackend()
	sandboxBackend.NameOverride = "sandbox"
	sandboxBackend.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{
			Name:    "alice",
			Backend: "sandbox",
			Status:  backend.StatusSleeping,
		}, nil
	}
	mctx := MemberContext{
		Name:                 "alice",
		Spec:                 v1beta1.WorkerSpec{Model: "qwen-plus"},
		BackendRuntime:       v1beta1.BackendRuntimeSandbox,
		StatusBackendRuntime: v1beta1.BackendRuntimeSandbox,
		AppliedSpecHash:      "new-hash",
		CurrentSpecHash:      "old-hash",
		SpecChanged:          true,
	}
	deps := MemberDeps{
		Backend: backend.NewRegistry([]backend.WorkerBackend{sandboxBackend}),
	}
	state := &MemberState{}

	if _, err := ensureMemberContainerAbsent(context.Background(), deps, mctx, false, state); err != nil {
		t.Fatalf("ensureMemberContainerAbsent: %v", err)
	}
	if len(sandboxBackend.Calls.Delete) != 0 {
		t.Fatalf("sleeping sandbox should be left alone, delete calls=%v", sandboxBackend.Calls.Delete)
	}
}

// TestHashAppliedWorkerSpec_ExcludesServiceFields locks in that the
// service-only fields (ServiceEnabled, Expose) are excluded from the
// applied-spec hash. Toggling either field must NOT trigger sandbox/pod
// recreation — they are consumed by ReconcileMemberService independently.
func TestHashAppliedWorkerSpec_ExcludesServiceFields(t *testing.T) {
	base := v1beta1.WorkerSpec{
		Model: "gpt-4",
		Image: "test:v1",
	}
	baseHash := hashAppliedWorkerSpec(base)

	t.Run("toggle ServiceEnabled", func(t *testing.T) {
		enabled := true
		withService := base
		withService.ServiceEnabled = &enabled
		if got := hashAppliedWorkerSpec(withService); got != baseHash {
			t.Fatalf("ServiceEnabled must not affect hash: got %q, want %q", got, baseHash)
		}
	})

	t.Run("add Expose port", func(t *testing.T) {
		withExpose := base
		withExpose.Expose = []v1beta1.ExposePort{{Port: 8080, Protocol: "http"}}
		if got := hashAppliedWorkerSpec(withExpose); got != baseHash {
			t.Fatalf("Expose must not affect hash: got %q, want %q", got, baseHash)
		}
	})
}

func TestHashAppliedWorkerSpec_WorkerDepsLayoutVersionAffectsSandboxOrMounts(t *testing.T) {
	base := v1beta1.WorkerSpec{
		Model: "qwen-plus",
		Image: "test:v1",
	}
	if got, want := hashAppliedWorkerSpec(base), legacyHashAppliedWorkerSpecForTest(base); got != want {
		t.Fatalf("hash without mounts changed: got %q, want legacy direct spec hash %q", got, want)
	}
	sandboxRuntime := v1beta1.BackendRuntimeSandbox
	sandbox := base
	sandbox.BackendRuntime = &sandboxRuntime
	if got, want := workerDepsLayoutHashVersion(sandbox), workerDepsLayoutVersion; got != want {
		t.Fatalf("sandbox worker deps layout version=%q, want %q", got, want)
	}
	if got, legacy := hashAppliedWorkerSpec(sandbox), legacyHashAppliedWorkerSpecForTest(sandbox); got == legacy {
		t.Fatalf("sandbox hash should include worker deps layout version, got unchanged %q", got)
	}
	withMount := base
	withMount.Volumes = testWorkerDepsVolumes()
	withMount.Mounts = testWorkerDepsMounts()
	if got, want := workerDepsLayoutHashVersion(withMount), workerDepsLayoutVersion; got != want {
		t.Fatalf("worker deps layout version=%q, want %q", got, want)
	}
	if got, legacy := hashAppliedWorkerSpec(withMount), legacyHashAppliedWorkerSpecForTest(withMount); got == legacy {
		t.Fatalf("hash with mounts should include worker deps layout version, got unchanged %q", got)
	}
}

func legacyHashAppliedWorkerSpecForTest(spec v1beta1.WorkerSpec) string {
	spec.Model = ""
	spec.McpServers = nil
	spec.AccessEntries = nil
	spec.State = nil
	spec.IdleTimeout = ""
	spec.ServiceEnabled = nil
	spec.Expose = nil
	buf, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return fmt.Sprintf("%x", h.Sum64())
}

// TestHashAppliedWorkerSpec_IncludesPodFields asserts the inverse:
// pod-affecting fields (Image) DO change the hash so that the reconciler
// will recreate the underlying pod when they diverge.
func TestHashAppliedWorkerSpec_IncludesPodFields(t *testing.T) {
	base := v1beta1.WorkerSpec{
		Model: "gpt-4",
		Image: "test:v1",
	}
	baseHash := hashAppliedWorkerSpec(base)

	t.Run("change Image", func(t *testing.T) {
		changed := base
		changed.Image = "test:v2"
		if got := hashAppliedWorkerSpec(changed); got == baseHash {
			t.Fatalf("Image change must affect hash: got %q", got)
		}
	})
}

func TestHashAppliedWorkerSpec_ExcludesConfigAndPermissionOnlyFields(t *testing.T) {
	base := v1beta1.WorkerSpec{
		Model: "gpt-4",
		Image: "test:v1",
		McpServers: []v1beta1.MCPServer{{
			Name: "github",
			URL:  "https://aigw.example.com/mcp/github",
		}},
	}
	baseHash := hashAppliedWorkerSpec(base)

	changed := base
	changed.Model = "gpt-5"
	changed.McpServers = []v1beta1.MCPServer{{
		Name:      "jira",
		URL:       "https://aigw.example.com/mcp/jira",
		Transport: "sse",
	}}
	changed.AccessEntries = []v1beta1.AccessEntry{{
		Service:     "ai-registry",
		Permissions: []string{"read"},
	}}
	changed.AgentIdentity = &v1beta1.AgentIdentitySpec{WorkloadIdentityName: "wi-worker-a"}
	changed.CredentialBindings = []v1beta1.CredentialBinding{{
		CredentialRef: v1beta1.CredentialRef{
			TokenVaultName:               "default",
			APIKeyCredentialProviderName: "GITHUB_TOKEN",
		},
	}}
	if got := hashAppliedWorkerSpec(changed); got != baseHash {
		t.Fatalf("model, MCP, accessEntries, and credential contract changes must not affect hash: got %q, want %q", got, baseHash)
	}
}

func TestHashAppliedWorkerSpec_ExcludesLifecycleAndIdlePolicy(t *testing.T) {
	running := "Running"
	sleeping := "Sleeping"
	base := v1beta1.WorkerSpec{
		Model:       "qwen-plus",
		Runtime:     "copaw",
		Image:       "worker:v1",
		IdleTimeout: "15m",
		State:       &running,
	}

	changedPolicy := base
	changedPolicy.IdleTimeout = "1h"
	changedPolicy.State = &sleeping

	if got, want := hashAppliedWorkerSpec(changedPolicy), hashAppliedWorkerSpec(base); got != want {
		t.Fatalf("state and idleTimeout must not affect sandbox spec hash: got %q, want %q", got, want)
	}

	changedImage := base
	changedImage.Image = "worker:v2"
	if got, want := hashAppliedWorkerSpec(changedImage), hashAppliedWorkerSpec(base); got == want {
		t.Fatalf("pod-affecting fields must affect sandbox spec hash: got %q", got)
	}
}

func TestHashAppliedWorkerSpecForRuntimeQwenPawExcludesHotConfig(t *testing.T) {
	state := "Running"
	base := v1beta1.WorkerSpec{
		Runtime: "qwenpaw",
		Image:   "qwenpaw:v1",
		Model:   "qwen-plus",
		Package: "nacos://registry/ns/dev-worker?version=1",
		McpServers: []v1beta1.MCPServer{{
			Name: "github",
			URL:  "https://aigw.example.com/mcp/github",
		}},
		ChannelPolicy: &v1beta1.ChannelPolicySpec{GroupAllowExtra: []string{"@leader:matrix.local"}},
		Skills:        []string{"dev-plan"},
		RemoteSkills:  []v1beta1.RemoteSkillSource{{Source: "nacos://skills", Skills: []v1beta1.RemoteSkill{{Name: "dev-pr"}}}},
		Identity:      "identity",
		Soul:          "soul",
		Agents:        "agents",
		Channels: &v1beta1.ChannelsSpec{
			DingTalk: &v1beta1.DingTalkChannelSpec{
				Enabled:      workerBoolPtr(true),
				ClientID:     "demo-client-id",
				ClientSecret: "test-client-secret",
			},
		},
		IdleTimeout: "15m",
		State:       &state,
		Env:         map[string]string{"A": "1"},
	}
	baseHash := hashAppliedWorkerSpecForRuntime(base, "qwenpaw")

	configOnly := base
	configOnly.Model = "qwen-max"
	configOnly.Package = "nacos://registry/ns/dev-worker?version=2"
	configOnly.McpServers = []v1beta1.MCPServer{{Name: "jira", URL: "https://aigw.example.com/mcp/jira"}}
	configOnly.ChannelPolicy = &v1beta1.ChannelPolicySpec{DmAllowExtra: []string{"@admin:matrix.local"}}
	configOnly.Skills = []string{"dev-wiki"}
	configOnly.AccessEntries = []v1beta1.AccessEntry{{
		Service:     "ai-registry",
		Permissions: []string{"read"},
	}}
	configOnly.AgentIdentity = &v1beta1.AgentIdentitySpec{WorkloadIdentityName: "wi-worker-a"}
	configOnly.CredentialBindings = []v1beta1.CredentialBinding{{
		CredentialRef: v1beta1.CredentialRef{
			TokenVaultName:               "default",
			APIKeyCredentialProviderName: "GITHUB_TOKEN",
		},
	}}
	configOnly.Channels = &v1beta1.ChannelsSpec{
		DingTalk: &v1beta1.DingTalkChannelSpec{Enabled: workerBoolPtr(false)},
	}
	configOnly.Identity = "new identity"
	configOnly.Soul = "new soul"
	configOnly.Agents = "new agents"
	if got := hashAppliedWorkerSpecForRuntime(configOnly, "qwenpaw"); got != baseHash {
		t.Fatalf("qwenpaw hot config and accessEntries fields must not affect pod hash: got %q, want %q", got, baseHash)
	}

	changedImage := base
	changedImage.Image = "qwenpaw:v2"
	if got := hashAppliedWorkerSpecForRuntime(changedImage, "qwenpaw"); got == baseHash {
		t.Fatalf("qwenpaw image change must affect pod hash: got %q", got)
	}

	changedEnv := base
	changedEnv.Env = map[string]string{"A": "2"}
	if got := hashAppliedWorkerSpecForRuntime(changedEnv, "qwenpaw"); got == baseHash {
		t.Fatalf("qwenpaw env change must affect pod hash: got %q", got)
	}
}

func workerBoolPtr(v bool) *bool { return &v }

// TestWorkerMemberContext_SpecChangedGate locks in the brand-new-worker
// guard. The "brand new" case is the load-bearing one: a second reconcile
// queued by the finalizer write can read a stale informer cache and see
// the just-created container as Running while ObservedGeneration is still
// 0. Without the gate, SpecChanged=true on that intervening pass can delete
// the container right after first create.
func TestWorkerMemberContext_SpecChangedGate(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}

	cases := []struct {
		name     string
		gen      int64
		observed int64
		want     bool
	}{
		// Brand-new Worker: never reconciled. Must NOT report SpecChanged
		// even though Generation > ObservedGeneration — that delta is the
		// "we have never observed this resource" signal, not a user edit.
		{"brand_new", 1, 0, false},
		// First reconcile committed: no edit pending.
		{"observed_no_edit", 1, 1, false},
		// User edit after first reconcile: spec genuinely diverged.
		{"observed_with_edit", 2, 1, true},
		// Periodic resync with no spec change.
		{"resync_no_edit", 5, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &v1beta1.Worker{}
			w.Name = "solo"
			w.Generation = tc.gen
			w.Status.ObservedGeneration = tc.observed
			mctx := r.workerMemberContext(w)
			if mctx.SpecChanged != tc.want {
				t.Fatalf("SpecChanged for (gen=%d, observed=%d): got %v, want %v",
					tc.gen, tc.observed, mctx.SpecChanged, tc.want)
			}
		})
	}
}

func TestWorkerReconcilerLegacySkipsDecoupledTeamMemberStandaloneState(t *testing.T) {
	ctx := context.Background()
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@dev:matrix.local",
			RoomID:       "!room-dev:matrix.local",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: RoleTeamLeader.String()},
				{Name: "dev"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker.DeepCopy(), team.DeepCopy()).
		Build()
	legacy, fakeOSS := newTestLegacy(t)
	prov := mocks.NewMockProvisioner()
	prov.MatrixUserIDFn = func(name string) string {
		return "@" + name + ":matrix.local"
	}
	r := &WorkerReconciler{
		Client:      c,
		Provisioner: prov,
		Legacy:      legacy,
	}

	r.reconcileLegacy(ctx, worker, &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixUserID: "@dev:matrix.local"},
	})

	if _, err := fakeOSS.GetObject(ctx, "agents/manager/workers-registry.json"); err == nil {
		t.Fatalf("WorkerReconciler must not write standalone workers-registry rows for decoupled Team members")
	}
}

func TestWorkerMemberContextQwenPawConfigOnlyChangeDoesNotSetSpecChanged(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}
	baseSpec := v1beta1.WorkerSpec{
		Runtime: "qwenpaw",
		Image:   "qwenpaw:v1",
		Model:   "qwen-plus",
		Package: "nacos://registry/ns/dev-worker?version=1",
	}

	w := &v1beta1.Worker{}
	w.Name = "solo"
	w.Generation = 2
	w.Status.ObservedGeneration = 1
	w.Status.SpecHash = hashAppliedWorkerSpecForRuntime(baseSpec, "qwenpaw")
	w.Spec = baseSpec
	w.Spec.Model = "qwen-max"
	w.Spec.Package = "nacos://registry/ns/dev-worker?version=2"

	if mctx := r.workerMemberContext(w); mctx.SpecChanged {
		t.Fatalf("qwenpaw model/package changes should not recreate container")
	}

	w.Spec.Image = "qwenpaw:v2"
	if mctx := r.workerMemberContext(w); !mctx.SpecChanged {
		t.Fatalf("qwenpaw image change should recreate container")
	}
}

func TestWorkerReconcileQwenPawConfigOnlyUpdateWritesRuntimeConfigWithoutRecreate(t *testing.T) {
	worker := newWorker("solo", v1beta1.WorkerSpec{
		Runtime:    "qwenpaw",
		Image:      "agentteams/qwenpaw-worker:v1",
		WorkerName: "worker-a",
		Model:      "qwen-plus",
		Package:    "nacos://registry/ns/dev-worker?version=1",
		McpServers: []v1beta1.MCPServer{{
			Name: "github",
			URL:  "https://aigw.example.com/mcp/github",
		}},
		ChannelPolicy: &v1beta1.ChannelPolicySpec{
			GroupAllowExtra: []string{"@leader:matrix.local"},
		},
	})
	worker.Generation = 1
	rig := newWorkerRig(t, worker)

	out, _, err := rig.reconcile("solo")
	if err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	if out.Status.ObservedGeneration != 1 {
		t.Fatalf("initial ObservedGeneration=%d, want 1", out.Status.ObservedGeneration)
	}
	if out.Status.SpecHash == "" {
		t.Fatal("initial reconcile did not record qwenpaw pod spec hash")
	}
	initialPodHash := out.Status.SpecHash
	if got := len(rig.deployer.Calls.DeployMemberRuntimeConfig); got != 1 {
		t.Fatalf("initial runtime config calls=%d, want 1", got)
	}
	initialRuntimeConfig := rig.deployer.Calls.DeployMemberRuntimeConfig[0]
	if initialRuntimeConfig.RuntimeName != "worker-a" {
		t.Fatalf("initial runtimeName=%q, want worker-a", initialRuntimeConfig.RuntimeName)
	}
	if initialRuntimeConfig.Spec.Package != "nacos://registry/ns/dev-worker?version=1" {
		t.Fatalf("initial package=%q", initialRuntimeConfig.Spec.Package)
	}
	creates, deletes, _, _, _ := rig.backend.CallSnapshot()
	if len(creates) != 1 || creates[0] != "solo" {
		t.Fatalf("initial creates=%v, want [solo]", creates)
	}
	if len(deletes) != 0 {
		t.Fatalf("initial deletes=%v, want none", deletes)
	}

	rig.backend.ClearCalls()
	rig.deployer.ClearCalls()

	var live v1beta1.Worker
	key := types.NamespacedName{Name: "solo", Namespace: "default"}
	if err := rig.client.Get(context.Background(), key, &live); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	live.Spec.Model = "qwen-max"
	live.Spec.Package = "nacos://registry/ns/dev-worker?version=2"
	live.Spec.McpServers = []v1beta1.MCPServer{{
		Name: "jira",
		URL:  "https://aigw.example.com/mcp/jira",
	}}
	live.Spec.ChannelPolicy = &v1beta1.ChannelPolicySpec{
		DmAllowExtra: []string{"@admin:matrix.local"},
	}
	live.Spec.AgentIdentity = &v1beta1.AgentIdentitySpec{WorkloadIdentityName: "wi-worker-a"}
	live.Spec.CredentialBindings = []v1beta1.CredentialBinding{{
		CredentialRef: v1beta1.CredentialRef{
			TokenVaultName:               "default",
			APIKeyCredentialProviderName: "GITHUB_TOKEN",
		},
	}}
	live.Spec.Channels = &v1beta1.ChannelsSpec{
		DingTalk: &v1beta1.DingTalkChannelSpec{Enabled: workerBoolPtr(false)},
	}
	live.Generation = 2
	if err := rig.client.Update(context.Background(), &live); err != nil {
		t.Fatalf("update worker spec: %v", err)
	}

	out, _, err = rig.reconcile("solo")
	if err != nil {
		t.Fatalf("config-only reconcile: %v", err)
	}
	if out.Status.ObservedGeneration != 2 {
		t.Fatalf("updated ObservedGeneration=%d, want 2", out.Status.ObservedGeneration)
	}
	if out.Status.SpecHash != initialPodHash {
		t.Fatalf("qwenpaw config-only update changed pod spec hash: got %q, want %q", out.Status.SpecHash, initialPodHash)
	}
	if got := len(rig.deployer.Calls.DeployMemberRuntimeConfig); got != 1 {
		t.Fatalf("updated runtime config calls=%d, want 1", got)
	}
	updatedRuntimeConfig := rig.deployer.Calls.DeployMemberRuntimeConfig[0]
	if updatedRuntimeConfig.Spec.Model != "qwen-max" {
		t.Fatalf("updated model=%q, want qwen-max", updatedRuntimeConfig.Spec.Model)
	}
	if updatedRuntimeConfig.Spec.Package != "nacos://registry/ns/dev-worker?version=2" {
		t.Fatalf("updated package=%q", updatedRuntimeConfig.Spec.Package)
	}
	if len(updatedRuntimeConfig.Spec.McpServers) != 1 || updatedRuntimeConfig.Spec.McpServers[0].Name != "jira" {
		t.Fatalf("updated MCP servers=%v", updatedRuntimeConfig.Spec.McpServers)
	}
	if updatedRuntimeConfig.Spec.ChannelPolicy == nil || len(updatedRuntimeConfig.Spec.ChannelPolicy.DmAllowExtra) != 1 {
		t.Fatalf("updated channel policy=%#v", updatedRuntimeConfig.Spec.ChannelPolicy)
	}
	if updatedRuntimeConfig.Spec.AgentIdentity == nil || updatedRuntimeConfig.Spec.AgentIdentity.WorkloadIdentityName != "wi-worker-a" {
		t.Fatalf("updated agent identity=%#v", updatedRuntimeConfig.Spec.AgentIdentity)
	}
	if len(updatedRuntimeConfig.Spec.CredentialBindings) != 1 || updatedRuntimeConfig.Spec.CredentialBindings[0].CredentialRef.APIKeyCredentialProviderName != "GITHUB_TOKEN" {
		t.Fatalf("updated credential bindings=%#v", updatedRuntimeConfig.Spec.CredentialBindings)
	}
	if updatedRuntimeConfig.Spec.Channels == nil || updatedRuntimeConfig.Spec.Channels.DingTalk == nil ||
		updatedRuntimeConfig.Spec.Channels.DingTalk.Enabled == nil || *updatedRuntimeConfig.Spec.Channels.DingTalk.Enabled {
		t.Fatalf("updated DingTalk channels=%#v", updatedRuntimeConfig.Spec.Channels)
	}
	creates, deletes, _, _, _ = rig.backend.CallSnapshot()
	if len(creates) != 0 {
		t.Fatalf("config-only update recreated worker: creates=%v", creates)
	}
	if len(deletes) != 0 {
		t.Fatalf("config-only update deleted worker: deletes=%v", deletes)
	}
}

func TestWorkerReconcileEdgeWritesRuntimeConfigWithoutContainer(t *testing.T) {
	edgeMode := v1beta1.DeployModeEdge
	worker := newWorker("edge-worker-cr", v1beta1.WorkerSpec{
		DeployMode:    &edgeMode,
		Runtime:       "openclaw",
		WorkerName:    "claude-local",
		Model:         "claude-sonnet-4",
		ModelProvider: "claude-provider",
		Package:       "oss://agents/claude-local/packages/demo.zip",
	})
	worker.Generation = 1
	rig := newWorkerRig(t, worker)
	rig.envBuilder.BuildFn = func(workerName string, prov *service.WorkerProvisionResult) map[string]string {
		return map[string]string{
			"AGENTTEAMS_WORKER_NAME": workerName,
			"SKILLS_API_URL":         "nacos://market.agentteams.io:80/public",
			"NACOS_AUTH_TYPE":        "sts-hiclaw",
		}
	}
	gw := &workerTestGateway{modelInfo: &gateway.ModelProviderInfo{
		HttpApiID:   "model-api-1",
		IntranetURL: "http://aigw.internal/v1/claude",
	}}
	rig.r.GatewayClient = gw

	out, res, err := rig.reconcile("edge-worker-cr")
	if err != nil {
		t.Fatalf("edge reconcile: %v", err)
	}
	if res.RequeueAfter != edgeReconcileInterval {
		t.Fatalf("edge RequeueAfter=%v, want %v", res.RequeueAfter, edgeReconcileInterval)
	}
	if out.Status.ObservedGeneration != 1 {
		t.Fatalf("ObservedGeneration=%d, want 1", out.Status.ObservedGeneration)
	}
	if out.Status.MatrixUserID != "@claude-local:localhost" {
		t.Fatalf("MatrixUserID=%q", out.Status.MatrixUserID)
	}
	if out.Status.RoomID != "!room-claude-local:localhost" {
		t.Fatalf("RoomID=%q", out.Status.RoomID)
	}
	if got := len(rig.deployer.Calls.DeployMemberRuntimeConfig); got != 1 {
		t.Fatalf("runtime config calls=%d, want 1", got)
	}
	req := rig.deployer.Calls.DeployMemberRuntimeConfig[0]
	if req.Name != "edge-worker-cr" || req.RuntimeName != "claude-local" {
		t.Fatalf("unexpected runtime config identity: %#v", req)
	}
	if req.Runtime != runtimeRemoteManagedLocal {
		t.Fatalf("runtime=%q, want %q", req.Runtime, runtimeRemoteManagedLocal)
	}
	if req.MatrixAccessToken != "mock-token-claude-local" {
		t.Fatalf("MatrixAccessToken=%q", req.MatrixAccessToken)
	}
	if req.GatewayKey != "mock-gw-key-claude-local" {
		t.Fatalf("GatewayKey=%q", req.GatewayKey)
	}
	if req.AIGatewayURL != "http://aigw.internal/v1/claude" {
		t.Fatalf("AIGatewayURL=%q", req.AIGatewayURL)
	}
	if req.SkillRegistryURL != "nacos://market.agentteams.io:80/public" {
		t.Fatalf("SkillRegistryURL=%q", req.SkillRegistryURL)
	}
	if req.SkillRegistryAuthType != "sts-hiclaw" {
		t.Fatalf("SkillRegistryAuthType=%q", req.SkillRegistryAuthType)
	}
	if len(gw.authorized) != 1 || gw.authorized[0] != "worker-claude-local" || gw.authorizedAPIs[0] != "model-api-1" {
		t.Fatalf("authorized=%v apis=%v", gw.authorized, gw.authorizedAPIs)
	}
	creates, deletes, _, _, statuses := rig.backend.CallSnapshot()
	if len(creates) != 0 || len(deletes) != 0 || len(statuses) != 0 {
		t.Fatalf("edge worker must not touch container backend, creates=%v deletes=%v statuses=%v", creates, deletes, statuses)
	}
	if got := len(rig.provisioner.Calls.EnsureServiceAccount); got != 0 {
		t.Fatalf("edge worker must not create pod ServiceAccount during reconcile, got %v", rig.provisioner.Calls.EnsureServiceAccount)
	}
}

func TestWorkerReconcileEdgeUUIDRotationDeletesSAAndInvalidatesAuthCache(t *testing.T) {
	edgeMode := v1beta1.DeployModeEdge
	worker := newWorker("edge-worker-cr", v1beta1.WorkerSpec{
		DeployMode: &edgeMode,
		Runtime:    "openclaw",
		WorkerName: "claude-local",
		Model:      "claude-sonnet-4",
	})
	worker.Labels = map[string]string{
		v1beta1.LabelWorkerEdgeUUID: "new-uuid",
	}
	worker.Annotations = map[string]string{
		v1beta1.AnnotationEdgeAppliedUUID: "old-uuid",
	}
	invalidator := &workerTestAuthCacheInvalidator{}
	rig := newWorkerRig(t, worker)
	rig.r.AuthCache = invalidator

	out, _, err := rig.reconcile("edge-worker-cr")
	if err != nil {
		t.Fatalf("edge reconcile: %v", err)
	}
	if got := rig.provisioner.Calls.DeleteServiceAccount; len(got) != 1 || got[0] != "edge-worker-cr" {
		t.Fatalf("DeleteServiceAccount calls=%v, want [edge-worker-cr]", got)
	}
	if invalidator.calls != 1 {
		t.Fatalf("InvalidateCache calls=%d, want 1", invalidator.calls)
	}
	if got := out.Annotations[v1beta1.AnnotationEdgeAppliedUUID]; got != "new-uuid" {
		t.Fatalf("applied uuid=%q, want new-uuid", got)
	}
}

func TestWorkerReconcileEdgePreservesHeartbeatRunningPhase(t *testing.T) {
	edgeMode := v1beta1.DeployModeEdge
	worker := newWorker("edge-worker-cr", v1beta1.WorkerSpec{
		DeployMode:    &edgeMode,
		Runtime:       "openclaw",
		WorkerName:    "claude-local",
		Model:         "claude-sonnet-4",
		ModelProvider: "claude-provider",
	})
	worker.Generation = 1
	worker.Status.Phase = "Running"
	worker.Status.LastHeartbeat = time.Now().UTC().Format(time.RFC3339)
	rig := newWorkerRig(t, worker)
	rig.r.GatewayClient = &workerTestGateway{modelInfo: &gateway.ModelProviderInfo{
		HttpApiID:   "model-api-1",
		IntranetURL: "http://aigw.internal/v1/claude",
	}}

	out, _, err := rig.reconcile("edge-worker-cr")
	if err != nil {
		t.Fatalf("edge reconcile: %v", err)
	}
	if out.Status.Phase != "Running" {
		t.Fatalf("phase=%q, want Running", out.Status.Phase)
	}
	if out.Status.ContainerState != "" {
		t.Fatalf("containerState=%q, want empty", out.Status.ContainerState)
	}
}

func TestWorkerReconcileEdgeStaleHeartbeatReturnsPending(t *testing.T) {
	edgeMode := v1beta1.DeployModeEdge
	worker := newWorker("edge-worker-cr", v1beta1.WorkerSpec{
		DeployMode:    &edgeMode,
		Runtime:       "openclaw",
		WorkerName:    "claude-local",
		Model:         "claude-sonnet-4",
		ModelProvider: "claude-provider",
	})
	worker.Generation = 1
	worker.Status.Phase = "Running"
	worker.Status.LastHeartbeat = time.Now().UTC().Add(-(edgeHeartbeatTimeout + time.Minute)).Format(time.RFC3339)
	rig := newWorkerRig(t, worker)
	rig.r.GatewayClient = &workerTestGateway{modelInfo: &gateway.ModelProviderInfo{
		HttpApiID:   "model-api-1",
		IntranetURL: "http://aigw.internal/v1/claude",
	}}

	out, res, err := rig.reconcile("edge-worker-cr")
	if err != nil {
		t.Fatalf("edge reconcile: %v", err)
	}
	if res.RequeueAfter != edgeReconcileInterval {
		t.Fatalf("edge RequeueAfter=%v, want %v", res.RequeueAfter, edgeReconcileInterval)
	}
	if out.Status.Phase != "Pending" {
		t.Fatalf("phase=%q, want Pending", out.Status.Phase)
	}
	if out.Status.ContainerState != "" {
		t.Fatalf("containerState=%q, want empty", out.Status.ContainerState)
	}
}

func TestWorkerReconcileConfigOnlyUpdateWritesOSSConfigWithoutRecreate(t *testing.T) {
	worker := newWorker("solo", v1beta1.WorkerSpec{
		Runtime: "openclaw",
		Image:   "agentteams/worker:v1",
		Model:   "qwen-plus",
		McpServers: []v1beta1.MCPServer{{
			Name: "github",
			URL:  "https://aigw.example.com/mcp/github",
		}},
	})
	worker.Generation = 1
	rig := newWorkerRig(t, worker)

	out, _, err := rig.reconcile("solo")
	if err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	if out.Status.SpecHash == "" {
		t.Fatal("initial reconcile did not record pod spec hash")
	}
	initialPodHash := out.Status.SpecHash
	creates, deletes, _, _, _ := rig.backend.CallSnapshot()
	if len(creates) != 1 || creates[0] != "solo" {
		t.Fatalf("initial creates=%v, want [solo]", creates)
	}
	if len(deletes) != 0 {
		t.Fatalf("initial deletes=%v, want none", deletes)
	}

	rig.backend.ClearCalls()
	rig.deployer.ClearCalls()

	var live v1beta1.Worker
	key := types.NamespacedName{Name: "solo", Namespace: "default"}
	if err := rig.client.Get(context.Background(), key, &live); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	live.Spec.Model = "qwen-max"
	live.Spec.McpServers = []v1beta1.MCPServer{{
		Name:      "jira",
		URL:       "https://aigw.example.com/mcp/jira",
		Transport: "sse",
	}}
	live.Generation = 2
	if err := rig.client.Update(context.Background(), &live); err != nil {
		t.Fatalf("update worker spec: %v", err)
	}

	out, _, err = rig.reconcile("solo")
	if err != nil {
		t.Fatalf("config-only reconcile: %v", err)
	}
	if out.Status.ObservedGeneration != 2 {
		t.Fatalf("updated ObservedGeneration=%d, want 2", out.Status.ObservedGeneration)
	}
	if out.Status.SpecHash != initialPodHash {
		t.Fatalf("config-only update changed pod spec hash: got %q, want %q", out.Status.SpecHash, initialPodHash)
	}
	if got := len(rig.deployer.Calls.DeployWorkerConfig); got != 1 {
		t.Fatalf("updated worker config calls=%d, want 1", got)
	}
	updatedConfig := rig.deployer.Calls.DeployWorkerConfig[0]
	if updatedConfig.Spec.Model != "qwen-max" {
		t.Fatalf("updated model=%q, want qwen-max", updatedConfig.Spec.Model)
	}
	if len(updatedConfig.McpServers) != 1 || updatedConfig.McpServers[0].Name != "jira" {
		t.Fatalf("updated MCP servers=%v", updatedConfig.McpServers)
	}
	creates, deletes, _, _, _ = rig.backend.CallSnapshot()
	if len(creates) != 0 {
		t.Fatalf("config-only update recreated worker: creates=%v", creates)
	}
	if len(deletes) != 0 {
		t.Fatalf("config-only update deleted worker: deletes=%v", deletes)
	}
}

func TestWorkerReconcileQwenPawPodAffectingUpdateRecreatesWorker(t *testing.T) {
	worker := newWorker("solo", v1beta1.WorkerSpec{
		Runtime:    "qwenpaw",
		Image:      "agentteams/qwenpaw-worker:v1",
		WorkerName: "worker-a",
		Model:      "qwen-plus",
		Package:    "nacos://registry/ns/dev-worker?version=1",
	})
	worker.Generation = 1
	rig := newWorkerRig(t, worker)

	out, _, err := rig.reconcile("solo")
	if err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	initialPodHash := out.Status.SpecHash
	if initialPodHash == "" {
		t.Fatal("initial reconcile did not record qwenpaw pod spec hash")
	}

	rig.backend.ClearCalls()
	rig.deployer.ClearCalls()

	var live v1beta1.Worker
	key := types.NamespacedName{Name: "solo", Namespace: "default"}
	if err := rig.client.Get(context.Background(), key, &live); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	live.Spec.Image = "agentteams/qwenpaw-worker:v2"
	live.Generation = 2
	if err := rig.client.Update(context.Background(), &live); err != nil {
		t.Fatalf("update worker spec: %v", err)
	}

	out, res, err := rig.reconcile("solo")
	if err != nil {
		t.Fatalf("pod-affecting reconcile: %v", err)
	}
	if res.RequeueAfter != reconcileRetryDelay {
		t.Fatalf("first update RequeueAfter=%v, want %v", res.RequeueAfter, reconcileRetryDelay)
	}
	if out.Status.SpecHash != initialPodHash {
		t.Fatalf("specHash advanced before replacement create: got %q, want %q", out.Status.SpecHash, initialPodHash)
	}
	if got := len(rig.deployer.Calls.DeployMemberRuntimeConfig); got != 1 {
		t.Fatalf("updated runtime config calls=%d, want 1", got)
	}
	creates, deletes, _, _, _ := rig.backend.CallSnapshot()
	if len(deletes) != 1 || deletes[0] != "solo" {
		t.Fatalf("first update deletes=%v, want [solo]", deletes)
	}
	if len(creates) != 0 {
		t.Fatalf("first update must wait before create, creates=%v", creates)
	}

	rig.backend.ClearCalls()
	rig.deployer.ClearCalls()

	out, _, err = rig.reconcile("solo")
	if err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	if out.Status.SpecHash == initialPodHash {
		t.Fatalf("qwenpaw image update did not change pod spec hash after replacement create: %q", out.Status.SpecHash)
	}
	creates, deletes, _, _, _ = rig.backend.CallSnapshot()
	if len(deletes) != 0 {
		t.Fatalf("replacement reconcile deletes=%v, want none", deletes)
	}
	if len(creates) != 1 || creates[0] != "solo" {
		t.Fatalf("replacement reconcile creates=%v, want [solo]", creates)
	}
}

func TestWorkerReconcileSandboxImageUpdateWaitsForDeleteBeforeCreate(t *testing.T) {
	backendRuntime := v1beta1.BackendRuntimeSandbox
	worker := newWorker("solo", v1beta1.WorkerSpec{
		Runtime:        "openclaw",
		Image:          "agentteams/worker:v1",
		Model:          "qwen-plus",
		BackendRuntime: &backendRuntime,
	})
	worker.Generation = 1
	rig := newWorkerRig(t, worker)
	rig.backend.NameOverride = "sandbox"

	out, _, err := rig.reconcile("solo")
	if err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	initialPodHash := out.Status.SpecHash
	if initialPodHash == "" {
		t.Fatal("initial reconcile did not record sandbox pod spec hash")
	}

	rig.backend.ClearCalls()
	rig.deployer.ClearCalls()

	var live v1beta1.Worker
	key := types.NamespacedName{Name: "solo", Namespace: "default"}
	if err := rig.client.Get(context.Background(), key, &live); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	live.Spec.Image = "agentteams/worker:v2"
	live.Generation = 2
	if err := rig.client.Update(context.Background(), &live); err != nil {
		t.Fatalf("update worker spec: %v", err)
	}

	out, res, err := rig.reconcile("solo")
	if err != nil {
		t.Fatalf("sandbox image update reconcile: %v", err)
	}
	if res.RequeueAfter != reconcileRetryDelay {
		t.Fatalf("first update RequeueAfter=%v, want %v", res.RequeueAfter, reconcileRetryDelay)
	}
	if out.Status.SpecHash != initialPodHash {
		t.Fatalf("specHash advanced before replacement SandboxClaim: got %q, want %q", out.Status.SpecHash, initialPodHash)
	}
	if out.Status.ContainerState != "stopping" {
		t.Fatalf("ContainerState=%q, want stopping", out.Status.ContainerState)
	}
	creates, deletes, _, _, _ := rig.backend.CallSnapshot()
	if len(deletes) != 1 || deletes[0] != "solo" {
		t.Fatalf("first update deletes=%v, want [solo]", deletes)
	}
	if len(creates) != 0 {
		t.Fatalf("first update must wait before create, creates=%v", creates)
	}

	rig.backend.ClearCalls()
	rig.deployer.ClearCalls()

	out, _, err = rig.reconcile("solo")
	if err != nil {
		t.Fatalf("sandbox replacement reconcile: %v", err)
	}
	if out.Status.SpecHash == initialPodHash {
		t.Fatalf("specHash did not advance after replacement create: %q", out.Status.SpecHash)
	}
	creates, deletes, _, _, _ = rig.backend.CallSnapshot()
	if len(deletes) != 0 {
		t.Fatalf("replacement reconcile deletes=%v, want none", deletes)
	}
	if len(creates) != 1 || creates[0] != "solo" {
		t.Fatalf("replacement reconcile creates=%v, want [solo]", creates)
	}
}

func TestReconcileWorkerSvcLabel(t *testing.T) {
	t.Run("adds label when svcName is non-empty", func(t *testing.T) {
		w := &v1beta1.Worker{}
		w.Name = "alice"
		changed := reconcileWorkerSvcLabel(w, "alice")
		if !changed {
			t.Fatal("expected label change, got false")
		}
		if got := w.Labels[v1beta1.LabelWorkerSvcName]; got != "alice" {
			t.Fatalf("label = %q, want alice", got)
		}
	})

	t.Run("no-op when label already matches", func(t *testing.T) {
		w := &v1beta1.Worker{}
		w.Labels = map[string]string{v1beta1.LabelWorkerSvcName: "alice"}
		changed := reconcileWorkerSvcLabel(w, "alice")
		if changed {
			t.Fatal("expected no change when label already matches")
		}
	})

	t.Run("removes label when svcName is empty", func(t *testing.T) {
		w := &v1beta1.Worker{}
		w.Labels = map[string]string{v1beta1.LabelWorkerSvcName: "alice", "other": "keep"}
		changed := reconcileWorkerSvcLabel(w, "")
		if !changed {
			t.Fatal("expected label removal, got false")
		}
		if _, exists := w.Labels[v1beta1.LabelWorkerSvcName]; exists {
			t.Fatal("label should have been removed")
		}
		if w.Labels["other"] != "keep" {
			t.Fatal("unrelated label should be preserved")
		}
	})

	t.Run("no-op when label absent and svcName empty", func(t *testing.T) {
		w := &v1beta1.Worker{}
		w.Labels = map[string]string{"other": "keep"}
		changed := reconcileWorkerSvcLabel(w, "")
		if changed {
			t.Fatal("expected no change when label already absent")
		}
	})
}

// TestReconcileWorkerSvcLabel_PatchBaseOrder locks in the patch-base
// ordering invariant in Reconcile: the base snapshot must be taken
// BEFORE reconcileWorkerSvcLabel mutates w. Otherwise client.MergeFrom
// computes an empty diff and the label change is silently dropped.
func TestReconcileWorkerSvcLabel_PatchBaseOrder(t *testing.T) {
	w := &v1beta1.Worker{}
	w.Name = "bob"
	w.Labels = map[string]string{"existing": "val"}

	// Correct order: snapshot base before mutation.
	base := w.DeepCopy()
	changed := reconcileWorkerSvcLabel(w, "bob")
	if !changed {
		t.Fatal("expected change")
	}
	// base must NOT carry the new label — proves MergeFrom would emit a
	// non-empty diff when applied against w.
	if _, exists := base.Labels[v1beta1.LabelWorkerSvcName]; exists {
		t.Fatal("base should not contain the new label")
	}
	if w.Labels[v1beta1.LabelWorkerSvcName] != "bob" {
		t.Fatalf("w.Labels should contain new label, got %v", w.Labels)
	}
}

// TestWorkerReconcile_ModelProviderNotFound_CreateFails verifies that a Worker
// with spec.modelProvider set fails reconciliation when the gateway returns
// model-provider-not-found. This ensures misconfigured Workers don't get
// silently created without AI Gateway access.
func TestWorkerReconcile_ModelProviderNotFound_CreateFails(t *testing.T) {
	worker := newWorker("worker-bad-mp", v1beta1.WorkerSpec{
		Runtime:       "qwenpaw",
		ModelProvider: "dashscope",
	})
	rig := newWorkerRig(t, worker)
	rig.r.GatewayClient = &workerTestGateway{
		modelErr: fmt.Errorf("ai-gateway: model provider %q not found", "dashscope"),
	}

	_, _, err := rig.reconcile("worker-bad-mp")
	if err == nil {
		t.Fatal("expected reconcile to fail when model provider not found, got nil error")
	}
	if !strings.Contains(err.Error(), "resolve model provider") {
		t.Fatalf("error should mention model provider resolution, got: %v", err)
	}
	if !strings.Contains(err.Error(), "dashscope") {
		t.Fatalf("error should mention the provider name, got: %v", err)
	}

	// Verify the Worker status reflects the failure.
	var out v1beta1.Worker
	if getErr := rig.client.Get(context.Background(), types.NamespacedName{Name: "worker-bad-mp", Namespace: "default"}, &out); getErr != nil {
		t.Fatalf("get worker: %v", getErr)
	}
	if out.Status.Phase != "Failed" {
		t.Fatalf("expected phase=Failed, got %q", out.Status.Phase)
	}
	if out.Status.ObservedGeneration != 0 {
		t.Fatalf("ObservedGeneration should remain 0 on failure, got %d", out.Status.ObservedGeneration)
	}
}
