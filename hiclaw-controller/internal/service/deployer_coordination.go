package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

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
