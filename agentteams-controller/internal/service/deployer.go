package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credprovider"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/executor"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// --- Request types ---

// WorkerDeployRequest describes a worker config deployment (create or update).
type WorkerDeployRequest struct {
	Name           string
	Spec           v1beta1.WorkerSpec
	Role           string // "standalone" | "team_leader" | "worker"
	TeamName       string
	TeamLeaderName string
	TeamRoomID     string
	LeaderDMRoomID string
	TeamMembers    []RuntimeConfigTeamMember

	// From provisioning
	MatrixToken    string
	GatewayKey     string
	MatrixPassword string

	// AIGatewayURL overrides the cluster-wide AI Gateway URL when modelProvider is set.
	AIGatewayURL string

	// MCP servers declared in spec.mcpServers. The deployer translates this into
	// config/mcporter.json and injects Authorization: Bearer <GatewayKey>.
	McpServers []v1beta1.MCPServer

	TeamAdminMatrixID  string
	TeamCoordinatorIDs []string

	// Heartbeat config from Team CR leader spec (nil for non-leader workers)
	Heartbeat *agentconfig.HeartbeatConfig

	IsUpdate bool
}

type WorkerDepsPrepareRequest struct {
	WorkerName   string
	TokenSubPath string
	EnvSubPath   string
	DataSubPath  string
	Storage      oss.StorageClient
	Token        string
	Env          map[string]string
	UseToken     bool
	UseEnv       bool
}

// RuntimeProjectionConfig holds non-secret cluster facts projected into
// runtime.yaml for managed runtimes such as QwenPaw.
type RuntimeProjectionConfig struct {
	StorageProvider           string
	StorageBucket             string
	StorageEndpoint           string
	AIGatewayURL              string
	AgentIdentityDataEndpoint string
}

// MemberRuntimeConfigDeployRequest describes one runtime.yaml projection.
// RuntimeName, not Name, is the object-storage member key.
type MemberRuntimeConfigDeployRequest struct {
	Name        string
	RuntimeName string
	Runtime     string
	Role        string
	Generation  int64
	Spec        v1beta1.WorkerSpec

	MatrixUserID   string
	PersonalRoomID string
	// MatrixAccessToken is projected only for remote-managed local runtimes.
	MatrixAccessToken string

	// GatewayKey is the worker's gateway API key projected only for
	// remote-managed local runtimes.
	GatewayKey string
	// AIGatewayURL overrides the cluster-wide AI Gateway URL when modelProvider is set.
	AIGatewayURL string
	// Skill registry facts are projected for runtime-local skill discovery.
	SkillRegistryURL      string
	SkillRegistryAuthType string

	TeamName          string
	TeamRoomID        string
	LeaderName        string
	LeaderRuntimeName string
	LeaderDMRoomID    string
	TeamAdminName     string
	TeamAdminMatrixID string
	TeamMembers       []RuntimeConfigTeamMember

	// DropTeamContext forces a standalone runtime.yaml even when an older
	// team-scoped runtime.yaml exists for the same runtime name.
	DropTeamContext bool
}

// RuntimeConfigTeamMember is a non-secret roster entry projected into
// runtime.yaml for worker runtimes that need live team facts.
type RuntimeConfigTeamMember struct {
	Name           string `json:"name,omitempty"`
	RuntimeName    string `json:"runtimeName,omitempty"`
	Role           string `json:"role,omitempty"`
	MatrixUserID   string `json:"matrixUserId,omitempty"`
	PersonalRoomID string `json:"personalRoomId,omitempty"`
}

// CoordinationDeployRequest describes coordination context injection for a team leader.
type CoordinationDeployRequest struct {
	LeaderName         string
	Role               string
	TeamName           string
	TeamRoomID         string
	LeaderDMRoomID     string
	HeartbeatEvery     string
	WorkerIdleTimeout  string
	TeamWorkers        []TeamWorkerEntry
	TeamAdminID        string
	TeamCoordinatorIDs []string
	LeaderSoul         string // from CR spec.leader.soul; used as seed if non-empty
}

// TeamWorkerEntry carries worker name + room ID for coordination context rendering.
type TeamWorkerEntry struct {
	Name   string
	RoomID string
}

// WorkerCoordinationRequest describes coordination context injection for a team member worker.
type WorkerCoordinationRequest struct {
	WorkerName         string
	TeamName           string
	TeamLeaderName     string
	TeamAdminID        string
	TeamCoordinatorIDs []string
}

// InjectHeartbeatRequest describes heartbeat config injection into a leader's openclaw.json.
type InjectHeartbeatRequest struct {
	WorkerName string
	Enabled    bool
	Every      string // e.g. "30m"
}

// InjectChannelPolicyRequest describes a channel-policy override applied to a
// member worker's openclaw.json. Used by TeamReconciler in the decoupled path
// to switch a Worker's Matrix allow-list from [manager, admin] (standalone
// default produced by WorkerReconciler) to the role-aware Team allow-list.
// Reset back to manager-mode on team deletion.
type InjectChannelPolicyRequest struct {
	WorkerName     string
	GroupAllowFrom []string
	DMAllowFrom    []string
}

// SyncTeamLeaderAssetsRequest describes the role-specific, non-credential
// assets that must be overlaid when a standalone Worker is attached as a Team
// Leader in the decoupled Team path.
type SyncTeamLeaderAssetsRequest struct {
	WorkerName string
	Runtime    string
}

// --- Deployer ---

