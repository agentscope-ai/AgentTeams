package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestManagedMCPListDiscoversGatewayServicesWithoutRegistry(t *testing.T) {
	g := &assetGateway{responses: map[string]any{
		"GET /v1/mcpServer":         []any{map[string]any{"name": "outside", "type": "DIRECT_ROUTE", "rawConfigurations": "gateway-secret"}},
		"GET /v1/mcpServer/outside": map[string]any{"directRouteConfig": map[string]any{"transportType": "sse"}, "dbConfig": map[string]any{"password": "gateway-secret"}},
	}}
	w := assetRequest(t, &gatewayAssets{gateway: g, gatewayURL: "http://gateway.test:8080"}, "mcps", `{"action":"list"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var result struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Servers) != 1 {
		t.Fatalf("%s: %v", w.Body.String(), err)
	}
	server := result.Servers[0]
	if server["name"] != "outside" || server["gatewayOnly"] != true || server["managed"] != false || server["kind"] != "proxy" || server["transport"] != "sse" || server["url"] != "http://gateway.test:8080/mcp-servers/outside/sse" {
		t.Fatalf("unexpected gateway summary: %v", server)
	}
	if strings.Contains(w.Body.String(), "gateway-secret") || server["connection"] != nil || server["authorizedWorkerCount"] != nil {
		t.Fatal("gateway-only entries must not invent editable connections or authorization counts")
	}
}

func TestManagedMCPListEnrichesGatewayAndPreservesRetryRecords(t *testing.T) {
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "docs", `{"name":"docs","managed":true,"url":"http://gateway.test/mcp-servers/docs/mcp","transport":"streaminghttp","gatewayState":"ready","connection":{"kind":"proxy","auth":{"type":"bearer","secret":"backend-secret"}},"authorizedWorkers":["alice","pending"],"pendingRemovalWorkers":["pending"],"probe":{"name":"private-probe","key":"probe-secret"},"verification":{"success":true,"tools":[{"name":"echo"}]}}`)
	mcpPutRegistry(t, store, "failed", `{"name":"failed","managed":true,"gatewayState":"error","connection":{"kind":"rest"},"authorizedWorkers":[]}`)
	mcpPutRegistry(t, store, "removed", `{"name":"removed","managed":true,"gatewayState":"ready","connection":{"kind":"proxy"},"authorizedWorkers":[],"verification":{"success":true,"tools":[{"name":"old-tool"}]}}`)
	mcpPutRegistry(t, store, "legacy", `{"name":"legacy","url":"http://legacy.test/mcp","transport":"streaminghttp"}`)
	g := &assetGateway{responses: map[string]any{"GET /v1/mcpServer": []any{map[string]any{"name": "docs", "type": "DIRECT_ROUTE"}}}}
	h := &gatewayAssets{gateway: g, gatewayURL: "http://gateway.test", resources: NewResourceHandler(nil, "default", nil, "", nil).WithOSS(store)}
	w := assetRequest(t, h, "mcps", `{"action":"list"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var result struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Servers) != 4 {
		t.Fatalf("%s: %v", w.Body.String(), err)
	}
	docs, failed, legacy, removed := result.Servers[0], result.Servers[1], result.Servers[2], result.Servers[3]
	if docs["name"] != "docs" || docs["toolCount"] != float64(1) || docs["authorizedWorkerCount"] != float64(1) || docs["gatewayOnly"] == true {
		t.Fatalf("managed metadata lost: %v", docs)
	}
	if failed["name"] != "failed" || failed["gatewayState"] != "error" || failed["connection"] == nil {
		t.Fatalf("failed save is no longer editable: %v", failed)
	}
	if legacy["name"] != "legacy" || legacy["managed"] != false || legacy["url"] != "http://legacy.test/mcp" {
		t.Fatalf("legacy registration lost: %v", legacy)
	}
	if removed["name"] != "removed" || removed["gatewayState"] != "error" || removed["verification"] != nil || removed["toolCount"] != float64(0) {
		t.Fatalf("removed gateway service still appears verified: %v", removed)
	}
	if strings.Contains(w.Body.String(), "backend-secret") || strings.Contains(w.Body.String(), "probe-secret") || strings.Contains(w.Body.String(), "private-probe") {
		t.Fatal("private managed credentials exposed")
	}
	if strings.Join(g.calls, ",") != "GET /v1/mcpServer" {
		t.Fatalf("managed entries should reuse stored metadata: %v", g.calls)
	}
}

func TestManagedMCPListPropagatesGatewayFailure(t *testing.T) {
	g := &assetGateway{fail: "GET /v1/mcpServer"}
	h := &gatewayAssets{gateway: g, resources: NewResourceHandler(nil, "default", nil, "", nil).WithOSS(ossfake.NewMemory())}
	w := assetRequest(t, h, "mcps", `{"action":"list"}`)
	if w.Code != 502 || strings.Contains(w.Body.String(), `"servers"`) {
		t.Fatalf("gateway failure hidden by registry: %d %s", w.Code, w.Body.String())
	}
}

func TestManagedMCPListRejectsMalformedGatewayDirectory(t *testing.T) {
	g := &assetGateway{responses: map[string]any{"GET /v1/mcpServer": map[string]any{"unexpected": true}}}
	h := &gatewayAssets{gateway: g, resources: NewResourceHandler(nil, "default", nil, "", nil).WithOSS(ossfake.NewMemory())}
	w := assetRequest(t, h, "mcps", `{"action":"list"}`)
	if w.Code != 502 {
		t.Fatalf("malformed gateway response reported as an empty catalog: %d %s", w.Code, w.Body.String())
	}
}
