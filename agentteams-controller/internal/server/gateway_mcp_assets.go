package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type mcpParameter struct {
	Name       string `json:"name"`
	Value      string `json:"value"`
	Secret     bool   `json:"secret,omitempty"`
	Configured bool   `json:"configured,omitempty"`
}
type mcpAuth struct {
	Type       string `json:"type"`
	Username   string `json:"username,omitempty"`
	Name       string `json:"name,omitempty"`
	In         string `json:"in,omitempty"`
	Secret     string `json:"secret,omitempty"`
	Configured bool   `json:"configured,omitempty"`
}
type mcpDatabase struct {
	Type       string `json:"type"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password,omitempty"`
	DBName     string `json:"dbname"`
	Configured bool   `json:"configured,omitempty"`
}
type mcpConnection struct {
	Kind       string           `json:"kind"`
	BackendURL string           `json:"backendUrl"`
	Transport  string           `json:"transport"`
	Auth       mcpAuth          `json:"auth"`
	Headers    []mcpParameter   `json:"headers"`
	Query      []mcpParameter   `json:"query"`
	Timeout    int              `json:"timeout"`
	Tools      []map[string]any `json:"tools"`
	RawConfig  string           `json:"rawConfig,omitempty"`
	Database   *mcpDatabase     `json:"database,omitempty"`
}
type managedMCP struct {
	Name                  string            `json:"name"`
	Description           string            `json:"description,omitempty"`
	Connection            mcpConnection     `json:"connection"`
	Managed               bool              `json:"managed"`
	URL                   string            `json:"url"`
	Transport             string            `json:"transport"`
	CreatedAt             string            `json:"createdAt"`
	UpdatedAt             string            `json:"updatedAt"`
	GatewayState          string            `json:"gatewayState"`
	AuthorizedWorkers     []string          `json:"authorizedWorkers"`
	PendingRemovalWorkers []string          `json:"pendingRemovalWorkers,omitempty"`
	WorkerConsumers       map[string]string `json:"workerConsumers,omitempty"`
	Verification          map[string]any    `json:"verification,omitempty"`
	Probe                 struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	} `json:"probe"`
}

