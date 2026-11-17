package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Dashboard and agt use the same control-plane orchestration. The per-instance
// lock serializes read/modify/write asset operations in this Controller.
type gatewayAssets struct {
	gateway    gateway.Client
	resources  *ResourceHandler
	gatewayURL string
	mu         sync.Mutex
}

type gatewayAssetRequest struct {
	Action    string          `json:"action"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Update    bool            `json:"update"`
	Worker    string          `json:"worker"`
	Workers   []string        `json:"workers"`
	Model     string          `json:"model"`
	Provider  string          `json:"provider"`
	Tool      string          `json:"tool"`
	Arguments map[string]any  `json:"arguments"`
	Content   string          `json:"content"`
}

type gatewayAssetError struct {
	status  int
	message string
}

func (e *gatewayAssetError) Error() string        { return e.message }
func assetError(status int, message string) error { return &gatewayAssetError{status, message} }

var assetName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

func validModelName(name string) bool {
	return strings.TrimSpace(name) != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\?#")
}

func (h *gatewayAssets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var input gatewayAssetRequest
	if r.Method == "GET" {
		input.Action = "capabilities"
	} else if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input) != nil {
		httputil.WriteError(w, 400, "invalid asset request")
		return
	}
	if r.PathValue("asset") != "models" && r.PathValue("asset") != "mcps" {
		httputil.WriteError(w, 404, "unknown gateway asset")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var result any
	var err error
	if r.PathValue("asset") == "models" {
		result, err = h.models(r.Context(), input)
	} else {
		result, err = h.mcps(r.Context(), input)
	}
	if err != nil {
		status := 502
		var typed *gatewayAssetError
		if errors.As(err, &typed) {
			status = typed.status
		}
		httputil.WriteJSON(w, status, map[string]any{"error": err.Error(), "fallbackAllowed": status == 501})
		return
	}
	httputil.WriteJSON(w, 200, result)
}

func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func textField(value any) string      { result, _ := value.(string); return result }
func decodeObject(raw []byte) map[string]any {
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	return v
}
func unwrapAsset(value map[string]any) any {
	if v, ok := value["data"]; ok {
		return v
	}
	return value
}

func (h *gatewayAssets) ai(ctx context.Context, method, resource, name string, payload any) (any, error) {
	g, ok := h.gateway.(gateway.ManagementClient)
	if !ok {
		return nil, assetError(501, "gateway model management is unavailable; direct Agent CR configuration is supported as a fallback")
	}
	var body json.RawMessage
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	raw, status, err := g.ManageAI(ctx, method, resource, name, body)
	if err != nil {
		return nil, assetError(502, "gateway model connection failed; retry without changing Agent CR")
	}
	if status == 404 && method == "GET" {
		return nil, nil
	}
	if status < 200 || status >= 300 {
		return nil, assetError(502, "gateway rejected model "+resource+" operation")
	}
	v := decodeObject(raw)
	if success, ok := v["success"].(bool); ok && !success {
		return nil, assetError(502, "gateway rejected model "+resource+" operation")
	}
	return unwrapAsset(v), nil
}

func (h *gatewayAssets) models(ctx context.Context, req gatewayAssetRequest) (any, error) {
	if _, ok := h.gateway.(gateway.ManagementClient); !ok {
		return nil, assetError(501, "gateway model management is unavailable; use Agent CR only for this unsupported gateway")
	}
	switch req.Action {
	case "capabilities":
		_, err := h.ai(ctx, "GET", "providers", "", nil)
		return map[string]any{"available": err == nil, "gateway": h.gatewayURL}, err
	case "list", "get":
		if req.Name != "" && !validModelName(req.Name) {
			return nil, assetError(400, "invalid provider name")
		}
		v, err := h.ai(ctx, "GET", "providers", req.Name, nil)
		if err == nil && req.Name != "" && v == nil {
			return nil, assetError(404, "model provider not found")
		}
		maskProviderTokens(v)
		return v, err
	case "save":
		provider := decodeObject(req.Input)
		name := textField(provider["name"])
		if !validModelName(name) {
			return nil, assetError(400, "invalid provider name")
		}
		if req.Update {
			value, err := h.ai(ctx, "GET", "providers", name, nil)
			if err != nil {
				return nil, err
			}
			previous := object(value)
			if previous == nil {
				return nil, assetError(404, "model provider not found")
			}
			tokens, _ := provider["tokens"].([]any)
			if len(tokens) == 0 {
				tokens, _ = previous["tokens"].([]any)
				if len(tokens) == 0 {
					tokens, _ = object(previous["rawConfigs"])["apiTokens"].([]any)
				}
				if len(tokens) > 0 {
					provider["tokens"] = tokens
				}
			}
			for _, key := range []string{"type", "protocol", "rawConfigs", "tokenFailoverConfig"} {
				if _, exists := provider[key]; !exists {
					if value, ok := previous[key]; ok {
						provider[key] = value
					}
				}
			}
		}
		if message := validateAIManagement("providers", provider, req.Update); message != "" {
			return nil, assetError(400, message)
		}
		for alias, target := range object(object(provider["rawConfigs"])["modelMapping"]) {
			if strings.TrimSpace(alias) == "" || strings.Contains(alias, "*") || strings.HasPrefix(alias, "~") || textField(target) == "" {
				return nil, assetError(400, "automatic routes require explicit model aliases")
			}
		}
		method, target := "POST", ""
		if req.Update {
			method, target = "PUT", name
		}
		_, err := h.ai(ctx, method, "providers", target, provider)
		if err != nil {
			return nil, err
		}
		route, err := h.ensureModelRoute(ctx, name, provider)
		if err != nil {
			return nil, assetError(502, "model connection saved, but route synchronization failed; retry save with update=true")
		}
		return map[string]any{"name": name, "ready": route != "", "route": route, "message": "model connection saved; bind a Worker and verify its gateway identity before reporting it usable"}, nil
	case "verify", "probe":
		return h.probeModel(ctx, req)
	case "bind":
		if !validModelName(req.Provider) || req.Model == "" || req.Worker == "" {
			return nil, assetError(400, "worker, provider and model are required")
		}
		_, err := h.ai(ctx, "GET", "providers", req.Provider, nil)
		if err != nil {
			return nil, err
		}
		// Resolve first: a provider without a usable route must not replace an
		// Agent's working model. The reconciler also enforces this binding.
		info, err := h.gateway.ResolveModelProvider(ctx, req.Provider)
		if err != nil || info == nil {
			return nil, assetError(409, "model provider has no usable gateway route")
		}
		var worker v1beta1.Worker
		if err = h.resources.reader().Get(ctx, client.ObjectKey{Name: req.Worker, Namespace: h.resources.namespace}, &worker); err != nil {
			return nil, assetError(404, "worker not found")
		}
		if err = h.gateway.AuthorizeAIRoutes(ctx, "worker-"+worker.Spec.EffectiveWorkerName(worker.Name), info.HttpApiID); err != nil {
			return nil, assetError(502, "Worker model gateway authorization failed")
		}
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := h.resources.reader().Get(ctx, client.ObjectKeyFromObject(&worker), &worker); err != nil {
				return err
			}
			if worker.Spec.Model != req.Model || worker.Spec.ModelProvider != req.Provider {
				worker.Spec.Model, worker.Spec.ModelProvider = req.Model, req.Provider
				h.resources.appendModelGrant(ctx, &worker, req.Provider, req.Model)
			}
			return h.resources.client.Update(ctx, &worker)
		})
		if err != nil {
			return nil, assetError(502, "Worker model configuration update failed; retry the binding")
		}
		return map[string]any{"success": true, "worker": req.Worker, "model": req.Model, "provider": req.Provider, "configurationSaved": true, "verified": false}, nil
	default:
		return nil, assetError(400, "unsupported model action")
	}
}

func (h *gatewayAssets) ensureModelRoute(ctx context.Context, name string, provider map[string]any) (string, error) {
	mappings := object(object(provider["rawConfigs"])["modelMapping"])
	if len(mappings) == 0 {
		return "", nil
	}
	aliases := make([]string, 0, len(mappings))
	for alias, target := range mappings {
		if strings.TrimSpace(alias) == "" || strings.Contains(alias, "*") || strings.HasPrefix(alias, "~") || textField(target) == "" {
			return "", assetError(400, "automatic routes require explicit model aliases")
		}
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	list, err := h.ai(ctx, "GET", "routes", "", nil)
	if err != nil {
		return "", err
	}
	routes, _ := list.([]any)
	if v := object(list); v != nil {
		routes, _ = v["routes"].([]any)
	}
	name = "agentteams-" + name
	var route map[string]any
	for _, v := range routes {
		r := object(v)
		upstreams, _ := r["upstreams"].([]any)
		if textField(r["name"]) == name {
			if len(upstreams) != 1 || textField(object(upstreams[0])["provider"]) != textField(provider["name"]) {
				return "", assetError(409, "automatic route name is already in use")
			}
			route = r
		} else {
			for _, u := range upstreams {
				if textField(object(u)["provider"]) == textField(provider["name"]) {
					return textField(r["name"]), nil
				}
			}
		}
	}
	method, target := "POST", ""
	if route != nil {
		method, target = "PUT", name
		// List responses may be summaries. Preserve the complete route,
		// including its auth allowlist, fallback and upstream settings.
		detail, err := h.ai(ctx, "GET", "routes", name, nil)
		if err != nil {
			return "", err
		}
		if object(detail) == nil {
			return "", assetError(409, "existing managed route is unavailable")
		}
		route = object(detail)
	} else {
		route = map[string]any{"name": name, "pathPredicate": map[string]any{"matchType": "PRE", "matchValue": "/v1"}, "authConfig": map[string]any{"enabled": true, "allowedCredentialTypes": []string{"key-auth"}, "allowedConsumers": []string{"manager"}}}
	}
	predicates := []any{}
	for _, alias := range aliases {
		predicates = append(predicates, map[string]any{"matchType": "EQUAL", "matchValue": alias})
	}
	route["modelPredicates"] = predicates
	route["upstreams"] = []any{map[string]any{"provider": provider["name"], "weight": 100, "modelMapping": mappings}}
	_, err = h.ai(ctx, method, "routes", target, route)
	return name, err
}

func (h *gatewayAssets) mcpGateway(ctx context.Context, method, path string, body any) (any, error) {
	g, ok := h.gateway.(gateway.MCPManagementClient)
	if !ok {
		return nil, assetError(501, "gateway MCP management is unavailable; use Agent CR only for this unsupported gateway")
	}
	return g.ManageMCP(ctx, method, path, body)
}

// reader selects the direct API reader for mutation preconditions.
func (h *ResourceHandler) reader() client.Reader {
	if h.apiReader != nil {
		return h.apiReader
	}
	return h.client
}

func assetResult(err error, worker string) map[string]any {
	message := "gateway permission and Worker configuration saved; runtime verification is still required"
	if err != nil {
		message = fmt.Sprint(err)
	}
	return map[string]any{"name": worker, "success": err == nil, "message": message}
}
