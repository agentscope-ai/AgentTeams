package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
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

	// MCP servers declared in spec.mcpServers. The deployer translates this into
	// mcporter-servers.json and injects Authorization: Bearer <GatewayKey>.
	McpServers []v1beta1.MCPServer

	TeamAdminMatrixID string

	// Heartbeat config from Team CR leader spec (nil for non-leader workers)
	Heartbeat *agentconfig.HeartbeatConfig

	IsUpdate bool
}

// CoordinationDeployRequest describes coordination context injection for a team leader.
type CoordinationDeployRequest struct {
	LeaderName        string
	Role              string
	TeamName          string
	TeamRoomID        string
	LeaderDMRoomID    string
	HeartbeatEvery    string
	WorkerIdleTimeout string
	TeamWorkers       []string
	TeamAdminID       string
}

// --- Deployer ---

// DeployerConfig holds configuration for constructing a Deployer.
type DeployerConfig struct {
	AgentConfig    *agentconfig.Generator
	OSS            oss.StorageClient
	Executor       *executor.Shell
	Packages       *executor.PackageResolver
	Legacy         *LegacyCompat
	AgentFSDir     string // embedded: /root/hiclaw-fs/agents
	WorkerAgentDir string // source for builtin agent files
	MatrixDomain   string
}

// Deployer orchestrates configuration deployment for workers: package resolution,
// inline config writes, openclaw.json generation, AGENTS.md merging, skill pushing,
// and OSS synchronization.
type Deployer struct {
	agentConfig    *agentconfig.Generator
	oss            oss.StorageClient
	executor       *executor.Shell
	packages       *executor.PackageResolver
	legacy         *LegacyCompat
	agentFSDir     string
	workerAgentDir string
	matrixDomain   string
}

func NewDeployer(cfg DeployerConfig) *Deployer {
	return &Deployer{
		agentConfig:    cfg.AgentConfig,
		oss:            cfg.OSS,
		executor:       cfg.Executor,
		packages:       cfg.Packages,
		legacy:         cfg.Legacy,
		agentFSDir:     cfg.AgentFSDir,
		workerAgentDir: cfg.WorkerAgentDir,
		matrixDomain:   cfg.MatrixDomain,
	}
}

