package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// These exercise the actual config deployer and reconciliation order with
// in-memory storage; they do not execute a Harness or recover a Task.
func workerRecoveryDeployer(t *testing.T, store *ossfake.Memory) *service.Deployer {
	t.Helper()
	return service.NewDeployer(service.DeployerConfig{
		OSS: store, AgentFSDir: t.TempDir(),
		AgentConfig: agentconfig.NewGenerator(agentconfig.Config{
			MatrixDomain: "matrix.local", MatrixServerURL: "https://matrix.local", DefaultModel: "qwen",
		}),
		RuntimeProjection: service.RuntimeProjectionConfig{AIGatewayURL: "https://gateway.local"},
	})
}

func readWorkerRecoveryRecord(t *testing.T, store *ossfake.Memory, name string) map[string]any {
	t.Helper()
	body, err := store.GetObject(context.Background(), "agents/"+name+"/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := yaml.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func assertWorkerRecoveryTransport(t *testing.T, store *ossfake.Memory, name string) map[string]any {
	t.Helper()
	record := readWorkerRecoveryRecord(t, store, name)
	transport := record["matrix"].(map[string]any)["transport"].(map[string]any)
	body, err := store.GetObject(context.Background(), "agents/"+name+"/openclaw.json")
	if err != nil {
		t.Fatal(err)
	}
	var compatibility map[string]any
	if err := json.Unmarshal(body, &compatibility); err != nil {
		t.Fatal(err)
	}
	matrix := compatibility["channels"].(map[string]any)["matrix"].(map[string]any)
	for _, key := range []string{"homeserver", "groupAllowFrom", "dm"} {
		if !reflect.DeepEqual(transport[key], matrix[key]) {
			t.Errorf("%s standard %s=%v, resolved policy=%v", name, key, transport[key], matrix[key])
		}
	}
	return record
}

func TestAllHarnessesPublishCurrentWorkerConfiguration(t *testing.T) {
	for _, runtime := range []string{"codex", "hermes", "qwenpaw", "deepseek-harness", "openclaw", "openhuman"} {
		t.Run(runtime, func(t *testing.T) { testHarnessWorkerConfiguration(t, runtime) })
	}
}

func testHarnessWorkerConfiguration(t *testing.T, runtime string) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := workerRecoveryDeployer(t, store)
	state := &MemberState{MatrixUserID: "@worker:matrix.local", RoomID: "!worker:matrix.local",
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "synthetic-token", GatewayKey: "synthetic-key"}}
	member := MemberContext{Name: "worker-cr", RuntimeName: "worker", Role: RoleStandalone,
		Spec: v1beta1.WorkerSpec{Runtime: runtime, Model: "qwen"}}
	for _, human := range []string{"@first:matrix.local", "@replacement:matrix.local"} {
		member.HumanMatrixIDs = []string{human}
		if err := ReconcileMemberConfig(ctx, MemberDeps{Deployer: deployer}, member, state); err != nil {
			t.Fatal(err)
		}
		record := readWorkerRecoveryRecord(t, store, "worker")
		if runtime == "hermes" || runtime == "openclaw" || runtime == "openhuman" {
			assertWorkerRecoveryTransport(t, store, "worker")
		}
		identity := record["member"].(map[string]any)
		if identity["runtime"] != runtime || identity["matrixUserId"] != state.MatrixUserID {
			t.Fatalf("wrong Worker identity: %v", identity)
		}
		member.IsUpdate = true
	}
}

func TestLegacyHarnessesTeamPublishesResolvedPolicyInSameReconcile(t *testing.T) {
	for _, runtime := range []string{"hermes", "openclaw", "openhuman"} {
		t.Run(runtime, func(t *testing.T) { testLegacyHarnessTeamPolicy(t, runtime) })
	}
}

func testLegacyHarnessTeamPolicy(t *testing.T, runtime string) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	leader := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:   v1beta1.WorkerSpec{Runtime: runtime, Model: "qwen"},
		Status: v1beta1.WorkerStatus{MatrixUserID: "@lead:matrix.local", RoomID: "!lead:matrix.local", Phase: "Running"}}
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:   v1beta1.WorkerSpec{Runtime: runtime, Model: "qwen"},
		Status: v1beta1.WorkerStatus{MatrixUserID: "@dev:matrix.local", RoomID: "!dev:matrix.local", Phase: "Running"}}
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "default"},
		Spec: v1beta1.TeamSpec{WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "lead", Role: "team_leader"}, {Name: "dev"}}}}
	for _, name := range []string{"lead", "dev"} {
		if err := store.PutObject(ctx, "agents/"+name+"/openclaw.json", []byte(`{"channels":{"matrix":{"homeserver":"https://matrix.local","groupAllowFrom":["@old:matrix.local"],"dm":{"policy":"allowlist","allowFrom":["@old:matrix.local"]}}}}`)); err != nil {
			t.Fatal(err)
		}
	}
	rig := newTeamReconcileRig(t, team, leader, worker)
	rig.r.Deployer = workerRecoveryDeployer(t, store)
	for _, extra := range []string{"@first:matrix.local", "@replacement:matrix.local"} {
		var current v1beta1.Worker
		if err := rig.client.Get(ctx, client.ObjectKeyFromObject(worker), &current); err != nil {
			t.Fatal(err)
		}
		current.Spec.ChannelPolicy = &v1beta1.ChannelPolicySpec{GroupAllowExtra: []string{extra}}
		if err := rig.client.Update(ctx, &current); err != nil {
			t.Fatal(err)
		}
		if _, _, err := rig.reconcile("team"); err != nil {
			t.Fatal(err)
		}
		assertWorkerRecoveryTransport(t, store, "lead")
		assertWorkerRecoveryTransport(t, store, "dev")
	}
}

func TestLegacyHarnessesDetachPublishesStandalonePolicyInSameReconcile(t *testing.T) {
	for _, runtime := range []string{"hermes", "openclaw", "openhuman"} {
		t.Run(runtime, func(t *testing.T) { testLegacyHarnessDetachPolicy(t, runtime) })
	}
}

func testLegacyHarnessDetachPolicy(t *testing.T, runtime string) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:   v1beta1.WorkerSpec{Runtime: runtime, Model: "qwen"},
		Status: v1beta1.WorkerStatus{MatrixUserID: "@dev:matrix.local", RoomID: "!dev:matrix.local"}}
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "old-team", Namespace: "default"}}
	if err := store.PutObject(ctx, "agents/dev/openclaw.json", []byte(`{"channels":{"matrix":{"homeserver":"https://matrix.local","groupAllowFrom":["@old-lead:matrix.local"],"dm":{"policy":"allowlist","allowFrom":["@old-lead:matrix.local"]}}}}`)); err != nil {
		t.Fatal(err)
	}
	rig := newTeamReconcileRig(t, worker)
	rig.r.Deployer = workerRecoveryDeployer(t, store)
	rig.r.ManagerConfig = service.NewManagerConfigStore(service.ManagerConfigStoreConfig{
		OSS: store, MatrixDomain: "matrix.local", ManagerName: "manager"})
	if err := rig.r.detachTeamMember(ctx, team, worker); err != nil {
		t.Fatal(err)
	}
	record := assertWorkerRecoveryTransport(t, store, "dev")
	if record["team"] != nil || record["member"].(map[string]any)["role"] != "standalone" {
		t.Fatalf("detached Worker retains Team context: %v", record)
	}
}
