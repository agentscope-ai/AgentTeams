package agentconfig

import (
	"encoding/json"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// GenerateMcporterConfig produces mcporter-servers.json content for a worker or
// manager's MCP servers. Each entry's URL is used verbatim (the CRD carries the
// full gateway endpoint), and an Authorization: Bearer <gatewayKey> header is
// injected so the agent authenticates with the same consumer key it uses for
// LLM access.
//
// The transport defaults to "http" (Streamable HTTP) when unset. Entries with
// an empty name or url are skipped silently. Returns (nil, nil) when the input
// is empty so the caller can skip writing the file entirely.
func (g *Generator) GenerateMcporterConfig(gatewayKey string, mcpServers []v1beta1.MCPServer) ([]byte, error) {
	if len(mcpServers) == 0 {
		return nil, nil
	}

	servers := make(map[string]interface{}, len(mcpServers))
	for _, s := range mcpServers {
		name := strings.TrimSpace(s.Name)
		url := strings.TrimSpace(s.URL)
		if name == "" || url == "" {
			continue
		}
		transport := strings.TrimSpace(s.Transport)
		if transport == "" {
			transport = "http"
		}
		servers[name] = map[string]interface{}{
			"url":       url,
			"transport": transport,
			"headers": map[string]string{
				"Authorization": "Bearer " + gatewayKey,
			},
		}
	}

	if len(servers) == 0 {
		return nil, nil
	}

	config := map[string]interface{}{
		"mcpServers": servers,
	}
	return json.MarshalIndent(config, "", "  ")
}
