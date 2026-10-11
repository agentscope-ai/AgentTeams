package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Probes use persisted worker identity and saved configuration, never caller
// supplied URLs, tokens, prompts or tools. They do not change permissions.
type workerGatewayProbe struct {
	client                client.Client
	namespace, gatewayURL string
	key                   func(context.Context, string) (string, error)
}

func (h *workerGatewayProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !canManageWorkerEnv(r) {
		httputil.WriteError(w, 403, "gateway probes require admin or manager access")
		return
	}
	var input struct {
		Kind   string `json:"kind"`
		Server string `json:"server"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&input) != nil || (input.Kind != "model" && input.Kind != "mcp") {
		httputil.WriteError(w, 400, "kind must be model or mcp")
		return
	}
	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: r.PathValue("name"), Namespace: h.namespace}, &worker); err != nil {
		writeK8sError(w, "get worker", err)
		return
	}
	if worker.Spec.ModelProvider != "" {
		httputil.WriteError(w, 501, "provider-specific gateway probing is not available; verify using the provider console")
		return
	}
	base, err := url.Parse(h.gatewayURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil {
		httputil.WriteError(w, 503, "gateway URL not configured")
		return
	}
	endpoint := strings.TrimRight(h.gatewayURL, "/") + "/v1/chat/completions"
	if input.Kind == "model" && worker.Spec.Model == "" {
		httputil.WriteError(w, 409, "save an explicit worker model before probing")
		return
	}
	if input.Kind == "mcp" {
		endpoint = ""
		for _, srv := range worker.Spec.McpServers {
			if srv.Name == input.Server {
				endpoint = srv.URL
				break
			}
		}
		target, e := url.Parse(endpoint)
		if e != nil || target.Host != base.Host || target.Scheme != base.Scheme || target.User != nil || !strings.HasPrefix(target.Path, "/mcp-servers/") || path.Clean(target.Path) != target.Path || target.RawQuery != "" || target.Fragment != "" {
			httputil.WriteError(w, 400, "select a saved MCP endpoint on the configured gateway under /mcp-servers/")
			return
		}
	}
	if h.key == nil {
		httputil.WriteError(w, 503, "worker credentials unavailable")
		return
	}
	key, err := h.key(r.Context(), worker.Name)
	if err != nil {
		httputil.WriteError(w, 409, "worker credentials unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	started := time.Now()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	session, version := "", ""
	call := func(payload any) (json.RawMessage, error) {
		b, _ := json.Marshal(payload)
		req, e := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
		if e != nil {
			return nil, fmt.Errorf("invalid gateway endpoint")
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		if version != "" {
			req.Header.Set("MCP-Protocol-Version", version)
		}
		res, e := hc.Do(req)
		if e != nil {
			return nil, fmt.Errorf("gateway request failed or timed out")
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return nil, fmt.Errorf("gateway returned HTTP %d; check Consumer authorization, route and upstream", res.StatusCode)
		}
		if id := res.Header.Get("Mcp-Session-Id"); id != "" {
			session = id
		}
		if res.StatusCode == 202 || res.StatusCode == 204 {
			return nil, nil
		}
		reader := io.LimitReader(res.Body, 1<<20)
		if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
			scanner := bufio.NewScanner(reader)
			scanner.Buffer(make([]byte, 4096), 1<<20)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data:") {
					raw := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
					var event map[string]json.RawMessage
					if json.Unmarshal(raw, &event) == nil && event["id"] != nil {
						return raw, nil
					}
				}
			}
			return nil, fmt.Errorf("no MCP response received")
		}
		b, e = io.ReadAll(reader)
		if e != nil {
			return nil, fmt.Errorf("cannot read gateway response")
		}
		return b, nil
	}
	var probeErr error
	if input.Kind == "model" {
		var raw json.RawMessage
		raw, probeErr = call(map[string]any{"model": worker.Spec.Model, "messages": []map[string]string{{"role": "user", "content": "Reply OK."}}, "max_tokens": 8, "stream": false})
		if probeErr == nil {
			var response struct {
				Choices []json.RawMessage `json:"choices"`
			}
			if json.Unmarshal(raw, &response) != nil || len(response.Choices) == 0 {
				probeErr = fmt.Errorf("gateway response did not contain model choices")
			}
		}
	} else {
		rpc := func(id int, method string, params any) (json.RawMessage, error) {
			payload := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
			if id > 0 {
				payload["id"] = id
			}
			raw, e := call(payload)
			if e != nil || id == 0 {
				return raw, e
			}
			var envelope struct {
				ID       int             `json:"id"`
				Protocol string          `json:"jsonrpc"`
				Result   json.RawMessage `json:"result"`
				Error    json.RawMessage `json:"error"`
			}
			if json.Unmarshal(raw, &envelope) != nil || envelope.ID != id || envelope.Protocol != "2.0" || (len(envelope.Error) > 0 && string(envelope.Error) != "null") || len(envelope.Result) == 0 {
				return nil, fmt.Errorf("MCP %s failed or returned an invalid response", method)
			}
			return envelope.Result, nil
		}
		var result json.RawMessage
		result, probeErr = rpc(1, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "agentteams-probe", "version": "1"}})
		if probeErr == nil {
			var init struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(result, &init) != nil || init.ProtocolVersion == "" {
				probeErr = fmt.Errorf("invalid MCP initialization response")
			} else {
				version = init.ProtocolVersion
			}
		}
		if probeErr == nil {
			_, probeErr = rpc(0, "notifications/initialized", map[string]any{})
		}
		if probeErr == nil {
			result, probeErr = rpc(2, "tools/list", map[string]any{})
			if probeErr == nil {
				var list struct {
					Tools *[]json.RawMessage `json:"tools"`
				}
				if json.Unmarshal(result, &list) != nil || list.Tools == nil {
					probeErr = fmt.Errorf("invalid MCP tools list")
				}
			}
		}
	}
	message := "Worker identity verified through gateway"
	if probeErr != nil {
		message = probeErr.Error()
	}
	httputil.WriteJSON(w, 200, map[string]any{"success": probeErr == nil, "message": message, "latencyMs": time.Since(started).Milliseconds(), "consumer": "worker-" + worker.Spec.EffectiveWorkerName(worker.Name), "kind": input.Kind})
}
