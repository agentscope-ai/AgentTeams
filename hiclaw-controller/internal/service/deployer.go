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

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	"github.com/hiclaw/hiclaw-controller/internal/credprovider"
	"github.com/hiclaw/hiclaw-controller/internal/executor"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
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

	// From provisioning
	MatrixToken    string
	GatewayKey     string
	MatrixPassword string

	// AIGatewayURL overrides the cluster-wide AI Gateway URL when modelProvider is set.
	AIGatewayURL string

	// MCP servers declared in spec.mcpServers. The deployer translates this into
	// mcporter-servers.json and injects Authorization: Bearer <GatewayKey>.
	McpServers []v1beta1.MCPServer

	TeamAdminMatrixID  string
	TeamCoordinatorIDs []string

	// Heartbeat config from Team CR leader spec (nil for non-leader workers)
	Heartbeat *agentconfig.HeartbeatConfig

	IsUpdate bool
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
	TeamWorkers        []string
	TeamAdminID        string
	TeamCoordinatorIDs []string
	LeaderSoul         string // from CR spec.leader.soul; used as seed if non-empty
}

// --- Deployer ---

// DeployerConfig holds configuration for constructing a Deployer.
type DeployerConfig struct {
	AgentConfig        *agentconfig.Generator
	OSS                oss.StorageClient
	Executor           *executor.Shell
	Packages           *executor.PackageResolver
	Legacy             *LegacyCompat
	AgentFSDir         string // embedded: /root/hiclaw-fs/agents
	WorkerAgentDir     string // source for builtin agent files
	MatrixDomain       string

	// NacosCredClient is used when remoteSkills use sts-hiclaw (see CRD authType).
	NacosCredClient credprovider.Client
}

// Deployer orchestrates configuration deployment for workers: package resolution,
// inline config writes, openclaw.json generation, AGENTS.md merging, skill pushing,
// and OSS synchronization.
type Deployer struct {
	agentConfig         *agentconfig.Generator
	oss                 oss.StorageClient
	executor            *executor.Shell
	packages            *executor.PackageResolver
	legacy              *LegacyCompat
	agentFSDir          string
	workerAgentDir      string
	matrixDomain        string
	nacosCredClient     credprovider.Client
}

