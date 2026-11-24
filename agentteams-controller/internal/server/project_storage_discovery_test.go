package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

func TestProjectStorageFaultSpawnDiscovery(t *testing.T) {
	for _, tc := range []struct{ name, prefix, endpoint string }{
		{"list_chats_read", "agents/alpha-lead/" + workerChatsPath, "list"},
		{"messages_chats_read", "agents/alpha-lead/" + workerChatsPath, "messages"},
		{"messages_room_task_read", "teams/alpha-team/shared/tasks/", "messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store := spawnMsgEnv(t, makeHistoryDBBytes(t, []historyRow{
				{kind: "context_msg", role: "user", content: "synthetic review task", sessionID: "sub-3f2a9b1c"},
			}))
			healthy := spawnMessagesRequest(h, humanCaller(), "p1", "sub-3f2a9b1c", "")
			if healthy.Code != http.StatusOK {
				t.Fatalf("healthy control=%d body=%s", healthy.Code, healthy.Body.String())
			}
			h.oss = newProjectFaultOSS(t, store, tc.prefix, "GetObject")
			var rec *httptest.ResponseRecorder
			if tc.endpoint == "messages" {
				rec = spawnMessagesRequest(h, humanCaller(), "p1", "sub-3f2a9b1c", "")
			} else {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/spawns", nil)
				req.SetPathValue("id", "p1")
				req = withCaller(req, humanCaller())
				rec = httptest.NewRecorder()
				h.GetProjectSpawns(rec, req)
			}
			t.Logf("status=%d body=%s", rec.Code, rec.Body.String())
			assertStorageUnavailable(t, rec, tc.name)
		})
	}
}

type secondProjectReadFault struct {
	*mcLikeOSS
	key   string
	reads int
}

func (s *secondProjectReadFault) GetObject(ctx context.Context, key string) ([]byte, error) {
	if key == s.key {
		s.reads++
		if s.reads > 1 {
			return nil, fmt.Errorf("synthetic second read fault: %w", oss.ErrStorageUnavailable)
		}
	}
	return s.mcLikeOSS.GetObject(ctx, key)
}

func TestProjectStorageFaultGetProjectMetaSecondRead(t *testing.T) {
	h, store := spawnMsgEnv(t, nil)
	h.oss = &secondProjectReadFault{mcLikeOSS: &mcLikeOSS{Memory: store}, key: "teams/alpha-team/shared/projects/p1/meta.json"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/meta", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, humanCaller())
	rec := httptest.NewRecorder()
	h.GetProjectMeta(rec, req)
	t.Logf("status=%d body=%s", rec.Code, rec.Body.String())
	assertStorageUnavailable(t, rec, "second meta read")
}
