package server

import (
	"context"
	"net/url"
	"sort"
	"strings"
)

func (h *gatewayAssets) listMCPAssets(ctx context.Context) (any, error) {
	value, err := h.mcpGateway(ctx, "GET", "/v1/mcpServer", nil)
	if err != nil {
		return nil, err
	}
	entries, ok := value.([]any)
	if value != nil && !ok {
		return nil, assetError(502, "invalid MCP gateway directory")
	}
	records := map[string]*managedMCP{}
	if h.resources != nil && h.resources.oss != nil {
		keys, err := h.resources.oss.ListObjects(ctx, mcpRegistryPrefix)
		if err != nil {
			return nil, assetError(503, "cannot list MCP assets")
		}
		for _, key := range keys {
			name := strings.TrimSuffix(strings.TrimPrefix(key, mcpRegistryPrefix), ".json")
			if !strings.HasSuffix(key, ".json") || !assetName.MatchString(name) {
				continue
			}
			record, err := h.readMCP(ctx, name)
			if err != nil {
				return nil, err
			}
			if record != nil {
				records[name] = record
			}
		}
	}
	servers := map[string]map[string]any{}
	for _, entry := range entries {
		gateway := object(entry)
		name := textField(gateway["name"])
		if !assetName.MatchString(name) {
			continue
		}
		if record := records[name]; record != nil && record.Managed {
			servers[name] = publicMCP(record)
			continue
		}
		// Higress summaries can omit direct-route details. Only fetch those
		// for services without stored connection metadata, to identify SSE.
		kind, transport := "proxy", "streaminghttp"
		switch textField(gateway["type"]) {
		case "OPEN_API":
			kind = "rest"
		case "DATABASE":
			kind = "database"
		case "DIRECT_ROUTE":
			route := object(gateway["directRouteConfig"])
			if route == nil {
				detail, err := h.mcpGateway(ctx, "GET", "/v1/mcpServer/"+name, nil)
				if err != nil {
					return nil, err
				}
				route = object(object(detail)["directRouteConfig"])
			}
			if textField(route["transportType"]) == "sse" {
				transport = "sse"
			}
		}
		base, err := url.Parse(h.gatewayURL)
		if err != nil || base.Host == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") {
			return nil, assetError(503, "gateway data plane is not configured")
		}
		base.Path, base.RawQuery, base.Fragment = "/mcp-servers/"+name+"/mcp", "", ""
		if transport == "sse" {
			base.Path = "/mcp-servers/" + name + "/sse"
		}
		// Do not return raw gateway configuration or pretend the Controller
		// can edit backend credentials that it does not own.
		servers[name] = map[string]any{"name": name, "description": textField(gateway["description"]), "url": base.String(), "transport": transport,
			"kind": kind, "managed": false, "gatewayOnly": true, "gatewayState": "ready"}
	}
	for name, record := range records {
		if servers[name] != nil {
			continue
		}
		if record.Managed {
			// Retain failed saves and externally removed services for repair,
			// while making their absence from the real gateway explicit.
			server := publicMCP(record)
			server["gatewayState"], server["toolCount"] = "error", 0
			delete(server, "verification")
			servers[name] = server
		} else {
			servers[name] = map[string]any{"name": record.Name, "url": record.URL, "transport": record.Transport, "managed": false}
		}
	}
	list := make([]map[string]any, 0, len(servers))
	for _, server := range servers {
		list = append(list, server)
	}
	sort.Slice(list, func(i, j int) bool { return textField(list[i]["name"]) < textField(list[j]["name"]) })
	return map[string]any{"servers": list}, nil
}
