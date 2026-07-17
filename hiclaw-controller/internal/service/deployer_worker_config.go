package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (d *Deployer) DeployWorkerConfig(ctx context.Context, req WorkerDeployRequest) error {
	logger := log.FromContext(ctx)
	agentPrefix := fmt.Sprintf("agents/%s", req.Name)
	localAgentDir := fmt.Sprintf("%s/%s", d.agentFSDir, req.Name)

	if err := d.ensureDirectoryObject(ctx, agentPrefix+"/"); err != nil {
		return fmt.Errorf("create worker storage prefix: %w", err)
	}
	logger.Info("worker storage prefix marker ensured", "worker", req.Name, "key", agentPrefix+"/.agentteams-keep")

	// --- Seed local agent files to storage FIRST (base layer) ---
	// Local/package files provide defaults only. They must not overwrite
	// runtime-mutated OSS state during reconcile; authoritative files are
	// written explicitly below via the overwrite whitelist.
	//
	// Always exclude SOUL.md, AGENTS.md, HEARTBEAT.md from the mirror — each
	// has a dedicated authoritative writer below (PutObject for SOUL.md,
	// prepareAndPushAgentsMD for AGENTS.md, pushBuiltinTopLevelFiles for
	// HEARTBEAT.md). Mirroring them here would race with that writer when
	// reconcile runs more than once: prepareAndPushAgentsMD only updates OSS
	// (not the local file), so a subsequent reconcile's mirror would push the
	// stale local copy back over OSS, transiently exposing wrapped-empty or
	// pre-merge content (the root cause of test-17 flakes).
	// Ensure the local agent directory exists before mirroring
	if err := os.MkdirAll(localAgentDir, 0755); err != nil {
		return fmt.Errorf("create agent dir: %w", err)
	}
	logger.Info("syncing agent files to storage", "name", req.Name)
	seedExcludes := map[string]struct{}{"SOUL.md": {}, "AGENTS.md": {}, "HEARTBEAT.md": {}}
	if err := d.seedLocalAgentFiles(ctx, localAgentDir, agentPrefix, seedExcludes); err != nil {
		logger.Error(err, "agent file sync failed (non-fatal)")
	}

	// --- openclaw.json ---
	var channelPolicy *agentconfig.ChannelPolicy
	if req.Spec.ChannelPolicy != nil {
		channelPolicy = &agentconfig.ChannelPolicy{
			GroupAllowExtra: req.Spec.ChannelPolicy.GroupAllowExtra,
			GroupDenyExtra:  req.Spec.ChannelPolicy.GroupDenyExtra,
			DMAllowExtra:    req.Spec.ChannelPolicy.DmAllowExtra,
			DMDenyExtra:     req.Spec.ChannelPolicy.DmDenyExtra,
		}
	}

	configJSON, err := d.agentConfig.GenerateOpenClawConfig(agentconfig.WorkerConfigRequest{
		WorkerName:     req.Name,
		MatrixToken:    req.MatrixToken,
		GatewayKey:     req.GatewayKey,
		ModelName:      req.Spec.Model,
		AIGatewayURL:   req.AIGatewayURL,
		TeamLeaderName: req.TeamLeaderName,
		ChannelPolicy:  channelPolicy,
		Heartbeat:      req.Heartbeat,
	})
	if err != nil {
		return fmt.Errorf("config generation failed: %w", err)
	}

	// Preserve user-customized plugin entries (e.g. memory-core dreaming
	// schedule) from the existing openclaw.json in storage. This is not
	// limited to IsUpdate: during legacy Team migration, Worker CR status is
	// seeded before WorkerReconciler's first pass, and TeamReconciler may have
	// already written a team-mode channel policy. Requiring IsUpdate would let
	// that first standalone Worker pass clobber the Team overlay.
	if existingJSON, err := d.oss.GetObject(ctx, agentPrefix+"/openclaw.json"); err == nil && len(existingJSON) > 0 {
		if merged, mergeErr := mergeUserPluginConfig(configJSON, existingJSON); mergeErr != nil {
			logger.Error(mergeErr, "plugin config merge failed, using generated config")
		} else {
			configJSON = merged
		}
	}

	openclawKey := agentPrefix + "/openclaw.json"
	if err := d.oss.PutObject(ctx, openclawKey, configJSON); err != nil {
		return fmt.Errorf("config push to storage failed: %w", err)
	}
	logger.Info("worker openclaw.json pushed to storage",
		"worker", req.Name,
		"key", openclawKey,
		"bytes", len(configJSON),
		"role", req.Role,
		"runtime", req.Spec.Runtime,
		"team", req.TeamName,
		"isUpdate", req.IsUpdate,
	)

	// --- SOUL.md (seed-only) ---
	// Written once on first deploy; never overwritten so the agent owns it
	// after startup. Team leaders are handled by renderAndPushSoulTemplate
	// in InjectCoordinationContext, so skip here.
	if req.Role != "team_leader" {
		soulKey := agentPrefix + "/SOUL.md"
		inlineOwnsSoul := req.Spec.Soul != "" || ((strings.EqualFold(req.Spec.Runtime, "copaw") || strings.EqualFold(req.Spec.Runtime, "hermes")) && req.Spec.Identity != "")
		// Try external config ref if no inline soul
		if inlineOwnsSoul {
			soulPath := filepath.Join(localAgentDir, "SOUL.md")
			soulContent, readErr := os.ReadFile(soulPath)
			if readErr != nil {
				if req.Spec.Soul != "" {
					soulContent = []byte(req.Spec.Soul)
				} else {
					logger.Error(readErr, "SOUL.md: inline content unavailable, skipping push", "worker", req.Name)
				}
			}
			if len(soulContent) > 0 {
				if err := d.oss.PutObject(ctx, soulKey, soulContent); err != nil {
					logger.Error(err, "SOUL.md push failed (non-fatal)")
				} else {
					logger.Info("SOUL.md: inline config pushed", "worker", req.Name)
				}
			}
		} else {
			_, err := d.oss.GetObject(ctx, soulKey)
			if err == nil {
				logger.Info("SOUL.md: seed-only, keeping existing version", "worker", req.Name)
			} else if !os.IsNotExist(err) {
				logger.Error(err, "SOUL.md: check existing failed, skipping seed", "worker", req.Name)
			} else {
				soulPath := filepath.Join(localAgentDir, "SOUL.md")
				var soulContent []byte
				if data, err := os.ReadFile(soulPath); err == nil {
					soulContent = data
				} else if !req.IsUpdate {
					soulContent = []byte(fmt.Sprintf("# %s\n\nYou are %s, an AI worker agent.\n", req.Name, req.Name))
				}
				if len(soulContent) > 0 {
					if err := d.oss.PutObject(ctx, soulKey, soulContent); err != nil {
						logger.Error(err, "SOUL.md push failed (non-fatal)")
					}
				}
			}
		}
	}

	// --- config/mcporter.json ---
	if len(req.McpServers) > 0 {
		d.deployWorkerMcporterConfig(ctx, agentPrefix, req.GatewayKey, req.McpServers)
	}

	// --- Matrix password to storage for E2EE re-login ---
	if req.MatrixPassword != "" {
		if err := d.oss.PutObject(ctx, agentPrefix+"/credentials/matrix/password", []byte(req.MatrixPassword)); err != nil {
			logger.Error(err, "failed to write Matrix password to storage (non-fatal)")
		}
	}

	// --- Builtin top-level files (e.g. HEARTBEAT.md for team leaders) ---
	if err := d.pushBuiltinTopLevelFiles(ctx, req.Name, agentPrefix, req.Role, req.Spec.Runtime); err != nil {
		logger.Error(err, "builtin top-level file sync failed (non-fatal)")
	}

	// --- AGENTS.md: merge builtin section + inject coordination context ---
	if err := d.prepareAndPushAgentsMD(ctx, req.Name, agentPrefix, req.Role, req.Spec.Runtime, req.TeamName, req.TeamLeaderName, req.TeamAdminMatrixID, req.TeamCoordinatorIDs, req.Spec.Agents); err != nil {
		logger.Error(err, "AGENTS.md prepare failed (non-fatal)")
	}
	if req.Role == "team_leader" && req.TeamName != "" && req.TeamRoomID != "" {
		teamWorkers := make([]TeamWorkerEntry, 0, len(req.TeamMembers))
		for _, member := range req.TeamMembers {
			if member.Role != "worker" {
				continue
			}
			teamWorkers = append(teamWorkers, TeamWorkerEntry{Name: member.RuntimeName, RoomID: member.PersonalRoomID})
		}
		if err := d.InjectCoordinationContext(ctx, CoordinationDeployRequest{
			LeaderName:         req.Name,
			Role:               req.Role,
			TeamName:           req.TeamName,
			TeamRoomID:         req.TeamRoomID,
			LeaderDMRoomID:     req.LeaderDMRoomID,
			HeartbeatEvery:     heartbeatEvery(req.Heartbeat),
			TeamWorkers:        teamWorkers,
			TeamAdminID:        req.TeamAdminMatrixID,
			TeamCoordinatorIDs: req.TeamCoordinatorIDs,
			LeaderSoul:         req.Spec.Soul,
		}); err != nil {
			logger.Error(err, "leader coordination context inject failed (non-fatal)", "worker", req.Name)
		}
	}

	// --- Push builtin skills from worker-agent template ---
	if err := d.pushBuiltinSkills(ctx, req.Name, agentPrefix, req.Role, req.Spec.Runtime); err != nil {
		logger.Error(err, "builtin skills push failed (non-fatal)")
	}

	return nil
}
func heartbeatEvery(cfg *agentconfig.HeartbeatConfig) string {
	if cfg == nil || !cfg.Enabled {
		return ""
	}
	return cfg.Every
}
func (d *Deployer) deployWorkerMcporterConfig(ctx context.Context, agentPrefix, gatewayKey string, mcpServers []v1beta1.MCPServer) {
	logger := log.FromContext(ctx)
	mcporterJSON, err := d.agentConfig.GenerateMcporterConfig(gatewayKey, mcpServers)
	if err != nil {
		logger.Error(err, "mcporter config generation failed (non-fatal)")
		return
	}
	if mcporterJSON == nil {
		return
	}

	mergedJSON, err := d.mergeExistingWorkerMcporterConfig(ctx, agentPrefix, mcporterJSON)
	if err != nil {
		logger.Error(err, "mcporter config merge failed, using generated config")
		mergedJSON = mcporterJSON
	}

	key := agentPrefix + "/config/mcporter.json"
	if err := d.oss.PutObject(ctx, key, mergedJSON); err != nil {
		logger.Error(err, "mcporter config push failed (non-fatal)", "key", key)
	}
}
func (d *Deployer) mergeExistingWorkerMcporterConfig(ctx context.Context, agentPrefix string, desiredJSON []byte) ([]byte, error) {
	existingJSON, ok := d.readExistingWorkerMcporterConfig(ctx, agentPrefix)
	if !ok {
		return desiredJSON, nil
	}
	return mergeMcporterConfigPreservingExternal(existingJSON, desiredJSON)
}
func (d *Deployer) readExistingWorkerMcporterConfig(ctx context.Context, agentPrefix string) ([]byte, bool) {
	data, err := d.oss.GetObject(ctx, agentPrefix+"/config/mcporter.json")
	if err == nil && len(data) > 0 {
		return data, true
	}
	return nil, false
}

