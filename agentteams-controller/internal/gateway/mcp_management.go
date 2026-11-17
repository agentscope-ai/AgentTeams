package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
)

// MCPManagementClient keeps Console credentials inside the gateway adapter.
// Only Controller's MCP service constructs these bounded Console operations.
type MCPManagementClient interface {
	ManageMCP(context.Context, string, string, any) (any, error)
}

var mcpManagementPath = regexp.MustCompile(`^/v1/(mcpServer(/([a-z][a-z0-9-]{0,39}|consumers|swaggerToMcpConfig))?|service-sources(/[a-z][a-z0-9-]*)?|consumers(/[a-z][a-z0-9-]*)?|wasm-plugins(/(transformer|agentteams-mcp-headers))?|routes/mcp-server-[a-z][a-z0-9-]{0,39}\.internal/plugin-instances/agentteams-mcp-headers)$`)

func (c *HigressClient) ManageMCP(ctx context.Context, method, path string, body any) (any, error) {
	if !mcpManagementPath.MatchString(path) || (method != "GET" && method != "POST" && method != "PUT" && method != "DELETE") {
		return nil, fmt.Errorf("unsupported MCP gateway operation")
	}
	raw, status, err := c.doJSON(ctx, method, path, body)
	if err != nil {
		return nil, fmt.Errorf("MCP gateway connection failed")
	}
	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &envelope)
	missing := status == http.StatusNotFound
	// Embedded Console wraps this exact missing-route exception as a 502.
	if method == "GET" && status == 502 && len(path) > len("/v1/mcpServer/") && path[:len("/v1/mcpServer/")] == "/v1/mcpServer/" {
		name := path[len("/v1/mcpServer/"):]
		missing = envelope.Message == "com.alibaba.higress.sdk.exception.NotFoundException: can't found the bound route by name: mcp-server-"+name+".internal"
	}
	if missing && (method == "GET" || method == "DELETE") {
		return nil, nil
	}
	if status < 200 || status >= 300 || (envelope.Success != nil && !*envelope.Success) {
		return nil, fmt.Errorf("MCP gateway rejected %s %s (HTTP %d)", method, path, status)
	}
	var result any
	if len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, &result); err != nil {
			return nil, fmt.Errorf("invalid MCP gateway response")
		}
	} else if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("invalid MCP gateway response")
		}
	}
	return result, nil
}
