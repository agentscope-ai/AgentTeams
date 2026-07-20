package agentconfig

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Generator produces worker runtime configuration files in pure Go.
type Generator struct {
	config Config
}

// NewGenerator creates an agent config generator.
func NewGenerator(cfg Config) *Generator {
	if cfg.AdminUser == "" {
		cfg.AdminUser = "admin"
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "qwen3.6-plus"
	}
	return &Generator{config: cfg}
}

// GenerateOpenClawConfig produces the openclaw.json content for a worker.
func (g *Generator) GenerateOpenClawConfig(req WorkerConfigRequest) ([]byte, error) {
	modelName := req.ModelName
	if modelName == "" {
		modelName = g.config.DefaultModel
	}
	modelName = strings.TrimPrefix(modelName, "agentteams-gateway/")

	matrixServerURL := g.config.MatrixServerURL
	if matrixServerURL == "" {
		// K8s deployments must set AGENTTEAMS_MATRIX_URL (Helm injects it automatically).
		// This default only applies to docker/embedded mode.
		matrixServerURL = "http://matrix-local.agentteams.io:8080"
	}

	aiGatewayURL := g.config.AIGatewayURL
	if req.AIGatewayURL != "" {
		aiGatewayURL = req.AIGatewayURL
	}
	if aiGatewayURL == "" {
		aiGatewayURL = "http://aigw-local.agentteams.io:8080"
	}

	matrixDomain := g.config.MatrixDomain
	if matrixDomain == "" {
		matrixDomain = "matrix-local.agentteams.io:8080"
	}

	adminUser := g.config.AdminUser
	adminMatrixID := fmt.Sprintf("@%s:%s", adminUser, matrixDomain)

	// Build the base openclaw.json structure (must match OpenClaw schema).
	//
	// gateway.port: 18799 — openclaw 2026.4.x onwards merges the Control UI HTTP
	// server into the same listener as the gateway WebSocket (older versions ran
	// the Control UI on a separate 18799 listener). Hiclaw's container port
	// mapping (host:AGENTTEAMS_PORT_MANAGER_CONSOLE → container:18799), Dockerfile
	// EXPOSE, install/health probes and the legacy nginx reverse proxy all
	// assume the user-facing console reaches us on 18799. Keep that contract by
	// pinning the gateway port to 18799 — the alternative (rewiring every
	// downstream consumer) is far more invasive.
	//
	// gateway.bind: "lan" — openclaw 2026.4.x defaults to loopback (127.0.0.1)
	// binding for the gateway/Control UI server. In hiclaw's embedded dual-
	// container topology the manager runs in its own container and the Control
	// UI is reached via a host port mapping (host:18888 → manager:18799), which
	// requires the listener to be reachable from outside the container's loop-
	// back interface. Bind LAN-wide (0.0.0.0); access remains gated by the
	// shared gateway token.
	//
	// gateway.controlUi.dangerouslyDisableDeviceAuth: true — Higress-hosted
	// console access uses the shared gateway token (no per-device pairing). The
	// manager template carries this flag; the controller-pushed config must
	// preserve it too, otherwise the mc-mirror sync strips it and the Control
	// UI starts demanding device authentication that hiclaw never provisions.
	//
	// gateway.controlUi.allowInsecureAuth: true — hiclaw exposes the console
	// over plain HTTP on the user's host port (AGENTTEAMS_PORT_MANAGER_CONSOLE),
	// so the strict HTTPS-only browser-auth checks introduced in 2026.4.x must
	// be relaxed.
	//
	// gateway.controlUi.allowedOrigins: ["*"] — the user picks the console host
	// port at install time and may reach it via 127.0.0.1, the host's LAN IP,
	// or a custom hostname; hiclaw cannot enumerate every legitimate origin
	// upfront. Token auth on the gateway remains the actual access boundary.
	//
	// gateway.auth.token / gateway.remote.token: req.GatewayKey — these used to
	// be `generateRandomHex(32)` per call, which produced a fresh value on every
	// reconcile. The agent file-sync skill pulls openclaw.json from MinIO on its
	// heartbeat tick; openclaw 2026.4.x diff-watches the config and any change
	// to gateway.auth.token forces a full gateway *restart* (matrix client is
	// torn down and recreated, in-flight agent dispatches are dropped). With
	// the default reconcile cadence this caused the manager to silently restart
	// every ~5 minutes — long-running tasks (test-06 onwards) lost their
	// in-flight reply, the test framework saw "Manager replied empty" and the
	// CI matrix turned red. The manager-side boot template already pins both
	// fields to the stable MANAGER_GATEWAY_KEY (loaded from creds.GatewayKey);
	// mirror that here so the controller-pushed config stays byte-stable across
	// reconciles. The token doubles as the Control UI / remote auth token, so
	// reusing GatewayKey is consistent with the manager template.
	config := map[string]interface{}{
		"gateway": map[string]interface{}{
			"mode": "local",
			"port": 18799,
			"bind": "lan",
			"auth": map[string]interface{}{
				"token": req.GatewayKey,
			},
			"remote": map[string]interface{}{
				"token": req.GatewayKey,
			},
			"controlUi": map[string]interface{}{
				"dangerouslyDisableDeviceAuth": true,
				"allowInsecureAuth":            true,
				"allowedOrigins":               []string{"*"},
			},
		},
		"channels": map[string]interface{}{
			"matrix": g.buildMatrixChannelConfig(req, matrixServerURL, matrixDomain, adminMatrixID),
		},
		"models": map[string]interface{}{
			"mode": "merge",
			"providers": map[string]interface{}{
				"agentteams-gateway": map[string]interface{}{
					"baseUrl": aiGatewayURL + "/v1",
					"apiKey":  req.GatewayKey,
					"api":     "openai-completions",
					"models":  g.allModelSpecs(modelName),
				},
			},
		},
		"agents": map[string]interface{}{
			"defaults": map[string]interface{}{
				"timeoutSeconds": 1800,
				"workspace":      "~",
				"model": map[string]interface{}{
					"primary": "agentteams-gateway/" + modelName,
				},
				"models":        g.allModelAliases(modelName),
				"maxConcurrent": 4,
				"subagents": map[string]interface{}{
					"maxConcurrent": 8,
				},
			},
		},
		"session": map[string]interface{}{
			"resetByType": map[string]interface{}{
				"dm":    map[string]interface{}{"mode": "daily", "atHour": 4},
				"group": map[string]interface{}{"mode": "daily", "atHour": 4},
			},
		},
		"plugins": map[string]interface{}{
			"load": map[string]interface{}{
				"paths": []string{"/opt/openclaw/extensions/matrix"},
			},
			"entries": map[string]interface{}{
				"matrix": map[string]interface{}{"enabled": true},
				"memory-core": map[string]interface{}{
					"enabled": true,
					"config": map[string]interface{}{
						"dreaming": map[string]interface{}{
							"enabled": true,
						},
					},
				},
			},
		},
	}

	// Add heartbeat config for team leaders
	if req.Heartbeat != nil && req.Heartbeat.Enabled {
		agents := config["agents"].(map[string]interface{})
		defaults := agents["defaults"].(map[string]interface{})
		hb := map[string]interface{}{"enabled": true}
		if req.Heartbeat.Every != "" {
			hb["every"] = req.Heartbeat.Every
		}
		defaults["heartbeat"] = hb
	}

	// Add embedding model for memory search if configured
	if g.config.EmbeddingModel != "" {
		agents := config["agents"].(map[string]interface{})
		defaults := agents["defaults"].(map[string]interface{})
		defaults["memorySearch"] = map[string]interface{}{
			"provider": "openai",
			"model":    g.config.EmbeddingModel,
			"remote": map[string]interface{}{
				"baseUrl": aiGatewayURL + "/v1",
				"apiKey":  req.GatewayKey,
			},
		}
	}

	// Apply channel policy overrides
	if req.ChannelPolicy != nil {
		g.applyChannelPolicy(config, req.ChannelPolicy, matrixDomain)
	}

	return json.MarshalIndent(config, "", "  ")
}

