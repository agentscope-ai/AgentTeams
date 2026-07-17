package service

import (
	"context"
	"fmt"
	"net/url"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

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