func contains(items []string, name string) bool {
	for _, v := range items {
		if v == name {
			return true
		}
	}
	return false
}
func without(items []string, name string) []string {
	out := []string{}
	for _, v := range items {
		if v != name {
			out = append(out, v)
		}
	}
	return out
}
func unique(items []string) []string {
	out := []string{}
	for _, v := range items {
		if !contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
func randomHex(size int) (string, error) {
	b := make([]byte, size)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}

func (h *gatewayAssets) readMCP(ctx context.Context, name string) (*managedMCP, error) {
	if !assetName.MatchString(name) {
		return nil, assetError(400, "invalid MCP name")
	}
	if h.resources == nil || h.resources.oss == nil {
		return nil, assetError(503, "MCP asset storage is unavailable")
	}
	data, err := h.resources.oss.GetObject(ctx, mcpRegistryPrefix+name+".json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, assetError(503, "cannot read MCP asset storage")
	}
	var record managedMCP
	if json.Unmarshal(data, &record) != nil {
		return nil, assetError(409, "invalid existing MCP asset")
	}
	return &record, nil
}
func (h *gatewayAssets) writeMCP(ctx context.Context, record *managedMCP) error {
	data, err := json.Marshal(record)
	if err == nil {
		err = h.resources.oss.PutObject(ctx, mcpRegistryPrefix+record.Name+".json", data)
	}
	if err != nil {
		return assetError(503, "cannot persist MCP asset; retry before reporting success")
	}
	return nil
}
func publicMCP(record *managedMCP) map[string]any {
	b, _ := json.Marshal(record)
	out := decodeObject(b)
	delete(out, "probe")
	delete(out, "workerConsumers")
	c := object(out["connection"])
	auth := object(c["auth"])
	auth["configured"] = record.Connection.Auth.Secret != ""
	delete(auth, "secret")
	if db := object(c["database"]); db != nil {
		db["configured"] = record.Connection.Database.Password != ""
		delete(db, "password")
	}
	for _, group := range []string{"headers", "query"} {
		rows, _ := c[group].([]any)
		for _, v := range rows {
			row := object(v)
			if row["secret"] == true {
				row["configured"] = textField(row["value"]) != ""
				row["value"] = ""
			}
		}
	}
	active := []string{}
	for _, name := range record.AuthorizedWorkers {
		if !contains(record.PendingRemovalWorkers, name) {
			active = append(active, name)
		}
	}
	out["authorizedWorkers"], out["authorizedWorkerCount"], out["kind"] = active, len(active), record.Connection.Kind
	tools, _ := record.Verification["tools"].([]any)
	out["toolCount"] = len(tools)
	return out
}

func mergeMCPSecrets(next *mcpConnection, old *mcpConnection) {
	if old == nil {
		return
	}
	if next.Auth.Type == old.Auth.Type && next.Auth.Secret == "" {
		next.Auth.Secret = old.Auth.Secret
	}
	if next.Database != nil && old.Database != nil && next.Database.Password == "" {
		next.Database.Password = old.Database.Password
	}
	merge := func(rows, previous []mcpParameter, fold bool) {
		for i := range rows {
			if !rows[i].Secret || rows[i].Value != "" {
				continue
			}
			for _, p := range previous {
				if p.Secret && (p.Name == rows[i].Name || (fold && strings.EqualFold(p.Name, rows[i].Name))) {
					rows[i].Value = p.Value
					break
				}
			}
		}
	}
	merge(next.Headers, old.Headers, true)
	merge(next.Query, old.Query, false)
}

var mcpParameterName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var protectedMCPHeader = regexp.MustCompile(`(?i)^(host|content-length|connection|transfer-encoding|authorization|mcp-.*|x-envoy-.*|x-mse-.*)$`)

func validateMCP(record *managedMCP) error {
	c := record.Connection
	if !assetName.MatchString(record.Name) || (c.Kind != "proxy" && c.Kind != "rest" && c.Kind != "database") {
		return assetError(400, "invalid MCP name or kind")
	}
	if c.Transport != "streaminghttp" && c.Transport != "sse" {
		return assetError(400, "transport must be streaminghttp or sse")
	}
	if c.Timeout < 1000 || c.Timeout > 30000 || len(c.RawConfig) > 512000 || len(c.Headers)+len(c.Query) > 32 {
		return assetError(400, "invalid MCP timeout or configuration size")
	}
	if c.Auth.Type != "none" && c.Auth.Type != "bearer" && c.Auth.Type != "basic" && c.Auth.Type != "api-key" {
		return assetError(400, "invalid MCP backend authentication")
	}
	if c.Auth.Type != "none" && c.Auth.Secret == "" {
		return assetError(400, "MCP backend credential is required")
	}
	if strings.ContainsAny(c.Auth.Secret, "\r\n") || (c.Auth.Type == "basic" && c.Auth.Username == "") {
		return assetError(400, "invalid MCP credential")
	}
	if c.Auth.Type == "api-key" && (!mcpParameterName.MatchString(c.Auth.Name) || (c.Auth.In != "header" && c.Auth.In != "query") || (c.Auth.In == "header" && protectedMCPHeader.MatchString(c.Auth.Name) && !strings.EqualFold(c.Auth.Name, "authorization"))) {
		return assetError(400, "invalid MCP API key parameter")
	}
	for group, rows := range [][]mcpParameter{c.Headers, c.Query} {
		seen := map[string]bool{}
		for _, p := range rows {
			key := p.Name
			if group == 0 {
				key = strings.ToLower(key)
			}
			if !mcpParameterName.MatchString(p.Name) || strings.ContainsAny(p.Name+p.Value, "\r\n") || seen[key] || (group == 0 && protectedMCPHeader.MatchString(p.Name)) {
				return assetError(400, "invalid, duplicate or reserved MCP parameter")
			}
			seen[key] = true
		}
	}
	if c.Kind == "database" {
		db := c.Database
		if db == nil || !contains([]string{"MYSQL", "POSTGRESQL", "CLICKHOUSE"}, db.Type) || db.Host == "" || db.Port < 1 || db.Port > 65535 || db.Username == "" || db.DBName == "" {
			return assetError(400, "invalid database connection")
		}
		return nil
	}
	u, err := url.Parse(c.BackendURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return assetError(400, "MCP backend must be an HTTP(S) URL without credentials, query or fragment")
	}
	if c.Kind == "proxy" && c.Transport == "sse" && !strings.HasSuffix(u.Path, "/sse") {
		return assetError(400, "SSE backend path must end in /sse")
	}
	if c.Kind == "rest" {
		_, err = compileMCPRest(record)
		return err
	}
	return nil
}

func mcpBackendParameters(c mcpConnection) (map[string]string, url.Values) {
	headers, query := map[string]string{}, url.Values{}
	for _, p := range c.Headers {
		headers[p.Name] = p.Value
	}
	for _, p := range c.Query {
		query.Set(p.Name, p.Value)
	}
	switch c.Auth.Type {
	case "bearer":
		headers["Authorization"] = "Bearer " + c.Auth.Secret
	case "basic":
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Auth.Username+":"+c.Auth.Secret))
	case "api-key":
		if c.Auth.In == "query" {
			query.Set(c.Auth.Name, c.Auth.Secret)
		} else {
			headers[c.Auth.Name] = c.Auth.Secret
		}
	}
	return headers, query
}