func (g *Generator) buildMatrixChannelConfig(req WorkerConfigRequest, serverURL, domain, adminMatrixID string) map[string]interface{} {
	workerMatrixID := fmt.Sprintf("@%s:%s", req.WorkerName, domain)

	// Default allow list: Manager + Admin
	managerMatrixID := fmt.Sprintf("@manager:%s", domain)
	groupAllowFrom := []string{managerMatrixID, adminMatrixID}
	dmAllowFrom := []string{managerMatrixID, adminMatrixID}

	// Team worker: use Leader + Admin instead
	if req.TeamLeaderName != "" {
		leaderMatrixID := fmt.Sprintf("@%s:%s", req.TeamLeaderName, domain)
		groupAllowFrom = []string{leaderMatrixID, adminMatrixID}
		dmAllowFrom = []string{leaderMatrixID, adminMatrixID}
	}

	cfg := map[string]interface{}{
		"homeserver":  serverURL,
		"enabled":     true,
		"userId":      workerMatrixID,
		"accessToken": req.MatrixToken,
		"encryption":  g.config.E2EEEnabled,
		"dm": map[string]interface{}{
			"policy":    "allowlist",
			"allowFrom": dmAllowFrom,
		},
		"groupPolicy":    "allowlist",
		"groupAllowFrom": groupAllowFrom,
		"groups": map[string]interface{}{
			"*": map[string]interface{}{"allow": true, "requireMention": true},
		},
		// Matrix plugin: editable preview while the LLM streams; one message per assistant block.
		"streaming":      "partial",
		"blockStreaming": true,
		// openclaw 2026.4.x onwards forwards the SSRF policy to the matrix-js-sdk
		// fetch path. Without this opt-in, /sync to private hosts (the embedded
		// `matrix-local.agentteams.io` alias resolves to 127.0.0.1, k8s service DNS
		// resolves to ClusterIP) is rejected and the worker never reaches a
		// PREPARED state — no room joins, no message delivery. The manager
		// template carries the same flag; mirror it here so every worker the
		// controller provisions can talk to the (always private) homeserver.
		"network": map[string]interface{}{
			"dangerouslyAllowPrivateNetwork": true,
		},
		// openclaw 2026.4.x defaults the matrix invite-handler policy to "off",
		// meaning agents *ignore* room invites — they stay in `membership:invite`
		// forever instead of becoming `join`. Hiclaw's provisioning flow always
		// invites the agent (manager or worker) into its dedicated room and then
		// expects the agent's matrix client to accept the invite on its own; if
		// it never joins, /sync delivers no room events, the agent is silent in
		// its own room and every dependent test (test-06 onwards) fails with
		// "Manager replied empty" / "Worker did not start". Force "always" so
		// any invite (the room-create flow + any future re-invites after a token
		// rotation or membership reset) is accepted automatically. Per-room
		// access is already gated by the matrix server's invite ACLs and our
		// dm/groupPolicy allowlists below, so accepting all invites doesn't
		// widen the trust boundary.
		"autoJoin": "always",
	}

	return cfg
}

