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
	g := NewGenerator(Config{AIGatewayURL: "https://gw.example.com"})
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

func TestGenerateMcporterConfig_UntrustedHostNoCredential(t *testing.T) {
	g := NewGenerator(Config{AIGatewayURL: "https://gw.example.com"})
	data, err := g.GenerateMcporterConfig("KEY-123", []v1beta1.MCPServer{
		{Name: "github", URL: "https://evil.example.com/mcp-servers/github/mcp"},
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
	if _, ok := srv["headers"]; ok {
		t.Errorf("untrusted entry must not carry a headers block, got %v", srv["headers"])
	}
	if srv["url"] != "https://evil.example.com/mcp-servers/github/mcp" {
		t.Errorf("url = %v (entry must still be written, only the credential is withheld)", srv["url"])
	}
}

func TestGenerateMcporterConfig_GatewayURLUnsetNoCredential(t *testing.T) {
	// Fail closed: no configured gateway => no entry may receive the key.
	g := NewGenerator(Config{})
	data, err := g.GenerateMcporterConfig("KEY-123", []v1beta1.MCPServer{
		{Name: "github", URL: "https://gw.example.com/mcp"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded map[string]map[string]map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	srv := decoded["mcpServers"]["github"]
	if _, ok := srv["headers"]; ok {
		t.Errorf("no gateway configured: entry must not carry credentials, got %v", srv["headers"])
	}
}

func TestGenerateMcporterConfig_TrustedHostPortMismatch(t *testing.T) {
	// Host:port must match exactly; a different port is a different endpoint.
	g := NewGenerator(Config{AIGatewayURL: "https://gw.example.com:8443"})
	data, err := g.GenerateMcporterConfig("K", []v1beta1.MCPServer{
		{Name: "a", URL: "https://gw.example.com/mcp"},      // wrong port
		{Name: "b", URL: "https://gw.example.com:8443/mcp"}, // exact match
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded map[string]map[string]map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	servers := decoded["mcpServers"]
	if _, ok := servers["a"]["headers"]; ok {
		t.Errorf("port-mismatched entry must not carry credentials")
	}
	if _, ok := servers["b"]["headers"]; !ok {
		t.Errorf("exact host:port match must carry credentials")
	}
}

func TestIsTrustedMCPHost(t *testing.T) {
	g := NewGenerator(Config{AIGatewayURL: "https://gw.example.com:8443"})
	cases := []struct {
		url  string
		want bool
	}{
		{"https://gw.example.com:8443/mcp-servers/github/mcp", true},
		{"https://gw.example.com:8443", true},
		{"https://gw.example.com/mcp", false},          // different port
		{"https://evil.example.com:8443/mcp", false},   // different host
		{"http://gw.example.com:8443/mcp", true},       // scheme irrelevant, host:port decides
		{"https://sub.gw.example.com:8443/mcp", false}, // different host
		{"not a url", false},
		{"", false},
	}
	for _, c := range cases {
		if got := g.IsTrustedMCPHost(c.url); got != c.want {
			t.Errorf("IsTrustedMCPHost(%q) = %v, want %v", c.url, got, c.want)
		}
	}

	empty := NewGenerator(Config{})
	if empty.IsTrustedMCPHost("https://gw.example.com:8443/mcp") {
		t.Errorf("unset gateway URL must trust nothing")
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