func compileMCPRest(record *managedMCP) (string, error) {
	c := record.Connection
	base, err := url.Parse(c.BackendURL)
	if err != nil || base.Host == "" {
		return "", assetError(400, "invalid REST backend URL")
	}
	headers, query := mcpBackendParameters(c)
	tools := []any{}
	if strings.TrimSpace(c.RawConfig) != "" {
		var node yaml.Node
		if yaml.Unmarshal([]byte(c.RawConfig), &node) != nil {
			return "", assetError(400, "invalid tool YAML")
		}
		var alias func(*yaml.Node) bool
		alias = func(n *yaml.Node) bool {
			if n.Kind == yaml.AliasNode {
				return true
			}
			for _, child := range n.Content {
				if alias(child) {
					return true
				}
			}
			return false
		}
		if alias(&node) {
			return "", assetError(400, "tool YAML aliases are not supported")
		}
		var config map[string]any
		if node.Decode(&config) != nil {
			return "", assetError(400, "invalid tool YAML")
		}
		tools, _ = config["tools"].([]any)
	} else {
		for _, tool := range c.Tools {
			name, method, path := textField(tool["name"]), textField(tool["method"]), textField(tool["path"])
			if !mcpParameterName.MatchString(name) || textField(tool["description"]) == "" || !contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE"}, method) || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, "#") {
				return "", assetError(400, "invalid REST tool")
			}
			parameters, _ := tool["parameters"].([]any)
			args := []any{}
			body := false
			names := map[string]bool{}
			for _, v := range parameters {
				p := object(v)
				n := textField(p["name"])
				position := textField(p["position"])
				if !mcpParameterName.MatchString(n) || names[n] || !contains([]string{"query", "path", "header", "body"}, position) || !contains([]string{"string", "number", "boolean", "object", "array"}, textField(p["type"])) || (position == "header" && protectedMCPHeader.MatchString(n)) {
					return "", assetError(400, "invalid REST tool parameter")
				}
				names[n] = true
				if position == "body" {
					body = true
				}
				description := textField(p["description"])
				if description == "" {
					description = n
				}
				args = append(args, map[string]any{"name": n, "description": description, "type": p["type"], "required": p["required"], "position": position})
			}
			template := map[string]any{"method": method, "url": base.Scheme + "://" + base.Host + strings.TrimRight(base.Path, "/") + path}
			if body {
				template["argsToJsonBody"] = true
			}
			tools = append(tools, map[string]any{"name": name, "description": tool["description"], "args": args, "requestTemplate": template})
		}
	}
	if len(tools) == 0 || len(tools) > 100 {
		return "", assetError(400, "configure 1 to 100 REST tools")
	}
	names := map[string]bool{}
	for _, v := range tools {
		tool := object(v)
		name := textField(tool["name"])
		template := object(tool["requestTemplate"])
		if !mcpParameterName.MatchString(name) || names[name] || template == nil {
			return "", assetError(400, "REST tools require unique names and requestTemplate")
		}
		names[name] = true
		target := textField(template["url"])
		if !strings.HasPrefix(target, base.Scheme+"://"+base.Host+"/") {
			return "", assetError(400, "REST tools must use the configured backend origin")
		}
		rows, _ := template["headers"].([]any)
		merged := []any{}
		for _, row := range rows {
			keep := true
			for key := range headers {
				if strings.EqualFold(key, textField(object(row)["key"])) {
					keep = false
				}
			}
			if keep {
				merged = append(merged, row)
			}
		}
		for key, value := range headers {
			merged = append(merged, map[string]any{"key": key, "value": value})
		}
		template["headers"] = merged
		if suffix := query.Encode(); suffix != "" {
			separator := "?"
			if strings.Contains(target, "?") {
				separator = "&"
			}
			template["url"] = target + separator + suffix
		}
	}
	raw, err := yaml.Marshal(map[string]any{"server": map[string]string{"name": record.Name, "type": "rest"}, "tools": tools})
	return string(raw), err
}