// DeployerConfig holds configuration for constructing a Deployer.
type DeployerConfig struct {
	AgentConfig    *agentconfig.Generator
	OSS            oss.StorageClient
	Executor       *executor.Shell
	Packages       *executor.PackageResolver
	Legacy         *LegacyCompat
	AgentFSDir     string // embedded: /root/agentteams-fs/agents
	WorkerAgentDir string // source for builtin agent files
	MatrixDomain   string

	RuntimeProjection RuntimeProjectionConfig

	// NacosCredClient is used when remoteSkills use sts-hiclaw (see CRD authType).
	NacosCredClient credprovider.Client
}

// Deployer orchestrates configuration deployment for workers: package resolution,
// inline config writes, openclaw.json generation, AGENTS.md merging, skill pushing,
// and OSS synchronization.
type Deployer struct {
	agentConfig       *agentconfig.Generator
	oss               oss.StorageClient
	executor          *executor.Shell
	packages          *executor.PackageResolver
	legacy            *LegacyCompat
	agentFSDir        string
	workerAgentDir    string
	matrixDomain      string
	runtimeProjection RuntimeProjectionConfig
	nacosCredClient   credprovider.Client
}

func NewDeployer(cfg DeployerConfig) *Deployer {
	return &Deployer{
		agentConfig:       cfg.AgentConfig,
		oss:               cfg.OSS,
		executor:          cfg.Executor,
		packages:          cfg.Packages,
		legacy:            cfg.Legacy,
		agentFSDir:        cfg.AgentFSDir,
		workerAgentDir:    cfg.WorkerAgentDir,
		matrixDomain:      cfg.MatrixDomain,
		runtimeProjection: cfg.RuntimeProjection,
		nacosCredClient:   cfg.NacosCredClient,
	}
}

// DeployPackage resolves, downloads, extracts, and deploys a package to OSS.
// No-op if uri is empty.
func (d *Deployer) DeployPackage(ctx context.Context, name, uri string, isUpdate bool) error {
	if uri == "" {
		return nil
	}
	logger := log.FromContext(ctx)
	safeURI := redactPackageURI(uri)
	logger.Info("package parameter detected; starting package deploy", "name", name, "package", safeURI, "isUpdate", isUpdate)
	if d.packages == nil {
		logger.Info("package resolver unavailable; skipping package deploy", "name", name, "package", safeURI)
		return nil
	}

	extractedDir, err := d.packages.ResolveAndExtract(ctx, uri, name)
	if err != nil {
		return fmt.Errorf("package resolve/extract failed: %w", err)
	}
	if extractedDir == "" {
		logger.Info("package resolve/extract returned empty result; skipping package deploy", "name", name, "package", safeURI, "isUpdate", isUpdate)
		return nil
	}
	logger.Info("package resolved and extracted", "name", name, "package", safeURI, "extractedDir", extractedDir, "isUpdate", isUpdate)

	if err := d.packages.DeployToMinIO(ctx, extractedDir, name, isUpdate, d.oss); err != nil {
		return fmt.Errorf("package deploy failed: %w", err)
	}
	logger.Info("package deploy completed", "name", name, "package", safeURI, "isUpdate", isUpdate)

	return nil
}

// WriteInlineConfigs writes inline identity/soul/agents content to the local agent directory.
// No-op if all inline fields are empty.
func (d *Deployer) WriteInlineConfigs(name string, spec v1beta1.WorkerSpec) error {
	if spec.Identity == "" && spec.Soul == "" && spec.Agents == "" {
		return nil
	}
	agentDir := fmt.Sprintf("%s/%s", d.agentFSDir, name)
	if err := executor.WriteInlineConfigs(agentDir, spec.Runtime, spec.Identity, spec.Soul, spec.Agents); err != nil {
		return err
	}
	log.Log.Info("inline configs written", "name", name)
	return nil
}

// DeployWorkerConfig generates and pushes all configuration files to OSS:
// openclaw.json, SOUL.md, mcporter config, Matrix password, agent file sync,
// AGENTS.md merge with builtin section + coordination context, builtin skills.
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
func (d *Deployer) InjectCoordinationContext(ctx context.Context, req CoordinationDeployRequest) error {
	leaderAgentPrefix := fmt.Sprintf("agents/%s", req.LeaderName)

	teamWorkers := make([]agentconfig.TeamWorkerInfo, 0, len(req.TeamWorkers))
	for _, tw := range req.TeamWorkers {
		teamWorkers = append(teamWorkers, agentconfig.TeamWorkerInfo{Name: tw.Name, RoomID: tw.RoomID})
	}

	coordCtx := agentconfig.CoordinationContext{
		WorkerName:         req.LeaderName,
		Role:               req.Role,
		MatrixDomain:       d.matrixDomain,
		TeamName:           req.TeamName,
		TeamRoomID:         req.TeamRoomID,
		LeaderDMRoomID:     req.LeaderDMRoomID,
		HeartbeatEvery:     req.HeartbeatEvery,
		WorkerIdleTimeout:  req.WorkerIdleTimeout,
		TeamWorkers:        teamWorkers,
		TeamAdminID:        req.TeamAdminID,
		TeamCoordinatorIDs: req.TeamCoordinatorIDs,
	}

	existing, _ := d.oss.GetObject(ctx, leaderAgentPrefix+"/AGENTS.md")
	injected := agentconfig.InjectCoordinationContext(string(existing), coordCtx)
	if err := d.oss.PutObject(ctx, leaderAgentPrefix+"/AGENTS.md", []byte(injected)); err != nil {
		return err
	}

	// --- Render SOUL.md from template ---
	// Team leader uses SOUL.md.tmpl with ${VAR} placeholders; render and push.
	if err := d.renderAndPushSoulTemplate(ctx, leaderAgentPrefix, req); err != nil {
		log.FromContext(ctx).Error(err, "SOUL.md template rendering failed (non-fatal)")
	}
	return nil
}

