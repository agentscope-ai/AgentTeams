package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

type taskMetaReadRaceStore struct {
	*ossfake.Memory
	key   string
	next  []byte
	armed bool
}

func (s *taskMetaReadRaceStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	data, err := s.Memory.GetObject(ctx, key)
	if err == nil && key == s.key && s.armed {
		s.armed = false
		if err := s.Memory.PutObject(ctx, key, s.next); err != nil {
			return nil, err
		}
	}
	return data, err
}

func TestGetTaskMetaETagMatchesBodyDuringConcurrentWrite(t *testing.T) {
	for _, duringRead := range []bool{false, true} {
		name := "write_after_response"
		if duringRead {
			name = "write_between_body_and_stat"
		}
		t.Run(name, func(t *testing.T) {
			store := ossfake.NewMemory()
			before := taskMetaBody(taskMeta(map[string]any{"progress": "before"}))
			concurrent := taskMetaBody(taskMeta(map[string]any{"progress": "concurrent"}))
			if err := store.PutObject(t.Context(), taskMetaKey, []byte(before)); err != nil {
				t.Fatal(err)
			}
			h := newProjectTestHandler(t, store, team("alpha-team"))
			if duringRead {
				h.oss = &taskMetaReadRaceStore{Memory: store, key: taskMetaKey, next: []byte(concurrent), armed: true}
			}
			caller := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/meta", nil)
			req.SetPathValue("id", "t1")
			rec := httptest.NewRecorder()
			h.GetTaskMeta(rec, withCaller(req, caller))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
			}
			if got, want := rec.Header().Get("ETag"), contentETag(rec.Body.Bytes()); got != want {
				t.Errorf("ETag %q does not match response body %q", got, want)
			}
			if !duringRead {
				if err := store.PutObject(t.Context(), taskMetaKey, []byte(concurrent)); err != nil {
					t.Fatal(err)
				}
			}
			var edit map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &edit); err != nil {
				t.Fatal(err)
			}
			edit["title"] = "user edit"
			put := putTaskMetaRequest(t, h, "t1", rec.Header().Get("ETag"), taskMetaBody(edit), caller)
			if put.Code != http.StatusConflict {
				t.Errorf("stale PUT: got %d, want 409: %s", put.Code, put.Body.String())
			}
			persisted, err := store.GetObject(t.Context(), taskMetaKey)
			if err != nil {
				t.Fatal(err)
			}
			if string(persisted) != concurrent {
				t.Errorf("concurrent write overwritten: %s", persisted)
			}
		})
	}
}
