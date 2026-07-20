package oss

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// MinIOAdminClient implements StorageAdminClient for embedded-mode MinIO.
// It uses the `mc admin` CLI to manage users and policies.
type MinIOAdminClient struct {
	config     Config
	aliasReady bool
}

// NewMinIOAdminClient creates a StorageAdminClient for managing MinIO users.
func NewMinIOAdminClient(cfg Config) *MinIOAdminClient {
	if cfg.MCBinary == "" {
		cfg.MCBinary = "mc"
	}
	if cfg.Alias == "" {
		cfg.Alias = "agentteams"
	}
	return &MinIOAdminClient{config: cfg}
}

func (c *MinIOAdminClient) ensureAlias(ctx context.Context) error {
	if c.aliasReady || c.config.Endpoint == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, c.config.MCBinary, "alias", "set", c.config.Alias, c.config.Endpoint, c.config.AccessKey, c.config.SecretKey)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mc alias set: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	c.aliasReady = true
	return nil
}

func (c *MinIOAdminClient) EnsureUser(ctx context.Context, username, password string) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	logger := log.FromContext(ctx)
	logger.Info("ensuring MinIO user", "user", username, "alias", c.config.Alias)
	// mc admin user add is idempotent — updates password if user exists
	_, err := c.runMCAdmin(ctx, "user", "add", c.config.Alias, username, password)
	if err != nil && !strings.Contains(err.Error(), "already") {
		return fmt.Errorf("ensure minio user %s: %w", username, err)
	}
	logger.Info("MinIO user ensured", "user", username, "alias", c.config.Alias)
	return nil
}

func (c *MinIOAdminClient) EnsurePolicy(ctx context.Context, req PolicyRequest) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	policyName := "worker-" + req.WorkerName
	bucket := req.Bucket
	if bucket == "" {
		bucket = c.config.Bucket
	}

	policy := c.buildWorkerPolicy(req.WorkerName, bucket, req.TeamName, req.IsManager)
	logger := log.FromContext(ctx)
	logger.Info("ensuring MinIO worker policy",
		"worker", req.WorkerName,
		"policy", policyName,
		"bucket", bucket,
		"team", req.TeamName,
		"isManager", req.IsManager,
		"listPrefixes", policyListPrefixes(policy),
		"objectResources", policyObjectResources(policy),
	)
	policyJSON, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal policy: %w", err)
	}

	policyFile, err := os.CreateTemp("", "hiclaw-policy-*.json")
	if err != nil {
		return fmt.Errorf("create policy temp file: %w", err)
	}
	defer os.Remove(policyFile.Name())

	if _, err := policyFile.Write(policyJSON); err != nil {
		policyFile.Close()
		return fmt.Errorf("write policy file: %w", err)
	}
	policyFile.Close()

	// Detach before remove so a worker keeps the freshly generated policy
	// after bucket/prefix rename changes instead of an older attached policy.
	if _, err := c.runMCAdmin(ctx, "policy", "detach", c.config.Alias, policyName, "--user", req.WorkerName); err != nil {
		logger.Info("MinIO worker policy detach skipped", "worker", req.WorkerName, "policy", policyName, "error", err.Error())
	}
	if _, err := c.runMCAdmin(ctx, "policy", "remove", c.config.Alias, policyName); err != nil {
		logger.Info("MinIO worker policy remove skipped", "worker", req.WorkerName, "policy", policyName, "error", err.Error())
	}
	if _, err := c.runMCAdmin(ctx, "policy", "create", c.config.Alias, policyName, policyFile.Name()); err != nil {
		return fmt.Errorf("create policy %s: %w", policyName, err)
	}
	logger.Info("MinIO worker policy created", "worker", req.WorkerName, "policy", policyName, "bucket", bucket)
	if _, err := c.runMCAdmin(ctx, "policy", "attach", c.config.Alias, policyName, "--user", req.WorkerName); err != nil {
		return fmt.Errorf("attach policy %s to user %s: %w", policyName, req.WorkerName, err)
	}
	logger.Info("MinIO worker policy attached", "worker", req.WorkerName, "policy", policyName, "bucket", bucket)
	return nil
}

func policyListPrefixes(policy s3Policy) []string {
	if len(policy.Statement) < 2 || policy.Statement[1].Condition == nil {
		return nil
	}
	stringLike, ok := policy.Statement[1].Condition["StringLike"].(map[string]interface{})
	if !ok {
		return nil
	}
	prefixes, ok := stringLike["s3:prefix"].([]string)
	if !ok {
		return nil
	}
	return append([]string(nil), prefixes...)
}

