package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

type projectMetaReadRaceStore struct {
	oss.StorageClient
	key   string
	next  []byte
	armed bool
}

func (s *projectMetaReadRaceStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	data, err := s.StorageClient.GetObject(ctx, key)
	if err == nil && key == s.key && s.armed {
		s.armed = false
		if err := s.StorageClient.PutObject(ctx, key, s.next); err != nil {
			return nil, err
		}
	}
	return data, err
}

func TestGetProjectMetaETagMatchesBodyDuringConcurrentWrite(t *testing.T) {
	store := ossfake.NewMemory()
	key := metaCommitKey("alpha-team", "pm1")
	before := []byte(`{"project_id":"pm1","title":"before","status":"active","plan_type":"dag","team_id":"alpha-team"}`)
	next := []byte(`{"project_id":"pm1","title":"concurrent","status":"active","plan_type":"dag","team_id":"alpha-team"}`)
	if err := store.PutObject(context.Background(), key, before); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	// Resolution reads the prior project; the actual response read then
	// observes a concurrent update. Its validator must describe that body.
	h.oss = &projectMetaReadRaceStore{StorageClient: h.oss, key: key, next: next, armed: true}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/pm1/meta", nil)
	req.SetPathValue("id", "pm1")
	rec := httptest.NewRecorder()
	h.GetProjectMeta(rec, withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(next) {
		t.Fatalf("GET body = %q, want concurrent bytes %q", rec.Body.String(), next)
	}
	if got, want := rec.Header().Get("ETag"), contentETag(rec.Body.Bytes()); got != want {
		t.Fatalf("ETag %q does not match returned body %q", got, want)
	}
}