// renderAndPushSoulTemplate merges the team leader's SOUL.md template into OSS.
// The rendered template is wrapped in markers; existing content (from package or
// prior runs) is preserved outside the markers. Priority: CR spec.leader.soul > template.
func (d *Deployer) renderAndPushSoulTemplate(ctx context.Context, agentPrefix string, req CoordinationDeployRequest) error {
	soulKey := agentPrefix + "/SOUL.md"

	if req.LeaderSoul != "" {
		return d.oss.PutObject(ctx, soulKey, []byte(req.LeaderSoul))
	}

	tmplPath := filepath.Join(d.builtinAgentDir("team_leader", ""), "SOUL.md.tmpl")
	tmplData, err := os.ReadFile(tmplPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read SOUL.md.tmpl: %w", err)
	}

	workerNames := make([]string, 0, len(req.TeamWorkers))
	for _, tw := range req.TeamWorkers {
		workerNames = append(workerNames, tw.Name)
	}

	rendered := string(tmplData)
	rendered = strings.ReplaceAll(rendered, "${TEAM_LEADER_NAME}", req.LeaderName)
	rendered = strings.ReplaceAll(rendered, "${TEAM_NAME}", req.TeamName)
	rendered = strings.ReplaceAll(rendered, "${TEAM_WORKERS}", strings.Join(workerNames, ", "))

	existing, err := d.oss.GetObject(ctx, soulKey)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("SOUL.md read existing failed: %w", err)
	}
	merged := agentconfig.MergeSoulTemplate(string(existing), rendered)

	return d.oss.PutObject(ctx, soulKey, []byte(merged))
}

// InjectWorkerCoordination writes team coordination context into a team member
// worker's AGENTS.md. This is the worker-side counterpart to
// InjectCoordinationContext, which targets the leader.
func (d *Deployer) InjectWorkerCoordination(ctx context.Context, req WorkerCoordinationRequest) error {
	agentPrefix := fmt.Sprintf("agents/%s", req.WorkerName)
	existing, _ := d.oss.GetObject(ctx, agentPrefix+"/AGENTS.md")
	coordCtx := agentconfig.CoordinationContext{
		WorkerName:         req.WorkerName,
		Role:               "worker",
		MatrixDomain:       d.matrixDomain,
		TeamName:           req.TeamName,
		TeamLeaderName:     req.TeamLeaderName,
		TeamAdminID:        req.TeamAdminID,
		TeamCoordinatorIDs: req.TeamCoordinatorIDs,
	}
	injected := agentconfig.InjectCoordinationContext(string(existing), coordCtx)
	return d.oss.PutObject(ctx, agentPrefix+"/AGENTS.md", []byte(injected))
}

// InjectHeartbeatConfig reads the leader's existing openclaw.json from OSS,
// injects or updates the heartbeat configuration, and writes it back.
func (d *Deployer) InjectHeartbeatConfig(ctx context.Context, req InjectHeartbeatRequest) error {
	agentPrefix := fmt.Sprintf("agents/%s", req.WorkerName)
	existing, _ := d.oss.GetObject(ctx, agentPrefix+"/openclaw.json")
	updated := agentconfig.InjectHeartbeat(existing, req.Enabled, req.Every)
	return d.oss.PutObject(ctx, agentPrefix+"/openclaw.json", updated)
}

// InjectChannelPolicy reads a member worker's existing openclaw.json from OSS,
// patches channels.matrix.groupAllowFrom and channels.matrix.dm.allowFrom to
// the caller-computed final allow-lists, and writes it back. WorkerReconciler
// regenerates openclaw.json with standalone semantics; when a Worker is
// referenced into a Team via spec.workerMembers, TeamReconciler calls this to
// apply the role-aware Team policy. On Team deletion, the caller resets the
// lists to standalone manager/admin semantics.
func (d *Deployer) InjectChannelPolicy(ctx context.Context, req InjectChannelPolicyRequest) error {
	if req.WorkerName == "" || len(req.GroupAllowFrom) == 0 || len(req.DMAllowFrom) == 0 {
		return nil
	}
	agentPrefix := fmt.Sprintf("agents/%s", req.WorkerName)
	existing, _ := d.oss.GetObject(ctx, agentPrefix+"/openclaw.json")
	updated := agentconfig.InjectChannelPolicy(existing, req.GroupAllowFrom, req.DMAllowFrom)
	return d.oss.PutObject(ctx, agentPrefix+"/openclaw.json", updated)
}

// SyncTeamLeaderAssets overlays the Team Leader built-in AGENTS.md section,
// built-in skills, and seed-only top-level files onto an already-provisioned
// Worker. It intentionally does not rewrite openclaw.json or credentials:
// decoupled Teams do not own Worker lifecycle/config wholesale.
func (d *Deployer) SyncTeamLeaderAssets(ctx context.Context, req SyncTeamLeaderAssetsRequest) error {
	if req.WorkerName == "" {
		return nil
	}
	agentPrefix := fmt.Sprintf("agents/%s", req.WorkerName)
	role := "team_leader"
	if err := d.prepareAndPushAgentsMD(ctx, req.WorkerName, agentPrefix, role, req.Runtime, "", "", "", nil, ""); err != nil {
		return err
	}
	if err := d.pushBuiltinSkills(ctx, req.WorkerName, agentPrefix, role, req.Runtime); err != nil {
		return err
	}
	if err := d.pushBuiltinTopLevelFiles(ctx, req.WorkerName, agentPrefix, role, req.Runtime); err != nil {
		return err
	}
	return nil
}