type rawMcporterConfig struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}
type rawMcporterServer struct {
	URL string `json:"url"`
}

func mergeMcporterConfigPreservingExternal(existingJSON, desiredJSON []byte) ([]byte, error) {
	var existing rawMcporterConfig
	if err := json.Unmarshal(existingJSON, &existing); err != nil {
		return nil, err
	}
	var desired rawMcporterConfig
	if err := json.Unmarshal(desiredJSON, &desired); err != nil {
		return nil, err
	}
	if len(desired.MCPServers) == 0 {
		return desiredJSON, nil
	}

	currentGatewayOrigins := mcporterGatewayOrigins(desired.MCPServers)
	merged := rawMcporterConfig{MCPServers: map[string]json.RawMessage{}}
	for name, server := range existing.MCPServers {
		if _, managed := desired.MCPServers[name]; managed {
			continue
		}
		if mcporterServerBelongsToGateway(server, currentGatewayOrigins) {
			continue
		}
		merged.MCPServers[name] = server
	}
	for name, server := range desired.MCPServers {
		merged.MCPServers[name] = server
	}
	return json.MarshalIndent(merged, "", "  ")
}
func mcporterGatewayOrigins(servers map[string]json.RawMessage) map[string]struct{} {
	origins := map[string]struct{}{}
	for _, server := range servers {
		parsed := parseMcporterServerURL(server)
		if parsed == nil || !strings.Contains(parsed.Path, "/mcp-servers/") {
			continue
		}
		origins[mcporterURLOrigin(parsed)] = struct{}{}
	}
	return origins
}
func mcporterServerBelongsToGateway(server json.RawMessage, gatewayOrigins map[string]struct{}) bool {
	if len(gatewayOrigins) == 0 {
		return false
	}
	parsed := parseMcporterServerURL(server)
	if parsed == nil || !strings.Contains(parsed.Path, "/mcp-servers/") {
		return false
	}
	_, ok := gatewayOrigins[mcporterURLOrigin(parsed)]
	return ok
}
func parseMcporterServerURL(server json.RawMessage) *url.URL {
	var decoded rawMcporterServer
	if err := json.Unmarshal(server, &decoded); err != nil {
		return nil
	}
	rawURL := strings.TrimSpace(decoded.URL)
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	return parsed
}
func mcporterURLOrigin(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// InjectCoordinationContext writes team coordination context into the leader's AGENTS.md.
