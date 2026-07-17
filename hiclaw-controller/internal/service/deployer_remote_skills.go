package service

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/executor"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

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