func NewDeployer(cfg DeployerConfig) *Deployer {
	return &Deployer{
		agentConfig:         cfg.AgentConfig,
		oss:                 cfg.OSS,
		executor:            cfg.Executor,
		packages:            cfg.Packages,
		legacy:              cfg.Legacy,
		agentFSDir:          cfg.AgentFSDir,
		workerAgentDir:      cfg.WorkerAgentDir,
		matrixDomain:        cfg.MatrixDomain,
		nacosCredClient:     cfg.NacosCredClient,
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

	// On update, preserve user-customized plugin entries (e.g. memory-core
	// dreaming schedule) from the existing openclaw.json in storage. The
	// generated config provides defaults for any new entries; existing
	// user-modified entries override the generated values.
	if req.IsUpdate {
		if existingJSON, err := d.oss.GetObject(ctx, agentPrefix+"/openclaw.json"); err == nil && len(existingJSON) > 0 {
			if merged, mergeErr := mergeUserPluginConfig(configJSON, existingJSON); mergeErr != nil {
				logger.Error(mergeErr, "plugin config merge failed, using generated config")
			} else {
				configJSON = merged
			}
		}
	}

	if err := d.oss.PutObject(ctx, agentPrefix+"/openclaw.json", configJSON); err != nil {
		return fmt.Errorf("config push to storage failed: %w", err)
	}

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

	// --- Push builtin skills from worker-agent template ---
	if err := d.pushBuiltinSkills(ctx, req.Name, agentPrefix, req.Role, req.Spec.Runtime); err != nil {
		logger.Error(err, "builtin skills push failed (non-fatal)")
	}

	return nil
}

// InjectCoordinationContext writes team coordination context into the leader's AGENTS.md.
func (d *Deployer) InjectCoordinationContext(ctx context.Context, req CoordinationDeployRequest) error {
	leaderAgentPrefix := fmt.Sprintf("agents/%s", req.LeaderName)

	teamWorkers := make([]agentconfig.TeamWorkerInfo, 0, len(req.TeamWorkers))
	for _, wn := range req.TeamWorkers {
		teamWorkers = append(teamWorkers, agentconfig.TeamWorkerInfo{Name: wn})
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

// renderAndPushSoulTemplate seeds the team leader's SOUL.md into OSS.
// Seed-only: if OSS already has SOUL.md, it is never overwritten -- the agent
// owns it after first startup. Priority: CR spec.leader.soul > template.
func (d *Deployer) renderAndPushSoulTemplate(ctx context.Context, agentPrefix string, req CoordinationDeployRequest) error {
	soulKey := agentPrefix + "/SOUL.md"
	_, err := d.oss.GetObject(ctx, soulKey)
	if err == nil {
		log.FromContext(ctx).Info("SOUL.md: seed-only, keeping existing version", "leader", req.LeaderName)
		return nil
	}
	if !os.IsNotExist(err) {
		log.FromContext(ctx).Error(err, "SOUL.md: check existing failed, skipping seed", "leader", req.LeaderName)
		return nil
	}

	if req.LeaderSoul != "" {
		return d.oss.PutObject(ctx, soulKey, []byte(req.LeaderSoul))
	}

	tmplPath := filepath.Join(d.builtinAgentDir("team_leader", ""), "SOUL.md.tmpl")
	tmplData, err := os.ReadFile(tmplPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no template, nothing to render
		}
		return fmt.Errorf("read SOUL.md.tmpl: %w", err)
	}

	workerNames := make([]string, 0, len(req.TeamWorkers))
	for _, wn := range req.TeamWorkers {
		workerNames = append(workerNames, wn)
	}

	result := string(tmplData)
	result = strings.ReplaceAll(result, "${TEAM_LEADER_NAME}", req.LeaderName)
	result = strings.ReplaceAll(result, "${TEAM_NAME}", req.TeamName)
	result = strings.ReplaceAll(result, "${TEAM_WORKERS}", strings.Join(workerNames, ", "))

	return d.oss.PutObject(ctx, soulKey, []byte(result))
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
	scriptPath := "/opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh"
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		logger.Info("push-worker-skills.sh not found (incluster mode), skipping on-demand skill push",
			"worker", workerName, "skills", skills)
		return nil
	}
	_, err := d.executor.RunSimple(ctx, scriptPath, "--worker", workerName, "--no-notify")
	return err
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
	for _, subdir := range []string{"shared/tasks/", "shared/projects/", "shared/knowledge/"} {
		if err := d.oss.PutObject(ctx, prefix+subdir+".keep", []byte("")); err != nil {
			return fmt.Errorf("create %s%s: %w", prefix, subdir, err)
		}
	}
	return nil
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
		case "openhuman":
			return filepath.Join(baseDir, "openhuman-worker-agent")
		}
		return d.workerAgentDir
	}
}

// mergeUserPluginConfig preserves user-customized plugin entries from an
// existing openclaw.json when regenerating config on update. The generated
// config provides defaults for any new entries; existing user-modified
// entries override generated values so that customizations (e.g. memory-core
// dreaming schedule) survive controller reconciles.
func mergeUserPluginConfig(generatedJSON, existingJSON []byte) ([]byte, error) {
	var generated, existing map[string]interface{}
	if err := json.Unmarshal(generatedJSON, &generated); err != nil {
		return generatedJSON, err
	}
	if err := json.Unmarshal(existingJSON, &existing); err != nil {
		return generatedJSON, err
	}

	genPlugins, _ := generated["plugins"].(map[string]interface{})
	existPlugins, _ := existing["plugins"].(map[string]interface{})
	if genPlugins == nil || existPlugins == nil {
		return generatedJSON, nil
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