// PushOnDemandSkills pushes on-demand skills to a worker.
// Built-in skills are pushed via push-worker-skills.sh. Remote skills are
// fetched from source registries (currently nacos://) and mirrored to OSS.
func (d *Deployer) PushOnDemandSkills(ctx context.Context, workerName string, skills []string, remoteSkills []v1beta1.RemoteSkillSource) error {
	logger := log.FromContext(ctx)
	if len(skills) == 0 && len(remoteSkills) == 0 {
		return nil
	}

	agentPrefix := fmt.Sprintf("agents/%s", workerName)
	if err := d.pushRemoteSkills(ctx, workerName, agentPrefix, remoteSkills); err != nil {
		return err
	}

	if len(skills) == 0 || d.executor == nil {
		return nil
	}
	scriptPath := "/opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh"
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		logger.Info("push-worker-skills.sh not found (incluster mode), skipping on-demand skill push",
			"worker", workerName, "skills", skills)
		return nil
	}
	_, err := d.executor.RunSimple(ctx, scriptPath, "--worker", workerName, "--no-notify")
	return err
}

func (d *Deployer) PrepareWorkerDeps(ctx context.Context, req WorkerDepsPrepareRequest) error {
	if req.WorkerName == "" {
		return fmt.Errorf("worker deps: workerName is required")
	}
	if req.TokenSubPath == "" && req.EnvSubPath == "" && req.DataSubPath == "" {
		return fmt.Errorf("worker deps: at least one subPath is required")
	}
	store := req.Storage
	if store == nil {
		store = d.oss
	}
	if store == nil {
		return fmt.Errorf("worker deps: object storage client is required")
	}
	objects := map[string][]byte{}
	if req.DataSubPath != "" {
		objects[workerDepsObjectKey(req.DataSubPath, ".agentteams-keep")] = nil
	}
	if req.UseToken {
		if req.TokenSubPath == "" {
			return fmt.Errorf("worker deps: tokenSubPath is required")
		}
		if req.Token == "" {
			return fmt.Errorf("worker deps: token is required")
		}
		objects[workerDepsObjectKey(req.TokenSubPath, "token")] = []byte(req.Token)
	}
	if req.UseEnv {
		if req.EnvSubPath == "" {
			return fmt.Errorf("worker deps: envSubPath is required")
		}
		objects[workerDepsObjectKey(req.EnvSubPath, "env")] = []byte(workerDepsEnvFile(req.Env))
	}
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := store.PutObject(ctx, key, objects[key]); err != nil {
			return fmt.Errorf("write worker deps %s: %w", key, err)
		}
	}
	for _, key := range keys {
		if err := store.Stat(ctx, key); err != nil {
			return fmt.Errorf("verify worker deps %s: %w", key, err)
		}
	}
	return nil
}

func workerDepsObjectKey(subPath, fileName string) string {
	return strings.Trim(subPath, "/") + "/" + fileName
}

func workerDepsEnvFile(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		if validEnvKey(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellSingleQuote(env[key]))
		b.WriteByte('\n')
	}
	return b.String()
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	first := key[0]
	if !((first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z') || first == '_') {
		return false
	}
	for i := 1; i < len(key); i++ {
		ch := key[i]
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' {
			continue
		}
		return false
	}
	return true
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (d *Deployer) seedLocalAgentFiles(ctx context.Context, localAgentDir, agentPrefix string, excludedTopLevel map[string]struct{}) error {
	info, err := os.Stat(localAgentDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}

	logger := log.FromContext(ctx)
	var seeded []string
	err = filepath.WalkDir(localAgentDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}

		rel, err := filepath.Rel(localAgentDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, excluded := excludedTopLevel[rel]; excluded {
			return nil
		}

		key := agentPrefix + "/" + rel
		if _, err := d.oss.GetObject(ctx, key); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := d.oss.PutFile(ctx, path, key); err != nil {
			return err
		}
		seeded = append(seeded, rel)
		return nil
	})
	if err != nil {
		return err
	}
	if len(seeded) > 0 {
		logger.Info("agent seed-only files pushed to storage", "target", agentPrefix, "files", seeded)
	}
	return nil
}

type nacosClientKey struct {
	nacosAddr string
	namespace string
	authType  string
	resources string
}

