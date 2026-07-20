package migration

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
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
