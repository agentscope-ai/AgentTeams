package server

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestHumanPresignReleaseLimit(t *testing.T) {
	const capBytes = int64(64 << 20)
	for _, configured := range []string{"", "5368709120", "67108865", "1024"} {
		t.Run(configured, func(t *testing.T) {
			t.Setenv("HUMAN_FILE_PRESIGN_MAX_BYTES", configured)
			h, _, _ := newHumanFilesRig(t)
			pr := hfPresign(t, h, "alice", "limit.bin", "presign=1")
			want := capBytes
			if configured == "1024" {
				want = 1024
			}
			if pr.MaxBytes != want {
				t.Fatalf("configured %q: presign limit=%d, want %d", configured, pr.MaxBytes, want)
			}
		})
	}
}

type oversizedPresignStage struct {
	*ossfake.Memory
	key   string
	reads int
}

func (s *oversizedPresignStage) StatMeta(ctx context.Context, key string) (oss.ObjectMeta, error) {
	if key == s.key {
		return oss.ObjectMeta{Size: (64 << 20) + 1}, nil
	}
	return s.Memory.StatMeta(ctx, key)
}

func (s *oversizedPresignStage) GetObject(ctx context.Context, key string) ([]byte, error) {
	if key == s.key {
		s.reads++
	}
	return s.Memory.GetObject(ctx, key)
}

func TestHumanPresignReleaseLimitRejectsOversizedStageBeforeRead(t *testing.T) {
	t.Setenv("HUMAN_FILE_PRESIGN_MAX_BYTES", strconv.FormatInt(5<<30, 10))
	h, memory, _ := newHumanFilesRig(t)
	pr := hfPresign(t, h, "alice", "limit.bin", "presign=1")
	store := &oversizedPresignStage{Memory: memory, key: hfStagingKey(t, &pr)}
	h.oss = store
	rec := hfPostFile(t, h, "alice", "limit.bin/commit", hfSelf(), "")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("commit status=%d, want413: %s", rec.Code, rec.Body.String())
	}
	if store.reads != 0 {
		t.Fatalf("oversized staged bytes were read %d times", store.reads)
	}
	if got := hfGetUsed(t, h, "alice"); got != 0 {
		t.Fatalf("rejected upload charged quota: %d", got)
	}
}