func (d *Deployer) pushRemoteSkills(ctx context.Context, workerName, agentPrefix string, remoteSkills []v1beta1.RemoteSkillSource) error {
	if len(remoteSkills) == 0 {
		return nil
	}

	logger := log.FromContext(ctx)
	logger.Info("pushing remote skills", "worker", workerName, "sources", len(remoteSkills))
	clients := map[nacosClientKey]*executor.NacosAIClient{}

	for _, source := range remoteSkills {
		if len(source.Skills) == 0 {
			return fmt.Errorf("remoteSkills source %q has empty skills list", source.Source)
		}
		for _, skill := range source.Skills {
			if strings.TrimSpace(skill.Name) == "" {
				return fmt.Errorf("remoteSkills source %q has an entry with empty name", source.Source)
			}
			if skill.Version != "" && skill.Label != "" {
				return fmt.Errorf("remote skill %q in source %q cannot set both version and label", skill.Name, source.Source)
			}
		}

		nacosAddr, namespace, err := parseNacosRemoteSource(source.Source)
		if err != nil {
			return fmt.Errorf("invalid remoteSkills.source %q: %w", source.Source, err)
		}

		authType, err := mapRemoteSkillAuthType(source.AuthType)
		if err != nil {
			return fmt.Errorf("invalid remoteSkills.authType for source %q: %w", source.Source, err)
		}

		stsResources := remoteSkillSTSResources(source.Skills)
		key := nacosClientKey{nacosAddr: nacosAddr, namespace: namespace, authType: authType}
		var opts []executor.NacosAIClientOption
		if authType == "sts-hiclaw" {
			key.resources = strings.Join(stsResources, ",")
			opts = append(opts, executor.WithNacosSTSResources(stsResources))
		}
		client, ok := clients[key]
		if !ok {
			logger.Info("connecting to nacos", "worker", workerName, "source", source.Source, "authType", authType)
			client, err = executor.NewNacosAIClient(ctx, nacosAddr, namespace, authType, d.nacosCredClient, opts...)
			if err != nil {
				return fmt.Errorf("connect to nacos source %q: %w", source.Source, err)
			}
			clients[key] = client
		}

		for _, skill := range source.Skills {
			tmpDir, err := os.MkdirTemp("", "nacos-skill-")
			if err != nil {
				return fmt.Errorf("create temp dir for skill %q: %w", skill.Name, err)
			}
			defer os.RemoveAll(tmpDir)

			if err := client.GetSkill(ctx, skill.Name, tmpDir, skill.Version, skill.Label); err != nil {
				return fmt.Errorf("fetch remote skill %q from %q: %w", skill.Name, source.Source, err)
			}
			logger.Info("remote skill fetched, mirroring to OSS",
				"worker", workerName,
				"source", source.Source,
				"skill", skill.Name,
				"version", skill.Version,
				"label", skill.Label)

			src := filepath.Join(tmpDir, skill.Name) + "/"
			dst := agentPrefix + "/skills/" + skill.Name + "/"
			if err := d.oss.Mirror(ctx, src, dst, oss.MirrorOptions{Overwrite: true}); err != nil {
				return fmt.Errorf("mirror remote skill %q from %q to OSS: %w", skill.Name, source.Source, err)
			}
			logger.Info("remote skill pushed",
				"worker", workerName,
				"source", source.Source,
				"skill", skill.Name,
				"version", skill.Version,
				"label", skill.Label)
		}
	}

	return nil
}

func mapRemoteSkillAuthType(raw string) (string, error) {
	authType := strings.TrimSpace(raw)
	switch authType {
	case "", "sts-hiclaw", "nacos", "none":
		return authType, nil
	default:
		return "", fmt.Errorf("unsupported authType %q", raw)
	}
}

func remoteSkillSTSResources(skills []v1beta1.RemoteSkill) []string {
	seen := make(map[string]struct{}, len(skills))
	for _, skill := range skills {
		name := strings.TrimSpace(skill.Name)
		if name == "" {
			continue
		}
		seen["skill/"+name] = struct{}{}
	}
	resources := make([]string, 0, len(seen))
	for res := range seen {
		resources = append(resources, res)
	}
	sort.Strings(resources)
	return resources
}

func parseNacosRemoteSource(raw string) (nacosAddr, namespace string, err error) {
	if !strings.HasPrefix(raw, "nacos://") {
		return "", "", fmt.Errorf("source must use nacos:// scheme")
	}

	parsed, err := url.Parse("http://" + strings.TrimPrefix(raw, "nacos://"))
	if err != nil {
		return "", "", err
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("missing host")
	}

	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 1 || parts[0] == "" {
		return "", "", fmt.Errorf("expected nacos://host:port/{namespace-id}")
	}

	nacosAddr = parsed.Host
	if parsed.User != nil {
		nacosAddr = parsed.User.String() + "@" + parsed.Host
	}
	return nacosAddr, parts[0], nil
}

// CleanupOSSData removes all agent data from OSS for a deleted worker.
// CleanLegacyPasswordFiles removes credentials/matrix/password from OSS for
// all listed agents. Called when switching from legacy password mode to
// AppService mode to prevent stale password files from lingering.
func (d *Deployer) CleanLegacyPasswordFiles(ctx context.Context, names []string) error {
	logger := log.FromContext(ctx).WithName("password-cleanup")
	for _, name := range names {
		key := fmt.Sprintf("agents/%s/credentials/matrix/password", name)
		if err := d.oss.DeleteObject(ctx, key); err != nil {
			logger.Error(err, "failed to delete legacy password file (non-fatal)", "name", name)
		}
	}
	return nil
}

func (d *Deployer) CleanupOSSData(ctx context.Context, workerName string) error {
	agentPrefix := fmt.Sprintf("agents/%s/", workerName)
	return d.oss.DeletePrefix(ctx, agentPrefix)
}

// EnsureTeamStorage creates the shared storage directories for a team.
func (d *Deployer) EnsureTeamStorage(ctx context.Context, teamName string) error {
	prefix := fmt.Sprintf("teams/%s/", teamName)
	if err := d.ensureDirectoryObject(ctx, prefix); err != nil {
		return fmt.Errorf("create %s: %w", prefix, err)
	}
	if err := d.ensureDirectoryObject(ctx, prefix+"shared/"); err != nil {
		return fmt.Errorf("create %sshared/: %w", prefix, err)
	}
	for _, subdir := range []string{"shared/tasks/", "shared/projects/", "shared/knowledge/"} {
		if err := d.oss.PutObject(ctx, prefix+subdir+".keep", []byte("")); err != nil {
			return fmt.Errorf("create %s%s: %w", prefix, subdir, err)
		}
	}
	return nil
}