func (h *gatewayAssets) applyMCP(ctx context.Context, record *managedMCP) error {
	c := record.Connection
	var target *url.URL
	if c.Kind == "database" {
		target = &url.URL{Scheme: "http", Host: net.JoinHostPort(c.Database.Host, strconv.Itoa(c.Database.Port))}
	} else {
		target, _ = url.Parse(c.BackendURL)
	}
	port, _ := strconv.Atoi(target.Port())
	if port == 0 {
		port = 80
		if target.Scheme == "https" {
			port = 443
		}
	}
	sourceName := "at-mcp-" + record.Name
	source := map[string]any{"type": "dns", "name": sourceName, "domain": target.Hostname(), "port": port, "protocol": target.Scheme}
	existing, err := h.mcpGateway(ctx, "GET", "/v1/service-sources/"+sourceName, nil)
	if err != nil {
		return err
	}
	if existing == nil {
		if _, err = h.mcpGateway(ctx, "POST", "/v1/service-sources", source); err != nil {
			return err
		}
	} else {
		previous := object(existing)
		if previous == nil {
			return assetError(502, "invalid MCP service source")
		}
		if previous["domain"] != source["domain"] || fmt.Sprint(previous["port"]) != fmt.Sprint(port) || previous["protocol"] != source["protocol"] {
			for key, value := range source {
				previous[key] = value
			}
			if _, err = h.mcpGateway(ctx, "PUT", "/v1/service-sources/"+sourceName, previous); err != nil {
				return err
			}
		}
	}
	consumers := []string{record.Probe.Name}
	for _, name := range record.AuthorizedWorkers {
		if !contains(record.PendingRemovalWorkers, name) {
			consumer := record.WorkerConsumers[name]
			if consumer == "" {
				consumer = "worker-" + name
			}
			consumers = append(consumers, consumer)
		}
	}
	existing, err = h.mcpGateway(ctx, "GET", "/v1/consumers/"+record.Probe.Name, nil)
	if err != nil {
		return err
	}
	if existing == nil {
		_, err = h.mcpGateway(ctx, "POST", "/v1/consumers", map[string]any{"name": record.Probe.Name, "credentials": []any{map[string]any{"type": "key-auth", "source": "BEARER", "values": []string{record.Probe.Key}}}})
		if err != nil {
			return err
		}
	}
	// Use the gateway's default virtual host, as model routes do. The
	// internal data-plane hostname need not be a configured Console domain.
	payload := map[string]any{"name": record.Name, "description": record.Description, "domains": []string{}, "consumerAuthInfo": map[string]any{"type": "key-auth", "enable": true, "allowedConsumers": consumers}, "services": []any{map[string]any{"name": sourceName + ".dns", "port": port, "weight": 100}}}
	switch c.Kind {
	case "database":
		db := c.Database
		payload["type"], payload["dbType"] = "DATABASE", db.Type
		payload["dbConfig"] = map[string]any{"host": db.Host, "port": strconv.Itoa(db.Port), "username": db.Username, "password": db.Password, "dbname": db.DBName}
	case "rest":
		raw, err := compileMCPRest(record)
		if err != nil {
			return err
		}
		payload["type"], payload["rawConfigurations"] = "OPEN_API", raw
	case "proxy":
		transport := "streamable"
		if c.Transport == "sse" {
			transport = "sse"
		}
		payload["type"], payload["directRouteConfig"] = "DIRECT_ROUTE", map[string]any{"path": target.Path, "transportType": transport}
	}
	if _, err = h.mcpGateway(ctx, "PUT", "/v1/mcpServer", payload); err != nil {
		return err
	}
	if c.Kind != "proxy" {
		return nil
	}
	const plugin = "agentteams-mcp-headers"
	existing, err = h.mcpGateway(ctx, "GET", "/v1/wasm-plugins/"+plugin, nil)
	if err != nil {
		return err
	}
	if existing == nil {
		template, err := h.mcpGateway(ctx, "GET", "/v1/wasm-plugins/transformer", nil)
		if err != nil {
			return err
		}
		p := object(template)
		if p == nil || textField(p["imageRepository"]) == "" {
			return assetError(503, "gateway request transformer is unavailable")
		}
		delete(p, "version")
		p["name"], p["builtIn"], p["phase"], p["priority"] = plugin, false, "AUTHZ", 10
		p["title"], p["description"] = "AgentTeams MCP upstream parameters", "Scoped MCP backend headers after Consumer authentication"
		if _, err = h.mcpGateway(ctx, "POST", "/v1/wasm-plugins", p); err != nil {
			return err
		}
	} else {
		p := object(existing)
		if p["phase"] != "AUTHZ" || fmt.Sprint(p["priority"]) != "10" {
			return assetError(409, "MCP transformer name is already in use")
		}
	}
	headers, query := mcpBackendParameters(c)
	removeHeaders := []any{map[string]string{"key": "Authorization"}}
	addHeaders, removeQuery, addQuery := []any{}, []any{}, []any{}
	for key, value := range headers {
		if !strings.EqualFold(key, "Authorization") {
			removeHeaders = append(removeHeaders, map[string]string{"key": key})
		}
		addHeaders = append(addHeaders, map[string]string{"key": key, "value": value})
	}
	for key, values := range query {
		removeQuery = append(removeQuery, map[string]string{"key": key})
		addQuery = append(addQuery, map[string]string{"key": key, "value": values[0]})
	}
	_, err = h.mcpGateway(ctx, "PUT", "/v1/routes/mcp-server-"+record.Name+".internal/plugin-instances/"+plugin, map[string]any{"enabled": true, "configurations": map[string]any{"reqRules": []any{map[string]any{"operate": "remove", "headers": removeHeaders, "querys": removeQuery}, map[string]any{"operate": "add", "headers": addHeaders, "querys": addQuery}}}})
	return err
}

