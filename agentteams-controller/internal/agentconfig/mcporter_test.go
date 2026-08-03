package agentconfig

import (
	"encoding/json"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

func TestGenerateMcporterConfig_EmptyReturnsNil(t *testing.T) {
	g := NewGenerator(Config{})
	data, err := g.GenerateMcporterConfig("bearer-key", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data != nil {
		t.Fatalf("expected nil data for empty input, got %q", string(data))
	}

	data, err = g.GenerateMcporterConfig("bearer-key", []v1beta1.MCPServer{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data != nil {
		t.Fatalf("expected nil data for empty slice, got %q", string(data))
	}
}

func TestGenerateMcporterConfig_SingleServerDefaultsTransportAndInjectsBearer(t *testing.T) {
	g := NewGenerator(Config{})
	data, err := g.GenerateMcporterConfig("KEY-123", []v1beta1.MCPServer{
		{Name: "github", URL: "https://gw.example.com/mcp-servers/github/mcp"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded map[string]map[string]map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	srv, ok := decoded["mcpServers"]["github"]
	if !ok {
		t.Fatalf("missing github entry: %s", string(data))
	}
	if srv["url"] != "https://gw.example.com/mcp-servers/github/mcp" {
		t.Errorf("url = %v", srv["url"])
	}
	if srv["transport"] != "http" {
		t.Errorf("transport = %v, expected http (default)", srv["transport"])
	}
	headers := srv["headers"].(map[string]interface{})
	if headers["Authorization"] != "Bearer KEY-123" {
		t.Errorf("Authorization = %v", headers["Authorization"])
	}
}

func TestGenerateMcporterConfig_PreservesExplicitTransport(t *testing.T) {
	g := NewGenerator(Config{})
	data, err := g.GenerateMcporterConfig("key", []v1beta1.MCPServer{
		{Name: "stream-mcp", URL: "https://gw/mcp-servers/stream/sse", Transport: "sse"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded map[string]map[string]map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if decoded["mcpServers"]["stream-mcp"]["transport"] != "sse" {
		t.Errorf("transport = %v", decoded["mcpServers"]["stream-mcp"]["transport"])
	}
}

func TestGenerateMcporterConfig_SkipsInvalidEntries(t *testing.T) {
	g := NewGenerator(Config{})
	data, err := g.GenerateMcporterConfig("key", []v1beta1.MCPServer{
		{Name: "", URL: "https://x/mcp"},       // empty name
		{Name: "noUrl", URL: ""},               // empty url
		{Name: "  ", URL: "https://x/mcp"},     // whitespace name
		{Name: "github", URL: "   "},           // whitespace url
		{Name: "ok", URL: "https://gw/ok/mcp"}, // valid
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded map[string]map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	servers := decoded["mcpServers"]
	if len(servers) != 1 {
		t.Fatalf("expected exactly 1 valid server, got %d: %v", len(servers), servers)
	}
	if _, ok := servers["ok"]; !ok {
		t.Errorf("expected 'ok' key, got %v", servers)
	}
}

func TestGenerateMcporterConfig_AllInvalidReturnsNil(t *testing.T) {
	g := NewGenerator(Config{})
	data, err := g.GenerateMcporterConfig("key", []v1beta1.MCPServer{
		{Name: "", URL: "x"},
		{Name: "y", URL: ""},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data != nil {
		t.Fatalf("expected nil when all entries invalid, got %q", string(data))
	}
}

func TestGenerateMcporterConfig_MultipleServers(t *testing.T) {
	g := NewGenerator(Config{})
	data, err := g.GenerateMcporterConfig("K", []v1beta1.MCPServer{
		{Name: "github", URL: "https://gw/mcp-servers/github/mcp"},
		{Name: "jira", URL: "https://gw/mcp-servers/jira/mcp", Transport: "sse"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded map[string]map[string]map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(decoded["mcpServers"]) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(decoded["mcpServers"]))
	}
	if decoded["mcpServers"]["github"]["transport"] != "http" {
		t.Errorf("github transport = %v", decoded["mcpServers"]["github"]["transport"])
	}
	if decoded["mcpServers"]["jira"]["transport"] != "sse" {
		t.Errorf("jira transport = %v", decoded["mcpServers"]["jira"]["transport"])
	}
}