func (d *Deployer) ensureDirectoryObject(ctx context.Context, key string) error {
	if key == "" || !strings.HasSuffix(key, "/") {
		return fmt.Errorf("directory object key must end with /: %q", key)
	}
	return d.oss.PutObject(ctx, key+".agentteams-keep", []byte(""))
}

// --- Manager Config Deployment ---

// ManagerDeployRequest describes a Manager config deployment (create or update).
type ManagerDeployRequest struct {
	Name           string
	Spec           v1beta1.ManagerSpec
	MatrixToken    string
	GatewayKey     string
	MatrixPassword string

	// MCP servers declared in spec.mcpServers. The deployer translates this into
	// mcporter-servers.json and injects Authorization: Bearer <GatewayKey>.
	McpServers []v1beta1.MCPServer

	// AIGatewayURL overrides the cluster-wide AI Gateway URL when modelProvider is set.
	AIGatewayURL string

	IsUpdate bool
}

// DeployManagerConfig generates and pushes Manager configuration files to OSS.
// Unlike Worker, AGENTS.md and builtin skills are managed by the Manager container
// itself (via upgrade-builtins.sh), so we only push runtime-generated files.
func (d *Deployer) DeployManagerConfig(ctx context.Context, req ManagerDeployRequest) error {
	logger := log.FromContext(ctx)
	agentPrefix := fmt.Sprintf("agents/%s", req.Name)

	// --- openclaw.json ---
	// Manager's Matrix username is always "manager" regardless of the Manager
	// CR name (which is typically "default"). Without this override the
	// generated openclaw.json ends up with userId=@<crName>:<domain>, the
	// Matrix client filters all DMs to that wrong localpart, and the agent
	// silently never sees admin messages. See commit 3f8f84b which fixed this
	// originally before the controller refactor accidentally reverted it.
	configJSON, err := d.agentConfig.GenerateOpenClawConfig(agentconfig.WorkerConfigRequest{
		WorkerName:   "manager",
		MatrixToken:  req.MatrixToken,
		GatewayKey:   req.GatewayKey,
		ModelName:    req.Spec.Model,
		AIGatewayURL: req.AIGatewayURL,
	})
	if err != nil {
		return fmt.Errorf("config generation failed: %w", err)
	}
	// Use LegacyCompat to write Manager config with mutex protection,
	// merging groupAllowFrom to avoid overwriting team leader additions.
	if d.legacy != nil && d.legacy.Enabled() {
		if err := d.legacy.PutManagerConfig(configJSON); err != nil {
			return fmt.Errorf("config push to storage failed: %w", err)
		}
	} else {
		if err := d.oss.PutObject(ctx, agentPrefix+"/openclaw.json", configJSON); err != nil {
			return fmt.Errorf("config push to storage failed: %w", err)
		}
	}

	// --- SOUL.md: inline > external ref ---
	soulContent := req.Spec.Soul
	if soulContent != "" {
		if err := d.oss.PutObject(ctx, agentPrefix+"/SOUL.md", []byte(soulContent)); err != nil {
			logger.Error(err, "SOUL.md push failed (non-fatal)")
		}
	}

	// --- AGENTS.md: inline > external ref ---
	agentsContent := req.Spec.Agents
	if agentsContent != "" {
		if err := d.oss.PutObject(ctx, agentPrefix+"/AGENTS.md", []byte(agentsContent)); err != nil {
			logger.Error(err, "AGENTS.md push failed (non-fatal)")
		}
	}

	// --- mcporter-servers.json ---
	if len(req.McpServers) > 0 {
		mcporterJSON, err := d.agentConfig.GenerateMcporterConfig(req.GatewayKey, req.McpServers)
		if err != nil {
			logger.Error(err, "mcporter config generation failed (non-fatal)")
		} else if mcporterJSON != nil {
			if err := d.oss.PutObject(ctx, agentPrefix+"/mcporter-servers.json", mcporterJSON); err != nil {
				logger.Error(err, "mcporter config push failed (non-fatal)")
			}
		}
	}

	// --- Matrix password for E2EE re-login ---
	if req.MatrixPassword != "" {
		if err := d.oss.PutObject(ctx, agentPrefix+"/credentials/matrix/password", []byte(req.MatrixPassword)); err != nil {
			logger.Error(err, "failed to write Matrix password to storage (non-fatal)")
		}
	}

	return nil
}

// --- Internal helpers ---

func redactPackageURI(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if username := u.User.Username(); username != "" {
		u.User = url.User(username)
	} else {
		u.User = nil
	}
	return u.String()
}

