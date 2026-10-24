package agentconfig

import (
	"encoding/json"
	"net/url"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// IsTrustedMCPHost reports whether urlStr is addressed to the configured AI
// gateway (exact host:port match against Config.AIGatewayURL). The MCP gateway
// consumer key is attached only to entries on the trusted gateway; entries on
// any other host are external and never receive the credential (#1220 §7).
// An unset or unparseable gateway URL trusts nothing (fail closed).
func (g *Generator) IsTrustedMCPHost(urlStr string) bool {
	gw, err := url.Parse(g.config.AIGatewayURL)
	if err != nil || gw.Host == "" {
		return false
	}
	entry, err := url.Parse(urlStr)
	if err != nil || entry.Host == "" {
		return false
	}
	return entry.Host == gw.Host
}

// GenerateMcporterConfig produces mcporter-servers.json content for a worker or
// manager's MCP servers. Each entry's URL is used verbatim (the CRD carries the
// full gateway endpoint). An Authorization: Bearer <gatewayKey> header is
// injected so the agent authenticates with the same consumer key it uses for
// LLM access — but only when the entry is addressed to the trusted AI gateway
// (IsTrustedMCPHost); entries on any other host must not receive the gateway
// credential.
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
		entry := map[string]interface{}{
			"url":       url,
			"transport": transport,
		}
		if g.IsTrustedMCPHost(url) {
			entry["headers"] = map[string]string{
				"Authorization": "Bearer " + gatewayKey,
			}
		}
		servers[name] = entry
	}

	if len(servers) == 0 {
		return nil, nil
	}

	config := map[string]interface{}{
		"mcpServers": servers,
	}
	return json.MarshalIndent(config, "", "  ")
}
