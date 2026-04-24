package accessresolver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/credprovider"
)

const testNS = "hiclaw"

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func rawJSON(t *testing.T, v any) *apiextensionsv1.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &apiextensionsv1.JSON{Raw: b}
}

func TestResolveWorker_DefaultEntries(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "alice"
	worker.Namespace = testNS
	c := newFakeClient(t, worker)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	session, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "alice", WorkerName: "alice",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if session != "hiclaw-worker-alice" {
		t.Fatalf("session = %q", session)
	}
	// Standalone workers now default to a single object-storage entry
	// that folds agents/<name>/* + shared/* together, mirroring the
	// embedded MinIO policy which grants both prefixes RW.
	if len(entries) != 1 {
		t.Fatalf("expected 1 default entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Service != credprovider.ServiceObjectStorage {
		t.Fatalf("service = %q", e.Service)
	}
	if e.Scope.Bucket != "hiclaw-test" {
		t.Fatalf("bucket not resolved: %+v", e.Scope)
	}
	if !hasPrefix(e.Scope.Prefixes, "agents/alice/*") {
		t.Fatalf("expected agents/alice/* prefix, got %+v", e.Scope.Prefixes)
	}
	if !hasPrefix(e.Scope.Prefixes, "shared/*") {
		t.Fatalf("expected shared/* prefix, got %+v", e.Scope.Prefixes)
	}
	if !hasAllPerms(e.Permissions, "read", "write", "list", "delete") {
		t.Fatalf("expected RW shared/* permissions, got %+v", e.Permissions)
	}
}

func hasPrefix(prefixes []string, want string) bool {
	for _, p := range prefixes {
		if p == want {
			return true
		}
	}
	return false
}

func hasAllPerms(perms []string, want ...string) bool {
	set := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		set[p] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

func TestResolveWorker_CustomBucketRef(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "bob"
	worker.Namespace = testNS
	worker.Spec.AccessEntries = []v1beta1.AccessEntry{
		{
			Service:     credprovider.ServiceObjectStorage,
			Permissions: []string{"read"},
			Scope: rawJSON(t, map[string]any{
				"bucketRef": "workspace",
				"prefixes":  []string{"custom/${self.name}/*"},
			}),
		},
	}
	c := newFakeClient(t, worker)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	_, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "bob", WorkerName: "bob",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	got := entries[0]
	if got.Scope.Bucket != "hiclaw-test" {
		t.Fatalf("bucket = %q", got.Scope.Bucket)
	}
	if got.Scope.Prefixes[0] != "custom/bob/*" {
		t.Fatalf("prefix = %q", got.Scope.Prefixes[0])
	}
}

func TestResolveWorker_UnknownService(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "eve"
	worker.Namespace = testNS
	worker.Spec.AccessEntries = []v1beta1.AccessEntry{
		{Service: "nonsense", Scope: rawJSON(t, map[string]any{})},
	}
	c := newFakeClient(t, worker)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	_, _, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "eve", WorkerName: "eve",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported service") {
		t.Fatalf("expected unsupported-service error, got: %v", err)
	}
}

func TestResolveWorker_ObjectStorageMissingPrefixes(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "dave"
	worker.Namespace = testNS
	worker.Spec.AccessEntries = []v1beta1.AccessEntry{
		{
			Service: credprovider.ServiceObjectStorage,
			Scope:   rawJSON(t, map[string]any{"bucket": "other"}),
		},
	}
	c := newFakeClient(t, worker)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	_, _, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "dave", WorkerName: "dave",
	})
	if err == nil || !strings.Contains(err.Error(), "prefixes is empty") {
		t.Fatalf("expected prefixes-empty error, got: %v", err)
	}
}

func TestResolveManager_Defaults(t *testing.T) {
	mgr := &v1beta1.Manager{}
	mgr.Name = "manager"
	mgr.Namespace = testNS
	c := newFakeClient(t, mgr)

	r := New(c, testNS, "hiclaw-test", "gw-1", auth.DefaultResourcePrefix)
	session, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleManager, Username: "manager",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if session != "hiclaw-manager-manager" {
		t.Fatalf("session = %q", session)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 default entry, got %d", len(entries))
	}
	prefixes := entries[0].Scope.Prefixes
	wantManager := false
	for _, p := range prefixes {
		if p == "manager/*" {
			wantManager = true
		}
	}
	if !wantManager {
		t.Fatalf("manager default entries missing 'manager/*': %+v", prefixes)
	}
}

