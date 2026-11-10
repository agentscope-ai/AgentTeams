package agentconfig

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// gatewayProviderID is the openclaw provider-block id this generator emits.
// Upstream provider selection happens inside the gateway (Higress AI API +
// consumer authorization + per-worker IntranetURL baseUrl); the local block id
// is intentionally constant — a "provider/model" prefix that does not resolve
// to a block in models.providers fails openclaw config validation.
const gatewayProviderID = "agentteams-gateway"

// providerFor returns the local openclaw provider-block id. The gateway
// provider named by spec.modelProvider is resolved by the controller for the
// AI route and gateway URL; it is not used as the local block id. Keeping the
// local id constant avoids collisions with runtime built-in provider ids.
func providerFor(_ string) string {
	return gatewayProviderID
}

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
	// Provider projection: the worker's models are qualified with the local
	// gateway provider block. ModelProvider remains the AI-gateway provider
	// name used by the controller to resolve the gateway URL; it is not a
	// local runtime provider id.
	pid := providerFor(req.ModelProvider)

	modelName := req.ModelName
	if modelName == "" {
		modelName = g.config.DefaultModel
	}
	// Strip the effective provider prefix first, then the built-in one, so
	// values still carrying a legacy agentteams-gateway/ prefix keep working.
	modelName = strings.TrimPrefix(modelName, pid+"/")
	modelName = strings.TrimPrefix(modelName, gatewayProviderID+"/")

	// agents.defaults.model block. Primary keeps the historical
	// "provider/model" string.
	modelCfg := map[string]interface{}{
		"primary": pid + "/" + modelName,
	}

	// Provider prefix stripped (effective provider, then the built-in for
	// legacy values): reused for the subagents.model value and to register
	// the subagent model in the provider catalog and alias table below.
	// Empty means "unset" — no subagents.model key, no catalog entry.
	subagentModel := strings.TrimPrefix(req.SubagentModelName, pid+"/")
	subagentModel = strings.TrimPrefix(subagentModel, gatewayProviderID+"/")

	// The subagent model is written to agents.defaults.subagents.model as a
	// single "provider/model" string. OpenClaw's zod schema accepts
	// subagents.model as a string or {primary, fallbacks}; the old
	// agents.defaults.model.subagent dict (QwenPaw shape) is not a valid
	// OpenClaw key and would be rejected by strict config validation.
	subagentsCfg := map[string]interface{}{
		"maxConcurrent": 8,
	}
	if subagentModel != "" {
		subagentsCfg["model"] = pid + "/" + subagentModel
	}

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
	// the Control UI on a separate 18799 listener). AgentTeams's container port
	// mapping (host:AGENTTEAMS_PORT_MANAGER_CONSOLE → container:18799), Dockerfile
	// EXPOSE, install/health probes and the legacy nginx reverse proxy all
	// assume the user-facing console reaches us on 18799. Keep that contract by
	// pinning the gateway port to 18799 — the alternative (rewiring every
	// downstream consumer) is far more invasive.
	//
	// gateway.bind: "lan" — openclaw 2026.4.x defaults to loopback (127.0.0.1)
	// binding for the gateway/Control UI server. In agentteams's embedded dual-
	// container topology the manager runs in its own container and the Control
	// UI is reached via a host port mapping (host:18888 → manager:18799), which
	// requires the listener to be reachable from outside the container's loop-
	// back interface. Bind LAN-wide (0.0.0.0); access remains gated by the
	// shared gateway token.
	//
	// gateway.controlUi device identity — since OpenClaw v2026.10.x the
	// Control UI handshake requires a device identity. Browsers provision one
	// automatically in pure JavaScript (no secure context required, so plain
	// HTTP works). The agentteams console is served through the manager nginx
	// loopback proxy: the host console port maps to nginx 18888 inside the
	// manager container, which proxies to 127.0.0.1:18799. That nginx config
	// deliberately sets no Host or X-Real-IP header, so the gateway treats a
	// host-localhost browser as a loopback direct connection (not a proxied
	// one) and pairing is auto-approved after gateway auth — zero manual
	// step. The same proxy injects the gateway token into the URL hash
	// (#token=...). Any other entry point — the container IP, a LAN address,
	// a custom hostname, or a proxy that adds forwarding headers — needs a
	// one-time `openclaw devices approve <requestId>` (or the Devices UI).
	// Token-only programmatic WS clients are no longer accepted: they must
	// present a device identity like any other client. No legacy flag is
	// carried or needed for this.
	//
	// gateway.controlUi.allowedOrigins: ["*"] — the user picks the console host
	// port at install time and may reach it via 127.0.0.1, the host's LAN IP,
	// or a custom hostname; agentteams cannot enumerate every legitimate origin
	// upfront. Token auth on the gateway remains the actual access boundary.
	// Existing deployments that later tighten this list are preserved: the
	// manager boot merge only fills allowedOrigins when it is absent.
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
	modelAliases := g.allModelAliases(pid, modelName, subagentModel)
	modelPolicy := map[string]interface{}{
		"allow": sortedKeys(modelAliases),
	}

	config := map[string]interface{}{
		"meta": map[string]interface{}{
			"migrations": map[string]interface{}{"modelPolicyAllowlist": true},
		},
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
				"allowedOrigins": []string{"*"},
			},
		},
		"channels": map[string]interface{}{
			"matrix": g.buildMatrixChannelConfig(req, matrixServerURL, matrixDomain, adminMatrixID),
		},
		"models": map[string]interface{}{
			"mode": "merge",
			"providers": map[string]interface{}{
				pid: map[string]interface{}{
					"baseUrl": aiGatewayURL + "/v1",
					"apiKey":  req.GatewayKey,
					"api":     "openai-completions",
					"models":  g.allModelSpecs(modelName, subagentModel),
				},
			},
		},
		// Matrix messages sent by agt use plain @name mentions. The SDK
		// cannot derive these from the Matrix user ID without a pattern.
		"messages": map[string]interface{}{
			"groupChat": map[string]interface{}{
				"mentionPatterns": []string{`(^|[^A-Za-z0-9_@-])@` + regexp.QuoteMeta(req.WorkerName) + `($|[^A-Za-z0-9_-])`},
			},
		},
		"agents": map[string]interface{}{
			"defaults": map[string]interface{}{
				"timeoutSeconds":  1800,
				"workspace":       "~",
				"model":           modelCfg,
				"models":          modelAliases,
				"modelPolicy":     modelPolicy,
				"maxConcurrent":   4,
				"subagents":       subagentsCfg,
				"elevatedDefault": "full",
			},
		},
		"tools": map[string]interface{}{
			"exec": map[string]interface{}{
				"host":     "gateway",
				"security": "full",
				"ask":      "off",
			},
			"elevated": map[string]interface{}{
				"enabled": true,
				"allowFrom": map[string]interface{}{
					"matrix": elevatedAllowFrom(req, matrixDomain, adminMatrixID),
				},
			},
		},
		"session": map[string]interface{}{
			"resetByType": map[string]interface{}{
				"direct": map[string]interface{}{"mode": "daily", "atHour": 4},
				"group":  map[string]interface{}{"mode": "daily", "atHour": 4},
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

	// Add supports_multimodal based on the selected model's capabilities.
	// The OpenClaw runtime strictly validates agents.defaults and does not
	// recognize these keys — only emit them for runtimes that consume them.
	modelSpec := g.resolveModelSpec(modelName)
	if req.Runtime != "openclaw" && len(modelSpec.Input) > 0 {
		supportsImage := false
		for _, inp := range modelSpec.Input {
			if inp == "image" {
				supportsImage = true
				break
			}
		}
		if supportsImage {
			agents := config["agents"].(map[string]interface{})
			defaults := agents["defaults"].(map[string]interface{})
			defaults["supports_multimodal"] = true
			defaults["supports_image"] = true
		}
	}

	// Add embedding model for memory search if configured.  OpenClaw
	// v2026.10.6 moved the legacy agents.defaults.memorySearch key to the
	// top-level memory.search object.
	if g.config.EmbeddingModel != "" {
		config["memory"] = map[string]interface{}{
			"search": map[string]interface{}{
				"provider": "openai",
				"model":    g.config.EmbeddingModel,
				"remote": map[string]interface{}{
					"baseUrl": aiGatewayURL + "/v1",
					"apiKey":  req.GatewayKey,
				},
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

	groups := map[string]interface{}{
		"*": map[string]interface{}{"requireMention": true},
	}
	if req.PersonalRoomID != "" {
		groups[req.PersonalRoomID] = map[string]interface{}{"requireMention": false}
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
		"groups":         groups,
		// Matrix v2026.10.6: nested streaming contract; editable preview while
		// the LLM streams, one message per assistant block.
		"streaming": map[string]interface{}{
			"mode":  "partial",
			"block": map[string]interface{}{"enabled": true},
		},
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
		// forever instead of becoming `join`. AgentTeams's provisioning flow always
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

// elevatedAllowFrom returns the Matrix senders allowed to trigger elevated
// exec. Mirror the worker template: Manager + Admin for standalone workers,
// Leader + Admin for team workers. Never use ["*"] — OpenClaw 2026.4.x
// allowFrom matching is too broad (display-name / case-fold collisions).
func elevatedAllowFrom(req WorkerConfigRequest, domain, adminMatrixID string) []string {
	if req.TeamLeaderName != "" {
		return []string{
			fmt.Sprintf("@%s:%s", req.TeamLeaderName, domain),
			adminMatrixID,
		}
	}
	return []string{
		fmt.Sprintf("@manager:%s", domain),
		adminMatrixID,
	}
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

// ResolveModelInput returns the input modalities (e.g. ["text", "image"]) for
// a model, applying the same config overrides as resolveModelSpec. It is the
// exported capability view used by the runtime-config projector so that
// managed runtimes (QwenPaw worker) receive model capabilities without having
// to re-derive them from the model name.
func (g *Generator) ResolveModelInput(modelName string) []string {
	if g == nil {
		return nil
	}
	return g.resolveModelSpec(modelName).Input
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
func (g *Generator) allModelSpecs(selectedModel, subagentModel string) []ModelSpec {
	allModels := []string{
		"gpt-5.4", "gpt-5.3-codex", "gpt-5-mini", "gpt-5-nano",
		"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5",
		"qwen3.6-plus", "qwen3.5-plus",
		"deepseek-chat", "deepseek-reasoner",
		"kimi-k2.5", "glm-5",
		"MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M2.5",
	}

	specs := make([]ModelSpec, 0, len(allModels)+2)
	seen := make(map[string]bool)
	for _, name := range allModels {
		specs = append(specs, g.resolveModelSpec(name))
		seen[name] = true
	}
	// Add custom model if not in the built-in list
	if !seen[selectedModel] {
		specs = append(specs, g.resolveModelSpec(selectedModel))
		seen[selectedModel] = true
	}
	// Register the subagent model too, so agents.defaults.subagents.model never
	// references a model the provider catalog does not declare.
	if subagentModel != "" && !seen[subagentModel] {
		specs = append(specs, g.resolveModelSpec(subagentModel))
	}
	return specs
}

// allModelAliases returns the agents.defaults.models alias map. Keys are
// qualified with pid — the provider serving this worker's models — so the
// alias table and the derived modelPolicy allowlist always resolve against
// the declared models.providers block.
func (g *Generator) allModelAliases(pid, selectedModel, subagentModel string) map[string]interface{} {
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
		aliases[pid+"/"+name] = map[string]interface{}{"alias": name}
	}
	// Add custom model if not in the built-in list
	if _, exists := aliases[pid+"/"+selectedModel]; !exists {
		aliases[pid+"/"+selectedModel] = map[string]interface{}{"alias": selectedModel}
	}
	// Alias the subagent model as well so it is selectable; the map lookup
	// dedupes it against the entries above.
	if subagentModel != "" {
		subagentKey := pid + "/" + subagentModel
		if _, exists := aliases[subagentKey]; !exists {
			aliases[subagentKey] = map[string]interface{}{"alias": subagentModel}
		}
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

func sortedKeys(values map[string]interface{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
