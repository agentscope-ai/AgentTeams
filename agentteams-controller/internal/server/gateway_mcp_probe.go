package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type mcpEvents struct {
	scanner *bufio.Scanner
	size    int
}

func newMCPEvents(body io.Reader) *mcpEvents {
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 4096), 2<<20)
	return &mcpEvents{scanner: s}
}
func (s *mcpEvents) next() (string, []byte, error) {
	event := "message"
	data := []string{}
	for s.scanner.Scan() {
		line := s.scanner.Text()
		s.size += len(line)
		if s.size > 2<<20 {
			return "", nil, fmt.Errorf("MCP response exceeded 2 MB")
		}
		if line == "" {
			if len(data) > 0 {
				return event, []byte(strings.Join(data, "\n")), nil
			}
			event = "message"
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[6:])
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
	if err := s.scanner.Err(); err != nil {
		return "", nil, fmt.Errorf("MCP stream failed or timed out")
	}
	if len(data) > 0 {
		return event, []byte(strings.Join(data, "\n")), nil
	}
	return "", nil, io.EOF
}

type managedMCPClient struct {
	ctx                             context.Context
	http                            *http.Client
	endpoint, key, session, version string
	id                              int
	stream                          *mcpEvents
}

func (c *managedMCPClient) send(payload any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(c.ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid MCP gateway endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	req.Header.Set("MCP-Protocol-Version", c.version)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MCP gateway connection failed or timed out")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		res.Body.Close()
		return nil, fmt.Errorf("MCP gateway returned HTTP %d; check permission and upstream", res.StatusCode)
	}
	if session := res.Header.Get("Mcp-Session-Id"); session != "" {
		c.session = session
	}
	return res, nil
}
func (c *managedMCPClient) rpc(method string, params any) (map[string]any, error) {
	c.id++
	res, err := c.send(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var raw []byte
	stream := c.stream
	if stream != nil {
		res.Body.Close()
	} else if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		stream = newMCPEvents(res.Body)
	}
	if stream != nil {
		for {
			_, data, err := stream.next()
			if err != nil {
				return nil, fmt.Errorf("MCP %s stream did not return a result", method)
			}
			var envelope struct {
				ID int `json:"id"`
			}
			if json.Unmarshal(data, &envelope) == nil && envelope.ID == c.id {
				raw = data
				break
			}
		}
	} else {
		raw, err = io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
		if err != nil || len(raw) > 2<<20 {
			return nil, fmt.Errorf("invalid MCP response size")
		}
	}
	var envelope struct {
		ID      int             `json:"id"`
		JSONRPC string          `json:"jsonrpc"`
		Result  map[string]any  `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.ID != c.id || envelope.JSONRPC != "2.0" || envelope.Result == nil || (len(envelope.Error) > 0 && string(envelope.Error) != "null") {
		return nil, fmt.Errorf("MCP %s returned an invalid result", method)
	}
	return envelope.Result, nil
}

func verifyMCP(parent context.Context, record *managedMCP, tool string, args map[string]any) map[string]any {
	timeout := record.Connection.Timeout
	if timeout < 1000 || timeout > 30000 {
		timeout = 15000
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	c := &managedMCPClient{ctx: ctx, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, endpoint: record.URL, key: record.Probe.Key, version: "2025-03-26"}
	started := time.Now()
	result, err := func() (map[string]any, error) {
		if record.Transport == "sse" {
			req, err := http.NewRequestWithContext(ctx, "GET", record.URL, nil)
			if err != nil {
				return nil, fmt.Errorf("invalid MCP endpoint")
			}
			req.Header.Set("Authorization", "Bearer "+c.key)
			req.Header.Set("Accept", "text/event-stream")
			res, err := c.http.Do(req)
			if err != nil {
				return nil, fmt.Errorf("MCP SSE connection failed")
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				return nil, fmt.Errorf("MCP SSE returned HTTP %d", res.StatusCode)
			}
			c.stream = newMCPEvents(res.Body)
			for {
				event, data, err := c.stream.next()
				if err != nil {
					return nil, fmt.Errorf("MCP SSE endpoint unavailable")
				}
				if event != "endpoint" {
					continue
				}
				base, _ := url.Parse(record.URL)
				target, err := base.Parse(string(data))
				if err != nil || target.Scheme != base.Scheme || target.Host != base.Host || target.User != nil {
					return nil, fmt.Errorf("MCP SSE endpoint must use the gateway origin")
				}
				c.endpoint = target.String()
				break
			}
		}
		init, err := c.rpc("initialize", map[string]any{"protocolVersion": c.version, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "agentteams-controller", "version": "1"}})
		if err != nil {
			return nil, err
		}
		if textField(init["protocolVersion"]) == "" || object(init["serverInfo"]) == nil {
			return nil, fmt.Errorf("invalid MCP initialize result")
		}
		c.version = textField(init["protocolVersion"])
		notification, err := c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		if err != nil {
			return nil, err
		}
		notification.Body.Close()
		tools := []any{}
		cursor := ""
		for page := 0; page < 20; page++ {
			params := map[string]any{}
			if cursor != "" {
				params["cursor"] = cursor
			}
			list, err := c.rpc("tools/list", params)
			if err != nil {
				return nil, err
			}
			items, ok := list["tools"].([]any)
			if !ok {
				return nil, fmt.Errorf("invalid MCP tools list")
			}
			for _, v := range items {
				t := object(v)
				if textField(t["name"]) == "" || object(t["inputSchema"]) == nil {
					return nil, fmt.Errorf("invalid MCP tool schema")
				}
			}
			tools = append(tools, items...)
			next := textField(list["nextCursor"])
			if next == "" {
				cursor = ""
				break
			}
			if next == cursor {
				return nil, fmt.Errorf("repeated MCP tools cursor")
			}
			cursor = next
		}
		if cursor != "" {
			return nil, fmt.Errorf("MCP tool listing exceeded pagination limit")
		}
		out := map[string]any{"tools": tools}
		if tool != "" {
			found := false
			for _, v := range tools {
				if object(v)["name"] == tool {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("MCP tool not found")
			}
			if args == nil {
				args = map[string]any{}
			}
			out["result"], err = c.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}()
	if result == nil {
		result = map[string]any{}
	}
	success := err == nil && object(result["result"])["isError"] != true
	message := "gateway MCP tools verified"
	if tool != "" {
		message = "MCP tool call completed"
	}
	if !success {
		message = "MCP tool returned a business error"
	}
	if err != nil {
		message = err.Error()
	}
	result["success"], result["message"], result["checkedAt"], result["latencyMs"] = success, message, time.Now().UTC().Format(time.RFC3339Nano), time.Since(started).Milliseconds()
	// Upstream descriptions and tool results can echo credentials.
	secrets := []string{record.Probe.Key, record.Connection.Auth.Secret}
	if record.Connection.Database != nil {
		secrets = append(secrets, record.Connection.Database.Password)
	}
	for _, p := range append(append([]mcpParameter{}, record.Connection.Headers...), record.Connection.Query...) {
		if p.Secret {
			secrets = append(secrets, p.Value)
		}
	}
	if record.Connection.Auth.Type == "basic" {
		secrets = append(secrets, base64.StdEncoding.EncodeToString([]byte(record.Connection.Auth.Username+":"+record.Connection.Auth.Secret)))
	}
	var redact func(any) any
	redact = func(v any) any {
		switch v := v.(type) {
		case string:
			for _, s := range secrets {
				if s != "" {
					v = strings.ReplaceAll(v, s, "••••••")
					v = strings.ReplaceAll(v, url.QueryEscape(s), "••••••")
				}
			}
			return v
		case []any:
			for i := range v {
				v[i] = redact(v[i])
			}
			return v
		case map[string]any:
			for k := range v {
				v[k] = redact(v[k])
			}
			return v
		default:
			return v
		}
	}
	return redact(result).(map[string]any)
}