func TestResolve_AIGatewayHappyPath(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "gw-bot"
	worker.Namespace = testNS
	worker.Spec.AccessEntries = []v1beta1.AccessEntry{
		{
			Service:     credprovider.ServiceAIGateway,
			Permissions: []string{"read", "write"},
			Scope: rawJSON(t, map[string]any{
				"gatewayRef": "default",
				"resources":  []string{"consumers/*", "routes/*"},
			}),
		},
	}
	c := newFakeClient(t, worker)

	r := New(c, testNS, "hiclaw-test", "gw-abc123", auth.DefaultResourcePrefix)
	_, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "gw-bot", WorkerName: "gw-bot",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	got := entries[0]
	if got.Service != credprovider.ServiceAIGateway {
		t.Fatalf("service = %q", got.Service)
	}
	if got.Scope.GatewayID != "gw-abc123" {
		t.Fatalf("gatewayId = %q", got.Scope.GatewayID)
	}
	if len(got.Scope.Resources) != 2 {
		t.Fatalf("resources = %+v", got.Scope.Resources)
	}
}

func TestResolve_AIRegistryDefaultResources(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "nacos-w"
	worker.Namespace = testNS
	worker.Spec.AccessEntries = []v1beta1.AccessEntry{
		{
			Service:     credprovider.ServiceAIRegistry,
			Permissions: []string{"read", "write"},
			Scope:       rawJSON(t, map[string]any{"namespaceId": "gw-abc123"}),
		},
	}
	c := newFakeClient(t, worker)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	_, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "nacos-w", WorkerName: "nacos-w",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	got := entries[0]
	if got.Service != credprovider.ServiceAIRegistry {
		t.Fatalf("service = %q", got.Service)
	}
	if got.Scope.NamespaceID != "gw-abc123" {
		t.Fatalf("namespaceId = %q", got.Scope.NamespaceID)
	}
	if len(got.Scope.Resources) != 2 || got.Scope.Resources[0] != "agentSpec/*" || got.Scope.Resources[1] != "skill/*" {
		t.Fatalf("resources = %+v", got.Scope.Resources)
	}
}

func TestResolve_AIRegistryCustomResources(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "nacos-w2"
	worker.Namespace = testNS
	worker.Spec.AccessEntries = []v1beta1.AccessEntry{
		{
			Service: credprovider.ServiceAIRegistry,
			Scope: rawJSON(t, map[string]any{
				"namespaceId": "ns1",
				"resources":   []string{"agentspec/*", "mcp/*"},
			}),
		},
	}
	c := newFakeClient(t, worker)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	_, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "nacos-w2", WorkerName: "nacos-w2",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := entries[0]
	if got.Scope.NamespaceID != "ns1" {
		t.Fatalf("namespaceId = %q", got.Scope.NamespaceID)
	}
	if len(got.Scope.Resources) != 2 || got.Scope.Resources[0] != "agentspec/*" {
		t.Fatalf("resources = %+v", got.Scope.Resources)
	}
}

func TestResolve_AIGatewayNoDefault(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "gw-bot2"
	worker.Namespace = testNS
	worker.Spec.AccessEntries = []v1beta1.AccessEntry{
		{
			Service: credprovider.ServiceAIGateway,
			Scope:   rawJSON(t, map[string]any{"gatewayRef": "default"}),
		},
	}
	c := newFakeClient(t, worker)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	_, _, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "gw-bot2", WorkerName: "gw-bot2",
	})
	if err == nil || !strings.Contains(err.Error(), "no AI Gateway configured") {
		t.Fatalf("expected no-AI-Gateway error, got: %v", err)
	}
}

func TestControllerDefaults(t *testing.T) {
	entries := ControllerDefaults("b1", "")
	if len(entries) != 1 || entries[0].Service != credprovider.ServiceObjectStorage {
		t.Fatalf("expected single object-storage entry, got %+v", entries)
	}

	entries = ControllerDefaults("b1", "gw-1")
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries with gateway, got %d", len(entries))
	}
	if entries[1].Service != credprovider.ServiceAIGateway || entries[1].Scope.GatewayID != "gw-1" {
		t.Fatalf("unexpected second entry: %+v", entries[1])
	}
}

// TestResolve_CustomPrefix verifies the STS session name carries the tenant
// prefix so cloud RAM auditing / policy matching can distinguish multiple
// HiClaw controllers running in the same cluster.
func TestResolve_CustomPrefix(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "alice"
	worker.Namespace = testNS
	c := newFakeClient(t, worker)

	r := New(c, testNS, "bucket", "", auth.ResourcePrefix("teamB-"))
	session, _, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "alice", WorkerName: "alice",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if session != "teamB-worker-alice" {
		t.Fatalf("session = %q, want teamB-worker-alice", session)
	}

	mgr := &v1beta1.Manager{}
	mgr.Name = "staging"
	mgr.Namespace = testNS
	c = newFakeClient(t, mgr)
	r = New(c, testNS, "bucket", "gw-1", auth.ResourcePrefix("teamB-"))
	session, _, err = r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleManager, Username: "staging",
	})
	if err != nil {
		t.Fatalf("resolve manager: %v", err)
	}
	if session != "teamB-manager-staging" {
		t.Fatalf("manager session = %q, want teamB-manager-staging", session)
	}
}