func (g *Generator) applyChannelPolicy(config map[string]interface{}, policy *ChannelPolicy, domain string) {
	channels, _ := config["channels"].(map[string]interface{})
	if channels == nil {
		return
	}
	matrixCfg, _ := channels["matrix"].(map[string]interface{})
	if matrixCfg == nil {
		return
	}

	resolveID := func(s string) string {
		if strings.HasPrefix(s, "@") {
			return s
		}
		return fmt.Sprintf("@%s:%s", s, domain)
	}

	// GroupAllowFrom additions
	if len(policy.GroupAllowExtra) > 0 {
		existing := toStringSlice(matrixCfg["groupAllowFrom"])
		for _, u := range policy.GroupAllowExtra {
			id := resolveID(u)
			if !containsString(existing, id) {
				existing = append(existing, id)
			}
		}
		matrixCfg["groupAllowFrom"] = existing
	}

	// DM AllowFrom additions
	if len(policy.DMAllowExtra) > 0 {
		dm, _ := matrixCfg["dm"].(map[string]interface{})
		if dm != nil {
			existing := toStringSlice(dm["allowFrom"])
			for _, u := range policy.DMAllowExtra {
				id := resolveID(u)
				if !containsString(existing, id) {
					existing = append(existing, id)
				}
			}
			dm["allowFrom"] = existing
		}
	}

	// GroupDenyExtra: remove from groupAllowFrom
	if len(policy.GroupDenyExtra) > 0 {
		existing := toStringSlice(matrixCfg["groupAllowFrom"])
		denySet := make(map[string]bool)
		for _, u := range policy.GroupDenyExtra {
			denySet[resolveID(u)] = true
		}
		var filtered []string
		for _, id := range existing {
			if !denySet[id] {
				filtered = append(filtered, id)
			}
		}
		matrixCfg["groupAllowFrom"] = filtered
	}

	// DMDenyExtra: remove from dm.allowFrom
	if len(policy.DMDenyExtra) > 0 {
		dm, _ := matrixCfg["dm"].(map[string]interface{})
		if dm != nil {
			existing := toStringSlice(dm["allowFrom"])
			denySet := make(map[string]bool)
			for _, u := range policy.DMDenyExtra {
				denySet[resolveID(u)] = true
			}
			var filtered []string
			for _, id := range existing {
				if !denySet[id] {
					filtered = append(filtered, id)
				}
			}
			dm["allowFrom"] = filtered
		}
	}
}