// prepareAndPushAgentsMD merges the builtin AGENTS.md section and injects
// coordination context in a single OSS read-write cycle.
func (d *Deployer) prepareAndPushAgentsMD(ctx context.Context, workerName, agentPrefix, role, runtime, teamName, teamLeaderName, teamAdminMatrixID string, teamCoordinatorIDs []string, inlineAgents string) error {
	logger := log.FromContext(ctx)
	builtinPath := filepath.Join(d.builtinAgentDir(role, runtime), "AGENTS.md")
	builtinContent, err := os.ReadFile(builtinPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read builtin AGENTS.md: %w", err)
	}
	if len(builtinContent) > 0 {
		logger.Info("AGENTS.md builtin template loaded", "worker", workerName, "role", role, "runtime", runtime, "path", builtinPath, "bytes", len(builtinContent))
	} else {
		logger.Info("AGENTS.md builtin template not found", "worker", workerName, "role", role, "runtime", runtime, "path", builtinPath)
	}

	// Priority: inline spec (user intent) > OSS (from package).
	// Read inline directly from memory to avoid local file race with background mc mirror.
	var content string
	source := "oss"
	if inlineAgents != "" {
		content = inlineAgents
		source = "inline spec.agents"
	}
	if content == "" && source == "oss" {
		existing, err := d.oss.GetObject(ctx, agentPrefix+"/AGENTS.md")
		if err != nil {
			if os.IsNotExist(err) {
				logger.Info("AGENTS.md package/OSS source not found", "worker", workerName, "key", agentPrefix+"/AGENTS.md")
			} else {
				logger.Error(err, "AGENTS.md package/OSS source read failed; continuing with empty content", "worker", workerName, "key", agentPrefix+"/AGENTS.md")
			}
		} else {
			logger.Info("AGENTS.md package/OSS source loaded", "worker", workerName, "key", agentPrefix+"/AGENTS.md", "bytes", len(existing), "hasBuiltinMarkers", strings.Contains(string(existing), "<!-- hiclaw-builtin-start -->"))
		}
		content = string(existing)
	}
	logger.Info("AGENTS.md source selected", "worker", workerName, "source", source, "bytes", len(content), "hasBuiltinMarkers", strings.Contains(content, "<!-- hiclaw-builtin-start -->"))
	if len(builtinContent) > 0 {
		sourceBytes := len(content)
		content = agentconfig.MergeBuiltinSection(content, string(builtinContent))
		logger.Info("AGENTS.md builtin section merged", "worker", workerName, "source", source, "builtinBytes", len(builtinContent), "sourceBytes", sourceBytes, "resultBytes", len(content))
	}

	// Team leaders get their coordination context from TeamReconciler.InjectCoordinationContext
	// which has the full context (room IDs, worker list). Skip here to avoid overwriting.
	if role != "team_leader" {
		if role == "standalone" && hasDecoupledTeamContext(content) {
			logger.Info("AGENTS.md team coordination context preserved", "worker", workerName, "role", role, "reason", "worker is likely referenced by a decoupled Team")
			if err := d.oss.PutObject(ctx, agentPrefix+"/AGENTS.md", []byte(content)); err != nil {
				return err
			}
			logger.Info("AGENTS.md pushed to storage", "worker", workerName, "key", agentPrefix+"/AGENTS.md", "bytes", len(content), "source", source)
			return nil
		}
		coordCtx := agentconfig.CoordinationContext{
			WorkerName:         workerName,
			MatrixDomain:       d.matrixDomain,
			TeamName:           teamName,
			TeamLeaderName:     teamLeaderName,
			TeamAdminID:        teamAdminMatrixID,
			TeamCoordinatorIDs: teamCoordinatorIDs,
		}
		if teamLeaderName != "" {
			coordCtx.Role = "worker"
		} else {
			coordCtx.Role = "standalone"
		}
		content = agentconfig.InjectCoordinationContext(content, coordCtx)
		logger.Info("AGENTS.md coordination context injected", "worker", workerName, "role", coordCtx.Role, "team", teamName, "teamLeader", teamLeaderName, "coordinatorCount", len(teamCoordinatorIDs), "resultBytes", len(content))
	} else {
		logger.Info("AGENTS.md coordination context skipped", "worker", workerName, "role", role, "reason", "team leader context is injected after room IDs are known")
	}

	if err := d.oss.PutObject(ctx, agentPrefix+"/AGENTS.md", []byte(content)); err != nil {
		return err
	}
	logger.Info("AGENTS.md pushed to storage", "worker", workerName, "key", agentPrefix+"/AGENTS.md", "bytes", len(content), "source", source)
	return nil
}

func hasDecoupledTeamContext(content string) bool {
	if !strings.Contains(content, "<!-- hiclaw-team-context-start -->") {
		return false
	}
	return strings.Contains(content, "Do NOT @mention Manager") ||
		strings.Contains(content, "- **Team Workers**:") ||
		strings.Contains(content, "- **Team Room**:")
}

// pushBuiltinSkills copies builtin skill directories to the worker's OSS prefix.
// Skills are read from the local agent template directory baked into the controller image.
func (d *Deployer) pushBuiltinSkills(ctx context.Context, workerName, agentPrefix, role, runtime string) error {
	skillsDir := filepath.Join(d.builtinAgentDir(role, runtime), "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no builtin skills for this role/runtime
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillName := entry.Name()
		src := skillsDir + "/" + skillName + "/"
		dst := agentPrefix + "/skills/" + skillName + "/"
		if err := d.oss.Mirror(ctx, src, dst, oss.MirrorOptions{Overwrite: true}); err != nil {
			return fmt.Errorf("push skill %s: %w", skillName, err)
		}
	}
	return nil
}