func policyObjectResources(policy s3Policy) []string {
	if len(policy.Statement) < 3 {
		return nil
	}
	return append([]string(nil), policy.Statement[2].Resource...)
}

func (c *MinIOAdminClient) DeleteUser(ctx context.Context, username string) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	policyName := "worker-" + username
	// Detach and remove policy first (ignore errors)
	c.runMCAdmin(ctx, "policy", "detach", c.config.Alias, policyName, "--user", username)
	c.runMCAdmin(ctx, "policy", "remove", c.config.Alias, policyName)
	// Remove user
	_, err := c.runMCAdmin(ctx, "user", "remove", c.config.Alias, username)
	if err != nil && !strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("delete minio user %s: %w", username, err)
	}
	return nil
}

type s3Policy struct {
	Version   string              `json:"Version"`
	Statement []s3PolicyStatement `json:"Statement"`
}

type s3PolicyStatement struct {
	Effect    string                 `json:"Effect"`
	Action    []string               `json:"Action"`
	Resource  []string               `json:"Resource"`
	Condition map[string]interface{} `json:"Condition,omitempty"`
}

func (c *MinIOAdminClient) buildWorkerPolicy(workerName, bucket, teamName string, isManager bool) s3Policy {
	listPrefixes := []string{
		"agents",
		"agents/",
		fmt.Sprintf("agents/%s", workerName),
		fmt.Sprintf("agents/%s/", workerName),
		fmt.Sprintf("agents/%s/*", workerName),
		fmt.Sprintf("agents/%s/openclaw.json", workerName),
		fmt.Sprintf("agents/%s/SOUL.md", workerName),
		fmt.Sprintf("agents/%s/AGENTS.md", workerName),
		fmt.Sprintf("agents/%s/HEARTBEAT.md", workerName),
		fmt.Sprintf("agents/%s/config", workerName),
		fmt.Sprintf("agents/%s/config/", workerName),
		fmt.Sprintf("agents/%s/config/*", workerName),
		"shared",
		"shared/",
		"shared/*",
	}
	rwResources := []string{
		fmt.Sprintf("arn:aws:s3:::%s/agents/%s", bucket, workerName),
		fmt.Sprintf("arn:aws:s3:::%s/agents/%s/", bucket, workerName),
		fmt.Sprintf("arn:aws:s3:::%s/agents/%s/*", bucket, workerName),
		fmt.Sprintf("arn:aws:s3:::%s/shared", bucket),
		fmt.Sprintf("arn:aws:s3:::%s/shared/", bucket),
		fmt.Sprintf("arn:aws:s3:::%s/shared/*", bucket),
	}

	if isManager {
		listPrefixes = append(listPrefixes,
			"manager",
			"manager/",
			"manager/*",
		)
		rwResources = append(rwResources,
			fmt.Sprintf("arn:aws:s3:::%s/manager", bucket),
			fmt.Sprintf("arn:aws:s3:::%s/manager/", bucket),
			fmt.Sprintf("arn:aws:s3:::%s/manager/*", bucket),
		)
	}

	if teamName != "" {
		listPrefixes = append(listPrefixes,
			"teams",
			"teams/",
			fmt.Sprintf("teams/%s", teamName),
			fmt.Sprintf("teams/%s/", teamName),
			fmt.Sprintf("teams/%s/*", teamName),
		)
		rwResources = append(rwResources,
			fmt.Sprintf("arn:aws:s3:::%s/teams/%s", bucket, teamName),
			fmt.Sprintf("arn:aws:s3:::%s/teams/%s/", bucket, teamName),
			fmt.Sprintf("arn:aws:s3:::%s/teams/%s/*", bucket, teamName),
		)
	}

	return s3Policy{
		Version: "2012-10-17",
		Statement: []s3PolicyStatement{
			{
				Effect:   "Allow",
				Action:   []string{"s3:GetBucketLocation"},
				Resource: []string{fmt.Sprintf("arn:aws:s3:::%s", bucket)},
			},
			{
				Effect:   "Allow",
				Action:   []string{"s3:ListBucket"},
				Resource: []string{fmt.Sprintf("arn:aws:s3:::%s", bucket)},
				Condition: map[string]interface{}{
					"StringLike": map[string]interface{}{
						"s3:prefix": listPrefixes,
					},
				},
			},
			{
				Effect:   "Allow",
				Action:   []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"},
				Resource: rwResources,
			},
		},
	}
}

func (c *MinIOAdminClient) runMCAdmin(ctx context.Context, args ...string) (string, error) {
	fullArgs := append([]string{"admin"}, args...)
	cmd := exec.CommandContext(ctx, c.config.MCBinary, fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mc admin %s: %w (stderr: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
