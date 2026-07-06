package migration

import (
	"context"
	"fmt"
	"testing"

	"github.com/hiclaw/hiclaw-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestExtractMCPServers_WrappedShape(t *testing.T) {
	fake := ossfake.NewMemory()
	workerName := "w1"
	payload := `{
  "mcpServers": {
    "github": {"url": "https://gw/mcp-servers/github/mcp", "transport": "http"},
    "jira":   {"url": "https://gw/mcp-servers/jira/mcp",  "transport": "sse"}
  }
}`
	key := fmt.Sprintf("agents/%s/mcporter-servers.json", workerName)
	if err := fake.PutObject(context.Background(), key, []byte(payload)); err != nil {
		t.Fatalf("put: %v", err)
	}

	m := &Migrator{OSS: fake}
	got := m.extractMCPServers(context.Background(), workerName)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2; got=%v", len(got), got)
	}

	by := map[string]map[string]interface{}{}
	for _, e := range got {
		by[e["name"].(string)] = e
	}
	if e := by["github"]; e["url"] != "https://gw/mcp-servers/github/mcp" || e["transport"] != "http" {
		t.Errorf("github entry=%v", e)
	}
	if e := by["jira"]; e["url"] != "https://gw/mcp-servers/jira/mcp" || e["transport"] != "sse" {
		t.Errorf("jira entry=%v", e)
	}
}

func TestExtractMCPServers_LegacyFlatShape(t *testing.T) {
	fake := ossfake.NewMemory()
	workerName := "w2"
	payload := `{
  "github": {"url": "https://gw/mcp-servers/github/mcp"}
}`
	key := fmt.Sprintf("agents/%s/mcporter-servers.json", workerName)
	if err := fake.PutObject(context.Background(), key, []byte(payload)); err != nil {
		t.Fatalf("put: %v", err)
	}

	m := &Migrator{OSS: fake}
	got := m.extractMCPServers(context.Background(), workerName)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1; got=%v", len(got), got)
	}
	e := got[0]
	if e["name"] != "github" || e["url"] != "https://gw/mcp-servers/github/mcp" || e["transport"] != "http" {
		t.Errorf("entry=%v", e)
	}
}

func TestExtractMCPServers_NotFound(t *testing.T) {
	fake := ossfake.NewMemory()
	m := &Migrator{OSS: fake}
	got := m.extractMCPServers(context.Background(), "missing")
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestCreateTeamCRUsesWorkerMembers(t *testing.T) {
	ctx := context.Background()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	m := &Migrator{
		OSS:       ossfake.NewMemory(),
		Namespace: "default",
	}

	err := m.createTeamCR(ctx, dyn, "team-a", teamRegEntry{
		Leader:  "lead",
		Workers: []string{"dev"},
	}, map[string]workerRegEntry{
		"lead": {},
		"dev":  {},
	})
	if err != nil {
		t.Fatalf("createTeamCR: %v", err)
	}

	obj, err := dyn.Resource(teamGVR).Namespace("default").Get(ctx, "team-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Team: %v", err)
	}
	spec, _, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if _, ok := spec["leader"]; ok {
		t.Fatalf("legacy spec.leader must not be written: %#v", spec)
	}
	if _, ok := spec["workers"]; ok {
		t.Fatalf("legacy spec.workers must not be written: %#v", spec)
	}
	members, ok, err := unstructured.NestedSlice(obj.Object, "spec", "workerMembers")
	if err != nil || !ok {
		t.Fatalf("workerMembers missing: ok=%v err=%v spec=%#v", ok, err, spec)
	}
	if len(members) != 2 {
		t.Fatalf("workerMembers len=%d, want 2: %#v", len(members), members)
	}
	leader := members[0].(map[string]interface{})
	if leader["name"] != "lead" || leader["role"] != "team_leader" {
		t.Fatalf("leader member=%#v, want lead team_leader", leader)
	}
	worker := members[1].(map[string]interface{})
	if worker["name"] != "dev" || worker["role"] != "worker" {
		t.Fatalf("worker member=%#v, want dev worker", worker)
	}
}
