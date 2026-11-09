package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

func (h *GatewayHandler) ManageAI(w http.ResponseWriter, r *http.Request) {
	client, ok := h.gw.(gateway.ManagementClient)
	if !ok {
		httputil.WriteError(w, http.StatusNotImplemented, "gateway management is not supported by this backend")
		return
	}
	resource, name := r.PathValue("resource"), r.PathValue("name")
	if resource != "providers" && resource != "routes" {
		httputil.WriteError(w, 404, "unknown AI resource")
		return
	}
	if strings.ContainsAny(name, "/\\?#") || name == "." || name == ".." {
		httputil.WriteError(w, 400, "invalid resource name")
		return
	}
	var body json.RawMessage
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			httputil.WriteError(w, 400, "invalid request body")
			return
		}
		var value map[string]interface{}
		if json.Unmarshal(payload, &value) != nil || value == nil {
			httputil.WriteError(w, 400, "a JSON object is required")
			return
		}
		if name != "" {
			value["name"] = name
		}
		if err := validateAIManagement(resource, value, r.Method == http.MethodPut); err != "" {
			httputil.WriteError(w, 400, err)
			return
		}
		// Keep the existing fallback-write opt-in: the Console's support varies by version.
		if resource == "routes" && !h.fallbackConfigWritable {
			delete(value, "fallbackConfig")
		}
		body, _ = json.Marshal(value)
	}
	response, status, err := client.ManageAI(r.Context(), r.Method, resource, name, body)
	if err != nil {
		httputil.WriteError(w, 502, "gateway request failed")
		return
	}
	if status < 200 || status >= 300 {
		if status == 401 || status == 403 || status < 400 || status >= 500 {
			status = 502
		}
		httputil.WriteError(w, status, "gateway rejected the operation")
		return
	}
	if len(response) == 0 {
		w.WriteHeader(status)
		return
	}
	var value interface{}
	if json.Unmarshal(response, &value) != nil {
		httputil.WriteError(w, 502, "invalid gateway response")
		return
	}
	if resource == "providers" {
		maskProviderTokens(value)
	}
	if resource == "routes" {
		if envelope, ok := value.(map[string]interface{}); ok {
			envelope["fallbackConfigWritable"] = h.fallbackConfigWritable
		}
	}
	httputil.WriteJSON(w, status, value)
}

func maskProviderTokens(value interface{}) {
	switch v := value.(type) {
	case []interface{}:
		for _, item := range v {
			maskProviderTokens(item)
		}
	case map[string]interface{}:
		if tokens, ok := v["tokens"].([]interface{}); ok {
			v["tokenCount"] = len(tokens)
		}
		delete(v, "tokens")
		// Higress duplicates provider credentials under rawConfigs.apiTokens.
		delete(v, "apiTokens")
		for _, item := range v {
			maskProviderTokens(item)
		}
	}
}

func validateAIManagement(resource string, value map[string]interface{}, update bool) string {
	nonempty := func(v interface{}) bool { s, ok := v.(string); return ok && strings.TrimSpace(s) != "" }
	if !update && !nonempty(value["name"]) {
		return "name is required"
	}
	if resource == "providers" {
		if v, exists := value["type"]; (!update || exists) && !nonempty(v) {
			return "provider type is required"
		}
		if v, exists := value["tokens"]; !update || exists {
			tokens, ok := v.([]interface{})
			if !ok || (!update && len(tokens) == 0) {
				return "provider tokens must be an array"
			}
			for _, token := range tokens {
				if !nonempty(token) {
					return "provider tokens must contain nonempty strings"
				}
			}
		}
		if v, exists := value["protocol"]; exists && v != "openai/v1" && v != "original" {
			return "invalid provider protocol"
		}
		if v, exists := value["rawConfigs"]; exists {
			if _, ok := v.(map[string]interface{}); !ok {
				return "rawConfigs must be an object"
			}
		}
	} else {
		if v, exists := value["pathPredicate"]; !update || exists {
			p, ok := v.(map[string]interface{})
			if !ok || p["matchType"] != "PRE" || !nonempty(p["matchValue"]) {
				return "pathPredicate must use PRE matching"
			}
		}
		if v, exists := value["upstreams"]; !update || exists {
			upstreams, ok := v.([]interface{})
			if !ok || len(upstreams) == 0 {
				return "upstreams are required"
			}
			total := 0.0
			for _, item := range upstreams {
				u, ok := item.(map[string]interface{})
				if !ok || !nonempty(u["provider"]) {
					return "invalid upstream"
				}
				weight, ok := u["weight"].(float64)
				if !ok || weight < 0 || weight > 100 || weight != float64(int(weight)) {
					return "invalid upstream weight"
				}
				total += weight
			}
			if len(upstreams) > 1 && total != 100 {
				return "upstream weights must total 100"
			}
		}
		if v, exists := value["authConfig"]; exists {
			a, ok := v.(map[string]interface{})
			if !ok {
				return "invalid authConfig"
			}
			enabled, ok := a["enabled"].(bool)
			if !ok {
				return "invalid authConfig.enabled"
			}
			types, ok := a["allowedCredentialTypes"].([]interface{})
			if !ok || (enabled && len(types) == 0) {
				return "invalid credential types"
			}
			for _, item := range types {
				if _, ok := item.(string); !ok {
					return "invalid credential type"
				}
			}
			if consumers, exists := a["allowedConsumers"]; exists {
				items, ok := consumers.([]interface{})
				if !ok {
					return "invalid allowedConsumers"
				}
				for _, item := range items {
					if _, ok := item.(string); !ok {
						return "invalid consumer"
					}
				}
			}
		}
	}
	return ""
}
