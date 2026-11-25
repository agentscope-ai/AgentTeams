package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/modelhealth"
)

// CheckManagerHealth reads the Harness result; protocol and credentials stay in the Harness.
func (d *Deployer) CheckManagerHealth(ctx context.Context, runtime, admin, room string, notify modelhealth.Notify) error {
	report := modelhealth.Unknown("missing_report")
	data, err := d.oss.GetObject(ctx, "agents/manager/runtime/health.json")
	if err == nil {
		report = modelhealth.Parse(data, runtime, "manager", time.Now().UTC())
	} else if !os.IsNotExist(err) {
		return errors.New("Harness health report unavailable")
	}
	return modelhealth.Check(ctx, d.oss, "agents/manager/model-health.json", modelhealth.Config{Runtime: runtime, Admin: admin, Room: room}, report, notify)
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