// resolveModelSpec returns model parameters, applying config overrides.
func (g *Generator) resolveModelSpec(modelName string) ModelSpec {
	spec := defaultModelSpec(modelName)

	// Apply user overrides
	if g.config.ModelContextWindow > 0 {
		spec.ContextWindow = g.config.ModelContextWindow
	}
	if g.config.ModelMaxTokens > 0 {
		spec.MaxTokens = g.config.ModelMaxTokens
	}
	if g.config.ModelVision != nil {
		if *g.config.ModelVision {
			spec.Input = []string{"text", "image"}
		} else {
			spec.Input = []string{"text"}
		}
	}
	if g.config.ModelReasoning != nil {
		spec.Reasoning = *g.config.ModelReasoning
	}

	return spec
}

// defaultModelSpec returns built-in parameters for known models.
func defaultModelSpec(modelName string) ModelSpec {
	type preset struct {
		ctx, max int
		vision   bool
		reason   bool
	}

	presets := map[string]preset{
		"gpt-5.3-codex":          {400000, 128000, true, true},
		"gpt-5-mini":             {400000, 128000, true, true},
		"gpt-5-nano":             {400000, 128000, true, true},
		"claude-opus-4-6":        {1000000, 128000, true, true},
		"claude-sonnet-4-6":      {1000000, 64000, true, true},
		"claude-haiku-4-5":       {200000, 64000, true, true},
		"qwen3.6-plus":           {200000, 64000, true, true},
		"qwen3.5-plus":           {200000, 64000, true, true},
		"deepseek-chat":          {256000, 128000, false, true},
		"deepseek-reasoner":      {256000, 128000, false, true},
		"kimi-k2.5":              {256000, 128000, true, true},
		"glm-5":                  {200000, 128000, false, true},
		"MiniMax-M2.7":           {200000, 128000, false, true},
		"MiniMax-M2.7-highspeed": {200000, 128000, false, true},
		"MiniMax-M2.5":           {200000, 128000, false, true},
	}

	p, found := presets[modelName]
	if !found {
		p = preset{150000, 128000, false, true}
	}

	input := []string{"text"}
	if p.vision {
		input = []string{"text", "image"}
	}

	return ModelSpec{
		ID:            modelName,
		Name:          modelName,
		ContextWindow: p.ctx,
		MaxTokens:     p.max,
		Reasoning:     p.reason,
		Input:         input,
	}
}

// helpers (duplicated from gateway to avoid cross-package dependency)