func TestResolveForCaller_RejectedRoles(t *testing.T) {
	r := New(newFakeClient(t), testNS, "b", "", auth.DefaultResourcePrefix)
	_, _, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{Role: auth.RoleAdmin})
	if err == nil {
		t.Fatalf("expected error for admin role")
	}
}

func newAlphaTeam() *v1beta1.Team {
	team := &v1beta1.Team{}
	team.Name = "alpha"
	team.Namespace = testNS
	team.Spec.Leader = v1beta1.LeaderSpec{Name: "lead"}
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{{Name: "w1"}}
	return team
}

func TestResolveTeamLeader_DefaultEntries(t *testing.T) {
	team := newAlphaTeam()
	c := newFakeClient(t, team)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	session, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role:     auth.RoleTeamLeader,
		Username: "lead",
		Team:     "alpha",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if session != "hiclaw-worker-lead" {
		t.Fatalf("session = %q, want hiclaw-worker-lead", session)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 default entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Scope.Bucket != "hiclaw-test" {
		t.Fatalf("bucket = %q", e.Scope.Bucket)
	}
	for _, want := range []string{"agents/lead/*", "shared/*", "teams/alpha/*"} {
		if !hasPrefix(e.Scope.Prefixes, want) {
			t.Fatalf("missing prefix %q in %+v", want, e.Scope.Prefixes)
		}
	}
	if !hasAllPerms(e.Permissions, "read", "write", "list", "delete") {
		t.Fatalf("expected RW permissions, got %+v", e.Permissions)
	}
}

func TestResolveTeamWorker_DefaultEntries(t *testing.T) {
	team := newAlphaTeam()
	c := newFakeClient(t, team)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	session, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role:       auth.RoleWorker,
		Username:   "w1",
		WorkerName: "w1",
		Team:       "alpha",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if session != "hiclaw-worker-w1" {
		t.Fatalf("session = %q", session)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 default entry, got %d", len(entries))
	}
	e := entries[0]
	for _, want := range []string{"agents/w1/*", "shared/*", "teams/alpha/*"} {
		if !hasPrefix(e.Scope.Prefixes, want) {
			t.Fatalf("missing prefix %q in %+v", want, e.Scope.Prefixes)
		}
	}
	if hasPrefix(e.Scope.Prefixes, "agents/lead/*") {
		t.Fatalf("team worker must not see leader's prefix: %+v", e.Scope.Prefixes)
	}
	if !hasAllPerms(e.Permissions, "read", "write", "list", "delete") {
		t.Fatalf("expected RW permissions, got %+v", e.Permissions)
	}
}

func TestResolveTeamMember_CustomEntries(t *testing.T) {
	team := newAlphaTeam()
	team.Spec.Workers[0].AccessEntries = []v1beta1.AccessEntry{
		{
			Service:     credprovider.ServiceObjectStorage,
			Permissions: []string{"read"},
			Scope: rawJSON(t, map[string]any{
				"bucketRef": "workspace",
				"prefixes":  []string{"custom/${self.team}/*", "agents/${self.name}/data/*"},
			}),
		},
	}
	c := newFakeClient(t, team)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	_, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role:       auth.RoleWorker,
		Username:   "w1",
		WorkerName: "w1",
		Team:       "alpha",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (defaults must not leak when custom entries are set)", len(entries))
	}
	got := entries[0].Scope.Prefixes
	if !hasPrefix(got, "custom/alpha/*") {
		t.Fatalf("${self.team} not expanded: %+v", got)
	}
	if !hasPrefix(got, "agents/w1/data/*") {
		t.Fatalf("${self.name} not expanded: %+v", got)
	}
	if len(entries[0].Permissions) != 1 || entries[0].Permissions[0] != "read" {
		t.Fatalf("permissions must come from CR, got %+v", entries[0].Permissions)
	}
}

func TestResolveTeamMember_TeamCRMissing(t *testing.T) {
	c := newFakeClient(t)

	r := New(c, testNS, "hiclaw-test", "", auth.DefaultResourcePrefix)
	_, entries, err := r.ResolveForCaller(context.Background(), &auth.CallerIdentity{
		Role:       auth.RoleWorker,
		Username:   "ghost-worker",
		WorkerName: "ghost-worker",
		Team:       "ghost",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 default entry, got %d", len(entries))
	}
	got := entries[0].Scope.Prefixes
	for _, want := range []string{"agents/ghost-worker/*", "shared/*", "teams/ghost/*"} {
		if !hasPrefix(got, want) {
			t.Fatalf("missing prefix %q in %+v", want, got)
		}
	}
}
