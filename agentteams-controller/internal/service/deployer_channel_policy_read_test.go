package service

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

type channelPolicyReadFailure struct {
	*ossfake.Memory
	err error
}

func (s *channelPolicyReadFailure) GetObject(context.Context, string) ([]byte, error) {
	return nil, s.err
}

func TestInjectChannelPolicyDoesNotPublishAfterReadFailure(t *testing.T) {
	ctx := context.Background()
	const key = "agents/worker/openclaw.json"
	const full = `{"gateway":{"mode":"local"},"models":{"mode":"merge"}}`
	for _, missing := range []bool{true, false} {
		name := "read-error-with-existing-config"
		if missing {
			name = "not-provisioned-yet"
		}
		t.Run(name, func(t *testing.T) {
			store := ossfake.NewMemory()
			readErr := errors.New("storage read failed")
			if missing {
				readErr = os.ErrNotExist
			} else if err := store.PutObject(ctx, key, []byte(full)); err != nil {
				t.Fatal(err)
			}
			d := NewDeployer(DeployerConfig{OSS: &channelPolicyReadFailure{Memory: store, err: readErr}})
			err := d.InjectChannelPolicy(ctx, InjectChannelPolicyRequest{
				WorkerName: "worker", GroupAllowFrom: []string{"@alice:local"}, DMAllowFrom: []string{"@alice:local"},
			})
			if !errors.Is(err, readErr) {
				t.Errorf("error=%v, want original storage error %v", err, readErr)
			}
			data, err := store.GetObject(ctx, key)
			if missing {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("published incomplete config before provisioning: %s, error=%v", data, err)
				}
			} else if err != nil || string(data) != full {
				t.Fatalf("overwrote unread config: %s, error=%v", data, err)
			}
		})
	}
}
