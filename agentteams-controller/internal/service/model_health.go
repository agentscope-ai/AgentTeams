package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/modelhealth"
	"sigs.k8s.io/yaml"
)

// CheckManagerModel reads the deployed contract and uses the Harness credential.
func (d *Deployer) CheckManagerModel(ctx context.Context, runtime, key, admin, room string, notify modelhealth.Notify) error {
	cfg := modelhealth.Config{Runtime: runtime, Key: key, Admin: admin, Room: room, Protocol: "chat"}
	if backend.UsesMemberRuntimeConfig(runtime) {
		b, err := d.oss.GetObject(ctx, memberRuntimeConfigObjectKey("manager"))
		if err != nil {
			return errors.New("model health configuration unavailable")
		}
		var doc memberRuntimeConfigDocument
		if yaml.Unmarshal(b, &doc) != nil {
			return errors.New("model health configuration invalid")
		}
		if doc.Desired.Model != nil {
			cfg.Model = doc.Desired.Model.Model
			cfg.URL = doc.Desired.Model.GatewayURL
			if doc.Desired.Model.GatewayKey != "" {
				cfg.Key = doc.Desired.Model.GatewayKey
			}
		}
		if runtime == "codex" {
			cfg.Protocol = "responses"
		} else if runtime == backend.RuntimeClaudeCode {
			cfg.Protocol = "anthropic-messages"
		}
		cfg.URL = strings.TrimRight(cfg.URL, "/")
		if cfg.URL != "" && !strings.HasSuffix(cfg.URL, "/v1") {
			cfg.URL += "/v1"
		}
	} else {
		b, err := d.oss.GetObject(ctx, "agents/manager/openclaw.json")
		if err != nil {
			return errors.New("model health configuration unavailable")
		}
		var doc struct {
			Agents struct {
				Defaults struct {
					Model struct {
						Primary string `json:"primary"`
					} `json:"model"`
				} `json:"defaults"`
			} `json:"agents"`
			Models struct {
				Providers map[string]struct {
					URL string `json:"baseUrl"`
					Key string `json:"apiKey"`
					API string `json:"api"`
				}
			} `json:"models"`
		}
		if json.Unmarshal(b, &doc) != nil {
			return errors.New("model health configuration invalid")
		}
		pid, model, ok := strings.Cut(doc.Agents.Defaults.Model.Primary, "/")
		if !ok {
			return errors.New("model health model selection invalid")
		}
		p, ok := doc.Models.Providers[pid]
		if !ok {
			return errors.New("model health provider unavailable")
		}
		cfg.Model = model
		cfg.URL = p.URL
		cfg.Key = p.Key
		cfg.Protocol = p.API
	}
	return modelhealth.Check(ctx, d.oss, "agents/manager/model-health.json", cfg, nil, notify)
}
func (p *Provisioner) ModelHealthAdmin() string {
	return fmt.Sprintf("@%s:%s", p.adminUser, p.matrixDomain)
}
func (p *Provisioner) SendModelHealthNotice(ctx context.Context, room, token, txn, body string) error {
	members, err := p.matrix.ListRoomMembersWithToken(ctx, room, token)
	if err != nil {
		return errors.New("admin DM membership unavailable")
	}
	adminJoined := false
	for _, m := range members {
		if m.UserID == p.ModelHealthAdmin() && m.Membership == "join" {
			adminJoined = true
		}
	}
	if !adminJoined {
		return errors.New("authorized admin not joined to DM")
	}
	sender, ok := p.matrix.(interface {
		SendNotice(context.Context, string, string, string, string) error
	})
	if !ok {
		return errors.New("idempotent Matrix notice unavailable")
	}
	return sender.SendNotice(ctx, room, token, txn, body)
}