func (h *gatewayAssets) mcps(ctx context.Context, req gatewayAssetRequest) (any, error) {
	if _, ok := h.gateway.(gateway.MCPManagementClient); !ok {
		return nil, assetError(501, "gateway MCP management is unavailable; use Agent CR only for this unsupported gateway")
	}
	if req.Action == "capabilities" {
		_, err := h.mcpGateway(ctx, "GET", "/v1/mcpServer", nil)
		return map[string]any{"available": err == nil, "kinds": []string{"proxy", "rest", "database"}, "gateway": h.gatewayURL}, err
	}
	if req.Action == "import" {
		if strings.TrimSpace(req.Content) == "" || len(req.Content) > 512000 {
			return nil, assetError(400, "invalid OpenAPI content")
		}
		value, err := h.mcpGateway(ctx, "POST", "/v1/mcpServer/swaggerToMcpConfig", map[string]any{"content": req.Content})
		if err != nil {
			return nil, err
		}
		var config map[string]any
		if yaml.Unmarshal([]byte(textField(value)), &config) != nil {
			return nil, assetError(502, "invalid OpenAPI conversion result")
		}
		tools, ok := config["tools"].([]any)
		if !ok {
			return nil, assetError(400, "no tools found in OpenAPI document")
		}
		raw, _ := yaml.Marshal(map[string]any{"tools": tools})
		return map[string]any{"rawConfig": string(raw), "toolCount": len(tools)}, nil
	}
	if req.Action == "list" {
		return h.listMCPAssets(ctx)
	}
	if req.Action == "save" {
		return h.saveMCP(ctx, req.Input)
	}
	record, err := h.readMCP(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if record == nil || !record.Managed {
		return nil, assetError(404, "managed MCP asset not found")
	}
	switch req.Action {
	case "get":
		return publicMCP(record), nil
	case "verify", "call":
		if record.GatewayState != "ready" {
			return nil, assetError(409, "save gateway configuration before verification")
		}
		if req.Action == "call" && req.Tool == "" {
			return nil, assetError(400, "tool is required")
		}
		verification := verifyMCP(ctx, record, req.Tool, req.Arguments)
		if req.Action == "verify" {
			record.Verification = verification
			if err := h.writeMCP(ctx, record); err != nil {
				return nil, err
			}
		}
		return verification, nil
	case "authorize", "bind", "unbind":
		desired := req.Workers
		if req.Action != "authorize" {
			if req.Worker == "" {
				return nil, assetError(400, "worker is required")
			}
			desired = without(record.AuthorizedWorkers, req.Worker)
			if req.Action == "bind" {
				desired = append(desired, req.Worker)
			}
		}
		if desired == nil || len(desired) > 100 {
			return nil, assetError(400, "workers must be an array of at most 100 names")
		}
		for _, name := range desired {
			if !regexp.MustCompile(`^[A-Za-z0-9][\w.-]*$`).MatchString(name) {
				return nil, assetError(400, "invalid worker name")
			}
		}
		if record.GatewayState != "ready" {
			return nil, assetError(409, "MCP gateway configuration is not ready")
		}
		for _, name := range desired {
			if !contains(record.AuthorizedWorkers, name) && record.Verification["success"] != true {
				return nil, assetError(409, "verify MCP gateway tools before granting a Worker")
			}
		}
		results := []any{}
		success := true
		targets := unique(append(append(append([]string{}, record.AuthorizedWorkers...), desired...), record.PendingRemovalWorkers...))
		if req.Action != "authorize" {
			targets = []string{req.Worker}
		}
		for _, name := range targets {
			err := h.bindMCP(ctx, record, name, contains(desired, name))
			if err != nil {
				success = false
			}
			results = append(results, assetResult(err, name))
		}
		return map[string]any{"success": success, "results": results, "authorizedWorkers": record.AuthorizedWorkers, "pendingRemovalWorkers": record.PendingRemovalWorkers, "verified": false}, nil
	case "delete":
		if len(record.AuthorizedWorkers)+len(record.PendingRemovalWorkers) > 0 {
			return nil, assetError(409, "remove all Worker bindings before deleting MCP")
		}
		var workers v1beta1.WorkerList
		var managers v1beta1.ManagerList
		if h.resources.reader().List(ctx, &workers, client.InNamespace(h.resources.namespace)) != nil || h.resources.reader().List(ctx, &managers, client.InNamespace(h.resources.namespace)) != nil {
			return nil, assetError(503, "cannot check MCP references")
		}
		for _, w := range workers.Items {
			for _, s := range w.Spec.McpServers {
				if s.Name == record.Name {
					return nil, assetError(409, "MCP is still referenced by a Worker")
				}
			}
		}
		for _, m := range managers.Items {
			for _, s := range m.Spec.McpServers {
				if s.Name == record.Name {
					return nil, assetError(409, "MCP is still referenced by a Manager")
				}
			}
		}
		for _, path := range []string{"/v1/mcpServer/" + record.Name, "/v1/service-sources/at-mcp-" + record.Name, "/v1/consumers/" + record.Probe.Name} {
			if _, err := h.mcpGateway(ctx, "DELETE", path, nil); err != nil {
				return nil, err
			}
		}
		if err := h.resources.oss.DeleteObject(ctx, mcpRegistryPrefix+record.Name+".json"); err != nil {
			return nil, assetError(503, "cannot delete MCP asset record")
		}
		return map[string]any{"success": true}, nil
	default:
		return nil, assetError(400, "unsupported MCP action")
	}
}

func (h *gatewayAssets) saveMCP(ctx context.Context, raw json.RawMessage) (any, error) {
	var record managedMCP
	if json.Unmarshal(raw, &record) != nil || !assetName.MatchString(record.Name) {
		return nil, assetError(400, "invalid MCP input")
	}
	existing, err := h.readMCP(ctx, record.Name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !existing.Managed || existing.Connection.Kind != record.Connection.Kind {
			return nil, assetError(409, "MCP name or kind conflicts with an existing asset")
		}
		if len(existing.AuthorizedWorkers) > 0 && existing.Transport != record.Connection.Transport && record.Connection.Kind == "proxy" {
			return nil, assetError(409, "remove Worker bindings before changing transport")
		}
		mergeMCPSecrets(&record.Connection, &existing.Connection)
		record.AuthorizedWorkers, record.PendingRemovalWorkers, record.WorkerConsumers, record.Probe, record.CreatedAt = existing.AuthorizedWorkers, existing.PendingRemovalWorkers, existing.WorkerConsumers, existing.Probe, existing.CreatedAt
	} else {
		record.AuthorizedWorkers = []string{}
		record.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		id, err := randomHex(6)
		if err != nil {
			return nil, err
		}
		key, err := randomHex(32)
		if err != nil {
			return nil, err
		}
		record.Probe.Name, record.Probe.Key = "at-mcp-check-"+id, key
	}
	if err := validateMCP(&record); err != nil {
		return nil, err
	}
	base, err := url.Parse(h.gatewayURL)
	if err != nil || base.Host == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, assetError(501, "gateway data plane is not configured; direct Agent CR configuration may be used")
	}
	base.Path, base.RawQuery, base.Fragment = "/mcp-servers/"+record.Name+"/mcp", "", ""
	if record.Connection.Kind == "proxy" && record.Connection.Transport == "sse" {
		base.Path = "/mcp-servers/" + record.Name + "/sse"
	}
	record.URL, record.Transport = base.String(), "streaminghttp"
	if record.Connection.Kind == "proxy" {
		record.Transport = record.Connection.Transport
	}
	record.Managed, record.GatewayState, record.UpdatedAt, record.Verification = true, "error", time.Now().UTC().Format(time.RFC3339Nano), nil
	if existing == nil {
		for _, path := range []string{"/v1/mcpServer/" + record.Name, "/v1/service-sources/at-mcp-" + record.Name} {
			value, err := h.mcpGateway(ctx, "GET", path, nil)
			if err != nil {
				return nil, err
			}
			if value != nil {
				return nil, assetError(409, "gateway already contains this MCP name or service source")
			}
		}
	}
	if err := h.writeMCP(ctx, &record); err != nil {
		return nil, err
	}
	err = h.applyMCP(ctx, &record)
	if err == nil {
		record.GatewayState = "ready"
	}
	if persist := h.writeMCP(ctx, &record); persist != nil {
		return nil, persist
	}
	if err != nil {
		return nil, err
	}
	return publicMCP(&record), nil
}

