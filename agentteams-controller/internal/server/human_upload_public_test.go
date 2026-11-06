package server

import (
	"context"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

type unsupportedUploadStore struct{ *ossfake.Memory }

func (*unsupportedUploadStore) PresignUpload(context.Context, string, int64, string, string, time.Duration) (string, string, map[string]string, error) {
	return "", "", nil, oss.ErrPresignUnsupported
}

func TestHumanUploadUnconfiguredPublicOriginIsCapabilityResponse(t *testing.T) {
	h, _, _ := newHumanFilesRig(t)
	h.oss = &unsupportedUploadStore{ossfake.NewMemory()}
	rec := hfPostFile(t, h, "alice", "a.txt", hfSelf(), "presign=1")
	if rec.Code != 501 {
		t.Fatalf("status=%d body=%s, want501 capability response", rec.Code, rec.Body.String())
	}
}