// DeployPackage resolves, downloads, extracts, and deploys a package to OSS.
// No-op if uri is empty.
func (d *Deployer) DeployPackage(ctx context.Context, name, uri string, isUpdate bool) error {
	if uri == "" || d.packages == nil {
		return nil
	}

	extractedDir, err := d.packages.ResolveAndExtract(ctx, uri, name)
	if err != nil {
		return fmt.Errorf("package resolve/extract failed: %w", err)
	}
	if extractedDir == "" {
		return nil
	}

	if err := d.packages.DeployToMinIO(ctx, extractedDir, name, isUpdate); err != nil {
		return fmt.Errorf("package deploy failed: %w", err)
	}

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

	// --- Sync local agent files to storage FIRST (base layer) ---
	// Mirror provides the base: package files, memory, custom skills, etc.
	// All subsequent PutObject calls overwrite on top with authoritative content.
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
	mirrorExcludes := []string{"SOUL.md", "AGENTS.md", "HEARTBEAT.md"}
	if err := d.oss.Mirror(ctx, localAgentDir+"/", agentPrefix+"/", oss.MirrorOptions{Overwrite: true, Exclude: mirrorExcludes}); err != nil {
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

	// --- SOUL.md ---
	// Priority: inline spec (user intent) > local file (from package) > generated default.
	// Inline spec is read directly from memory to avoid local file race with background mc mirror.
	soulPath := filepath.Join(localAgentDir, "SOUL.md")
	if req.Spec.Soul != "" {
		if err := d.oss.PutObject(ctx, agentPrefix+"/SOUL.md", []byte(req.Spec.Soul)); err != nil {
			logger.Error(err, "SOUL.md push failed (non-fatal)")
		}
	} else if soulData, err := os.ReadFile(soulPath); err == nil {
		if err := d.oss.PutObject(ctx, agentPrefix+"/SOUL.md", soulData); err != nil {
			logger.Error(err, "SOUL.md push failed (non-fatal)")
		}
	} else if !req.IsUpdate && req.Role != "team_leader" {
		// Team leaders get SOUL.md from template rendering in InjectCoordinationContext.
		soulContent := fmt.Sprintf("# %s\n\nYou are %s, an AI worker agent.\n", req.Name, req.Name)
		if err := d.oss.PutObject(ctx, agentPrefix+"/SOUL.md", []byte(soulContent)); err != nil {
			logger.Error(err, "SOUL.md push failed (non-fatal)")
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
	if err := d.prepareAndPushAgentsMD(ctx, req.Name, agentPrefix, req.Role, req.Spec.Runtime, req.TeamName, req.TeamLeaderName, req.TeamAdminMatrixID, req.Spec.Agents); err != nil {
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
		WorkerName:        req.LeaderName,
		Role:              req.Role,
		MatrixDomain:      d.matrixDomain,
		TeamName:          req.TeamName,
		TeamRoomID:        req.TeamRoomID,
		LeaderDMRoomID:    req.LeaderDMRoomID,
		HeartbeatEvery:    req.HeartbeatEvery,
		WorkerIdleTimeout: req.WorkerIdleTimeout,
		TeamWorkers:       teamWorkers,
		TeamAdminID:       req.TeamAdminID,
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

// renderAndPushSoulTemplate reads SOUL.md.tmpl from the builtin team-leader-agent
// directory, substitutes ${VAR} placeholders, and pushes the result as SOUL.md.
func (d *Deployer) renderAndPushSoulTemplate(ctx context.Context, agentPrefix string, req CoordinationDeployRequest) error {
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

	return d.oss.PutObject(ctx, agentPrefix+"/SOUL.md", []byte(result))
}

// PushOnDemandSkills runs the push-worker-skills.sh script for on-demand skills.
// No-op if no skills or executor is nil.
// NOTE: In incluster mode, the script may not exist in the controller container.
// On-demand skills are then expected to be pushed by the Manager agent via its
// worker-management skill at runtime.
func (d *Deployer) PushOnDemandSkills(ctx context.Context, workerName string, skills []string) error {
	logger := log.FromContext(ctx)
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

// CleanupOSSData removes all agent data from OSS for a deleted worker.
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
		WorkerName:  "manager",
		MatrixToken: req.MatrixToken,
		GatewayKey:  req.GatewayKey,
		ModelName:   req.Spec.Model,
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

	// --- SOUL.md (only if explicitly set in CRD spec) ---
	if req.Spec.Soul != "" {
		if err := d.oss.PutObject(ctx, agentPrefix+"/SOUL.md", []byte(req.Spec.Soul)); err != nil {
			logger.Error(err, "SOUL.md push failed (non-fatal)")
		}
	}

	// --- AGENTS.md (only if explicitly set in CRD spec) ---
	if req.Spec.Agents != "" {
		if err := d.oss.PutObject(ctx, agentPrefix+"/AGENTS.md", []byte(req.Spec.Agents)); err != nil {
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

// prepareAndPushAgentsMD merges the builtin AGENTS.md section and injects
// coordination context in a single OSS read-write cycle.
func (d *Deployer) prepareAndPushAgentsMD(ctx context.Context, workerName, agentPrefix, role, runtime, teamName, teamLeaderName, teamAdminMatrixID, inlineAgents string) error {
	builtinPath := filepath.Join(d.builtinAgentDir(role, runtime), "AGENTS.md")
	builtinContent, err := os.ReadFile(builtinPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read builtin AGENTS.md: %w", err)
	}

	// Priority: inline spec (user intent) > OSS (from package).
	// Read inline directly from memory to avoid local file race with background mc mirror.
	var content string
	if inlineAgents != "" {
		content = inlineAgents
	} else {
		existing, _ := d.oss.GetObject(ctx, agentPrefix+"/AGENTS.md")
		content = string(existing)
	}
	if len(builtinContent) > 0 {
		content = agentconfig.MergeBuiltinSection(content, string(builtinContent))
	}

	// Team leaders get their coordination context from TeamReconciler.InjectCoordinationContext
	// which has the full context (room IDs, worker list). Skip here to avoid overwriting.
	if role != "team_leader" {
		coordCtx := agentconfig.CoordinationContext{
			WorkerName:     workerName,
			MatrixDomain:   d.matrixDomain,
			TeamName:       teamName,
			TeamLeaderName: teamLeaderName,
			TeamAdminID:    teamAdminMatrixID,
		}
		if teamLeaderName != "" {
			coordCtx.Role = "worker"
		} else {
			coordCtx.Role = "standalone"
		}
		content = agentconfig.InjectCoordinationContext(content, coordCtx)
	}

	return d.oss.PutObject(ctx, agentPrefix+"/AGENTS.md", []byte(content))
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