func (h *gatewayAssets) bindMCP(ctx context.Context, record *managedMCP, name string, enable bool) error {
	known := record.WorkerConsumers[name]
	if known == "" {
		known = "worker-" + name
	}
	if !enable {
		record.PendingRemovalWorkers = unique(append(record.PendingRemovalWorkers, name))
		if err := h.writeMCP(ctx, record); err != nil {
			return err
		}
		if _, err := h.mcpGateway(ctx, "DELETE", "/v1/mcpServer/consumers", map[string]any{"mcpServerName": record.Name, "consumers": []string{known}}); err != nil {
			return err
		}
		record.AuthorizedWorkers = without(record.AuthorizedWorkers, name)
		if err := h.writeMCP(ctx, record); err != nil {
			return err
		}
	}
	var worker v1beta1.Worker
	err := h.resources.reader().Get(ctx, client.ObjectKey{Name: name, Namespace: h.resources.namespace}, &worker)
	if err != nil && !(apierrors.IsNotFound(err) && !enable) {
		return assetError(404, "Worker not found")
	}
	if err == nil {
		consumer := "worker-" + worker.Spec.EffectiveWorkerName(worker.Name)
		if enable {
			// Grant first, then publish the runtime reference. A failed grant
			// leaves the Worker's existing configuration unchanged.
			if _, err = h.mcpGateway(ctx, "PUT", "/v1/mcpServer/consumers", map[string]any{"mcpServerName": record.Name, "consumers": []string{consumer}}); err != nil {
				return err
			}
		} else if known != consumer {
			if _, err = h.mcpGateway(ctx, "DELETE", "/v1/mcpServer/consumers", map[string]any{"mcpServerName": record.Name, "consumers": []string{consumer}}); err != nil {
				return err
			}
		}
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := h.resources.reader().Get(ctx, client.ObjectKeyFromObject(&worker), &worker); err != nil {
				return err
			}
			servers := []v1beta1.MCPServer{}
			var approvals *v1beta1.MCPApprovals
			for _, s := range worker.Spec.McpServers {
				if s.Name != record.Name {
					servers = append(servers, s)
				} else {
					approvals = s.Approvals
				}
			}
			if enable {
				transport := "http"
				if record.Transport == "sse" {
					transport = "sse"
				}
				servers = append(servers, v1beta1.MCPServer{Name: record.Name, URL: record.URL, Transport: transport, Approvals: approvals})
			}
			worker.Spec.McpServers = servers
			return h.resources.client.Update(ctx, &worker)
		})
		if err != nil {
			if enable && !contains(record.AuthorizedWorkers, name) {
				_, _ = h.mcpGateway(ctx, "DELETE", "/v1/mcpServer/consumers", map[string]any{"mcpServerName": record.Name, "consumers": []string{consumer}})
			}
			return assetError(502, "Worker configuration update failed; retry binding")
		}
		if record.WorkerConsumers == nil {
			record.WorkerConsumers = map[string]string{}
		}
		record.WorkerConsumers[name] = consumer
	}
	if enable {
		record.AuthorizedWorkers = unique(append(record.AuthorizedWorkers, name))
	}
	record.PendingRemovalWorkers = without(record.PendingRemovalWorkers, name)
	return h.writeMCP(ctx, record)
}