func toStringSlice(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch arr := v.(type) {
	case []interface{}:
		var result []string
		for _, item := range arr {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return arr
	}
	return nil
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// allModelSpecs returns all known model specs for the openclaw.json models list.
func (g *Generator) allModelSpecs(selectedModel string) []ModelSpec {
	allModels := []string{
		"gpt-5.4", "gpt-5.3-codex", "gpt-5-mini", "gpt-5-nano",
		"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5",
		"qwen3.6-plus", "qwen3.5-plus",
		"deepseek-chat", "deepseek-reasoner",
		"kimi-k2.5", "glm-5",
		"MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M2.5",
	}

	specs := make([]ModelSpec, 0, len(allModels)+1)
	seen := make(map[string]bool)
	for _, name := range allModels {
		specs = append(specs, g.resolveModelSpec(name))
		seen[name] = true
	}
	// Add custom model if not in the built-in list
	if !seen[selectedModel] {
		specs = append(specs, g.resolveModelSpec(selectedModel))
	}
	return specs
}

// allModelAliases returns the agents.defaults.models alias map.
func (g *Generator) allModelAliases(selectedModel string) map[string]interface{} {
	allModels := []string{
		"gpt-5.4", "gpt-5.3-codex", "gpt-5-mini", "gpt-5-nano",
		"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5",
		"qwen3.6-plus", "qwen3.5-plus",
		"deepseek-chat", "deepseek-reasoner",
		"kimi-k2.5", "glm-5",
		"MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M2.5",
	}

	aliases := make(map[string]interface{})
	for _, name := range allModels {
		aliases["agentteams-gateway/"+name] = map[string]interface{}{"alias": name}
	}
	// Add custom model if not in the built-in list
	if _, exists := aliases["agentteams-gateway/"+selectedModel]; !exists {
		aliases["agentteams-gateway/"+selectedModel] = map[string]interface{}{"alias": selectedModel}
	}
	return aliases
}

// InjectHeartbeat reads existing openclaw.json bytes, injects or updates the
// heartbeat configuration under agents.defaults.heartbeat, and returns the
// updated JSON bytes. If enabled is false or every is empty, the heartbeat
// key is removed.
func InjectHeartbeat(existing []byte, enabled bool, every string) []byte {
	var config map[string]interface{}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &config)
	}
	if config == nil {
		config = make(map[string]interface{})
	}
	agents, _ := config["agents"].(map[string]interface{})
	if agents == nil {
		agents = make(map[string]interface{})
		config["agents"] = agents
	}
	defaults, _ := agents["defaults"].(map[string]interface{})
	if defaults == nil {
		defaults = make(map[string]interface{})
		agents["defaults"] = defaults
	}

	if enabled && every != "" {
		defaults["heartbeat"] = map[string]interface{}{"enabled": true, "every": every}
	} else {
		delete(defaults, "heartbeat")
	}

	out, _ := json.MarshalIndent(config, "", "  ")
	return out
}

// InjectChannelPolicy patches channels.matrix.groupAllowFrom and
// channels.matrix.dm.allowFrom in an existing openclaw.json blob with the
// caller-computed final allow-lists. Empty inputs are treated as no-op to
// avoid wiping a valid policy on partial information.
func InjectChannelPolicy(existing []byte, groupAllowFrom, dmAllowFrom []string) []byte {
	groupAllowFrom = uniqueNonEmptyStrings(groupAllowFrom)
	dmAllowFrom = uniqueNonEmptyStrings(dmAllowFrom)
	if len(groupAllowFrom) == 0 || len(dmAllowFrom) == 0 {
		return existing
	}
	var config map[string]interface{}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &config)
	}
	if config == nil {
		config = make(map[string]interface{})
	}
	channels, _ := config["channels"].(map[string]interface{})
	if channels == nil {
		channels = make(map[string]interface{})
		config["channels"] = channels
	}
	matrix, _ := channels["matrix"].(map[string]interface{})
	if matrix == nil {
		matrix = make(map[string]interface{})
		channels["matrix"] = matrix
	}

	matrix["groupAllowFrom"] = stringSliceToInterfaces(groupAllowFrom)

	dm, _ := matrix["dm"].(map[string]interface{})
	if dm == nil {
		dm = make(map[string]interface{})
		matrix["dm"] = dm
	}
	dm["allowFrom"] = stringSliceToInterfaces(dmAllowFrom)

	out, _ := json.MarshalIndent(config, "", "  ")
	return out
}

func stringSliceToInterfaces(values []string) []interface{} {
	out := make([]interface{}, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
