package service

import (
	"context"
	"fmt"
	"io/fs"
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
	AgentFSDir     string // embedded: /root/hiclaw-fs/agents
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