func (d *Deployer) pushBuiltinTopLevelFiles(ctx context.Context, workerName, agentPrefix, role, runtime string) error {
	agentDir := d.builtinAgentDir(role, runtime)
	for _, name := range []string{"HEARTBEAT.md"} {
		ossKey := agentPrefix + "/" + name
		if existing, _ := d.oss.GetObject(ctx, ossKey); existing != nil {
			log.FromContext(ctx).Info("seed-only: skipping (already in MinIO)", "file", name, "worker", workerName)
			continue
		}
		src := filepath.Join(agentDir, name)
		content, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := d.oss.PutObject(ctx, ossKey, content); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) builtinAgentDir(role, runtime string) string {
	baseDir := filepath.Dir(d.workerAgentDir)
	switch role {
	case "team_leader":
		return filepath.Join(baseDir, "team-leader-agent")
	default:
		switch runtime {
		case "copaw":
			return filepath.Join(baseDir, "copaw-worker-agent")
		case "hermes":
			return filepath.Join(baseDir, "hermes-worker-agent")
		}
		return d.workerAgentDir
	}
}

// mergeUserPluginConfig preserves user-customized plugin entries from an
// existing openclaw.json when regenerating config on update. The generated
// config provides defaults for any new entries; existing user-modified
// entries override generated values so that customizations (e.g. memory-core
// dreaming schedule) survive controller reconciles.
//
// It also preserves channels.matrix.groupAllowFrom and channels.matrix.dm.allowFrom
// from the existing config, because TeamReconciler in the decoupled path
// overrides these to [leader, admin] for team members. WorkerReconciler is
// team-agnostic and would otherwise revert them to standalone [manager, admin]
// on every reconcile, breaking team-scoped task delivery.
func mergeUserPluginConfig(generatedJSON, existingJSON []byte) ([]byte, error) {
	var generated, existing map[string]interface{}
	if err := json.Unmarshal(generatedJSON, &generated); err != nil {
		return generatedJSON, err
	}
	if err := json.Unmarshal(existingJSON, &existing); err != nil {
		return generatedJSON, err
	}

	preserveChannelMatrixAllowFrom(generated, existing)

	genPlugins, _ := generated["plugins"].(map[string]interface{})
	existPlugins, _ := existing["plugins"].(map[string]interface{})
	if genPlugins == nil || existPlugins == nil {
		return json.MarshalIndent(generated, "", "  ")
	}

	// Merge plugin entries: generated provides base/defaults, existing
	// user-modified values override. This preserves user customizations
	// of memory-core, diagnostics-otel, etc. while letting the controller
	// inject new default entries on upgrade.
	genEntries, _ := genPlugins["entries"].(map[string]interface{})
	existEntries, _ := existPlugins["entries"].(map[string]interface{})
	if existEntries != nil && genEntries != nil {
		merged := make(map[string]interface{})
		for k, v := range genEntries {
			merged[k] = v
		}
		for k, v := range existEntries {
			if genV, has := merged[k]; has {
				merged[k] = deepMergeMap(toMap(genV), toMap(v))
			} else {
				merged[k] = v
			}
		}
		genPlugins["entries"] = merged
	}

	// Union plugin load paths so user-added extension directories survive.
	genLoad, _ := genPlugins["load"].(map[string]interface{})
	existLoad, _ := existPlugins["load"].(map[string]interface{})
	if genLoad != nil && existLoad != nil {
		genPaths := toStringSliceCompat(genLoad["paths"])
		existPaths := toStringSliceCompat(existLoad["paths"])
		seen := make(map[string]bool, len(genPaths)+len(existPaths))
		var unionPaths []string
		for _, p := range genPaths {
			if !seen[p] {
				seen[p] = true
				unionPaths = append(unionPaths, p)
			}
		}
		for _, p := range existPaths {
			if !seen[p] {
				seen[p] = true
				unionPaths = append(unionPaths, p)
			}
		}
		genLoad["paths"] = unionPaths
	}

	return json.MarshalIndent(generated, "", "  ")
}

func toMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

// preserveChannelMatrixAllowFrom copies channels.matrix.groupAllowFrom and
// channels.matrix.dm.allowFrom from existing into generated when the existing
// values are non-empty. This ensures TeamReconciler-injected team-mode
// channel policies are not reverted to standalone defaults on every Worker
// reconcile.
func preserveChannelMatrixAllowFrom(generated, existing map[string]interface{}) {
	existChannels, _ := existing["channels"].(map[string]interface{})
	if existChannels == nil {
		return
	}
	existMatrix, _ := existChannels["matrix"].(map[string]interface{})
	if existMatrix == nil {
		return
	}

	genChannels, _ := generated["channels"].(map[string]interface{})
	if genChannels == nil {
		genChannels = make(map[string]interface{})
		generated["channels"] = genChannels
	}
	genMatrix, _ := genChannels["matrix"].(map[string]interface{})
	if genMatrix == nil {
		genMatrix = make(map[string]interface{})
		genChannels["matrix"] = genMatrix
	}

	if existAllow, ok := existMatrix["groupAllowFrom"].([]interface{}); ok && len(existAllow) > 0 {
		genMatrix["groupAllowFrom"] = existAllow
	}
	if existDM, ok := existMatrix["dm"].(map[string]interface{}); ok {
		genDM, _ := genMatrix["dm"].(map[string]interface{})
		if genDM == nil {
			genDM = make(map[string]interface{})
			genMatrix["dm"] = genDM
		}
		if existDMAllow, ok := existDM["allowFrom"].([]interface{}); ok && len(existDMAllow) > 0 {
			genDM["allowFrom"] = existDMAllow
		}
	}
}

// deepMergeMap recursively merges override into base; override wins on
// leaf-level conflicts. Both inputs must be non-nil (caller guards).
func deepMergeMap(base, override map[string]interface{}) map[string]interface{} {
	if base == nil {
		return override
	}
	if override == nil {
		return base
	}
	result := make(map[string]interface{}, len(base)+len(override))
	for k, v := range base {
		result[k] = v
	}
	for k, ov := range override {
		bv, exists := result[k]
		if !exists {
			result[k] = ov
			continue
		}
		bMap, bIsMap := bv.(map[string]interface{})
		oMap, oIsMap := ov.(map[string]interface{})
		if bIsMap && oIsMap {
			result[k] = deepMergeMap(bMap, oMap)
		} else {
			result[k] = ov
		}
	}
	return result
}

func toStringSliceCompat(v interface{}) []string {
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
