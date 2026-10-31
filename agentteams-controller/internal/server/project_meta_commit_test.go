package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests cover the conditional project meta commit endpoint (#1402),
// PUT /api/v1/projects/{id}/meta: the worker sync path commits the full
// meta.json against a strict If-Match CAS instead of a blind mc cp.

// roleAuthenticator is a fake Authenticator returning a fixed identity —
// the full-HTTP-chain tests need worker identities the way alwaysAdminAuth
// (project_handler_test.go) provides admin.
type roleAuthenticator struct{ identity *authpkg.CallerIdentity }

func (a *roleAuthenticator) Authenticate(_ context.Context, _ string) (*authpkg.CallerIdentity, error) {
	return a.identity, nil
}

// noStrictConditionalStore promotes only the StorageClient methods of the
// embedded client: PutObjectIfMatchStrict is NOT in the promoted set, so it
// stands in for a backend without a strict conditional write (the 503
// path). Distinct from weakStore in loop_handler_test.go (which promotes
// the strict write but always returns precondition-failed).
type noStrictConditionalStore struct {
	oss.StorageClient
}

func metaCommitKey(teamID, projectID string) string {
	if teamID == "" {
		return "shared/projects/" + projectID + "/meta.json"
	}
	return "teams/" + teamID + "/shared/projects/" + projectID + "/meta.json"
}

func TestPutProjectMeta_MissingIfMatch428(t *testing.T) {
	store := ossfake.NewMemory()
	key := metaCommitKey("alpha-team", "pm1")
	original := []byte(`{"project_id":"pm1","title":"PM1","status":"active","plan_type":"dag","team_id":"alpha-team"}`)
	if err := store.PutObject(context.Background(), key, original); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	cases := []struct {
		name  string
		hdr   string
		set   bool
	}{
		{"absent", "", false},
		{"blank", "   ", true},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/pm1/meta", strings.NewReader(`{"project_id":"pm1"}`))
		req.SetPathValue("id", "pm1")
		if tc.set {
			req.Header.Set("If-Match", tc.hdr)
		}
		req = withCaller(req, admin)
		rec := httptest.NewRecorder()
		h.PutProjectMeta(rec, req)
		if rec.Code != http.StatusPreconditionRequired {
			t.Fatalf("%s: status=%d, want 428; body=%s", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "If-Match") {
			t.Fatalf("%s: body must explain the missing If-Match: %s", tc.name, rec.Body.String())
		}
	}

	// 428 happens before any write: the object is byte-identical.
	got, err := store.GetObject(context.Background(), key)
	if err != nil || string(got) != string(original) {
		t.Fatalf("object changed after 428: %q (err=%v)", got, err)
	}
}

func TestPutProjectMeta_BodyValidation400(t *testing.T) {
	store := ossfake.NewMemory()
	key := metaCommitKey("alpha-team", "pm1")
	original := []byte(`{"project_id":"pm1","title":"PM1","status":"active","plan_type":"dag","team_id":"alpha-team"}`)
	if err := store.PutObject(context.Background(), key, original); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	badBodies := []string{
		`{"project_id":`,    // truncated JSON
		`[1, 2, 3]`,         // JSON array, not an object
		`"just a string"`,   // JSON scalar
		``,                  // empty body
	}
	for _, body := range badBodies {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/pm1/meta", strings.NewReader(body))
		req.SetPathValue("id", "pm1")
		req.Header.Set("If-Match", read.ETag)
		req = withCaller(req, admin)
		rec := httptest.NewRecorder()
		h.PutProjectMeta(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status=%d, want 400; body=%s", body, rec.Code, rec.Body.String())
		}
	}

	got, err := store.GetObject(context.Background(), key)
	if err != nil || string(got) != string(original) {
		t.Fatalf("object changed after 400: %q (err=%v)", got, err)
	}
}

func TestPutProjectMeta_BodyTooLarge413(t *testing.T) {
	store := ossfake.NewMemory()
	key := metaCommitKey("alpha-team", "pm1")
	if err := store.PutObject(context.Background(), key, []byte(`{"project_id":"pm1","status":"active","plan_type":"dag","team_id":"alpha-team"}`)); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}

	// Total body > 1 MiB (the MaxBytesReader limit).
	body := `{"padding":"` + strings.Repeat("a", 1<<20) + `"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/pm1/meta", strings.NewReader(body))
	req.SetPathValue("id", "pm1")
	req.Header.Set("If-Match", read.ETag)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PutProjectMeta(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutProjectMeta_CommitReturnsNewETag(t *testing.T) {
	store := ossfake.NewMemory()
	key := metaCommitKey("alpha-team", "pm1")
	original := []byte(`{"project_id":"pm1","title":"PM1","status":"active","plan_type":"dag","team_id":"alpha-team"}`)
	if err := store.PutObject(context.Background(), key, original); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}

	// Commit body with deliberate spacing: a re-marshal would normalize
	// it, so byte-equality with the stored object proves the raw bytes
	// are written through.
	body := []byte(`{"project_id": "pm1", "title": "PM1", "status": "active", "plan_type": "dag", "team_id": "alpha-team", "note": "committed-bytes"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/pm1/meta", strings.NewReader(string(body)))
	req.SetPathValue("id", "pm1")
	req.Header.Set("If-Match", read.ETag)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.PutProjectMeta(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["etag"] != contentETag(body) {
		t.Fatalf("response etag=%v, want %s (MD5 of the committed bytes)", resp["etag"], contentETag(body))
	}
	stored, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(body) {
		t.Fatalf("stored bytes differ from commit body:\n stored=%q\n commit=%q", stored, body)
	}
	stat, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if stat.ETag != resp["etag"] {
		t.Fatalf("re-stat etag=%s, response etag=%v", stat.ETag, resp["etag"])
	}
}

func TestPutProjectMeta_ConflictKeepsConcurrentWrite(t *testing.T) {
	store := ossfake.NewMemory()
	key := metaCommitKey("alpha-team", "pm1")
	original := []byte(`{"project_id":"pm1","title":"PM1","status":"active","plan_type":"dag","team_id":"alpha-team"}`)
	if err := store.PutObject(context.Background(), key, original); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}

	// A concurrent writer (the Go pause handler) commits a pause between
	// the client's read and its commit.
	concurrent := []byte(`{"project_id":"pm1","title":"PM1","status":"paused","plan_type":"dag","team_id":"alpha-team","loop":{"pause_sources":["project"]}}`)
	if err := store.PutObject(context.Background(), key, concurrent); err != nil {
		t.Fatal(err)
	}

	commit := []byte(`{"project_id":"pm1","title":"PM1","status":"active","plan_type":"dag","team_id":"alpha-team","synced":true}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/pm1/meta", strings.NewReader(string(commit)))
	req.SetPathValue("id", "pm1")
	req.Header.Set("If-Match", read.ETag) // stale etag
	req = withCaller(req, worker)
	rec := httptest.NewRecorder()
	h.PutProjectMeta(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale commit status=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["code"] != "conflict" || resp["message"] != "project meta changed since the read; re-read, merge and retry" {
		t.Fatalf("409 body=%v, want code=conflict + re-read/merge/retry message", resp)
	}
	got, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(concurrent) {
		t.Fatalf("concurrent write was clobbered: %q", got)
	}

	// The client re-reads, merges and retries with the fresh ETag.
	fresh, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	merged := []byte(`{"project_id":"pm1","title":"PM1","status":"paused","plan_type":"dag","team_id":"alpha-team","loop":{"pause_sources":["project"]},"synced":true}`)
	req2 := httptest.NewRequest(http.MethodPut, "/api/v1/projects/pm1/meta", strings.NewReader(string(merged)))
	req2.SetPathValue("id", "pm1")
	req2.Header.Set("If-Match", fresh.ETag)
	req2 = withCaller(req2, worker)
	rec2 := httptest.NewRecorder()
	h.PutProjectMeta(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry status=%d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
	got2, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != string(merged) {
		t.Fatalf("retry did not land: %q", got2)
	}
}

func TestPutProjectMeta_WorkerTeamScope(t *testing.T) {
	store := ossfake.NewMemory()
	key := metaCommitKey("alpha-team", "pm1")
	teamMeta := []byte(`{"project_id":"pm1","status":"active","plan_type":"dag","team_id":"alpha-team"}`)
	if err := store.PutObject(context.Background(), key, teamMeta); err != nil {
		t.Fatal(err)
	}
	// A global-prefix project resolves to team "" — unreachable by workers.
	globalKey := metaCommitKey("", "pg")
	globalMeta := []byte(`{"project_id":"pg","status":"active","plan_type":"dag"}`)
	if err := store.PutObject(context.Background(), globalKey, globalMeta); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	etag, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	globalEtag, err := store.StatMeta(context.Background(), globalKey)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		id      string
		ifMatch string
		caller  *authpkg.CallerIdentity
		want    int
	}{
		{"same-team", "pm1", etag.ETag,
			&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}, http.StatusOK},
		{"cross-team", "pm1", etag.ETag,
			&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "beta-dev", Team: "beta-team"}, http.StatusForbidden},
		{"teamless", "pm1", etag.ETag,
			&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "standalone-dev"}, http.StatusForbidden},
		{"global-project", "pg", globalEtag.ETag,
			&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/"+tc.id+"/meta", strings.NewReader(`{"project_id":"`+tc.id+`"}`))
		req.SetPathValue("id", tc.id)
		req.Header.Set("If-Match", tc.ifMatch)
		req = withCaller(req, tc.caller)
		rec := httptest.NewRecorder()
		h.PutProjectMeta(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s: status=%d, want %d; body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	// Only the same-team case may have written: the team project holds the
	// committed bytes, the global project is untouched.
	got, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"project_id":"pm1"}` {
		t.Fatalf("same-team commit bytes differ: %q", got)
	}
	gotGlobal, err := store.GetObject(context.Background(), globalKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotGlobal) != string(globalMeta) {
		t.Fatalf("global project was written by a worker: %q", gotGlobal)
	}
}

func TestPutProjectMeta_BackendWithoutStrictWrites503(t *testing.T) {
	store := ossfake.NewMemory()
	// newProjectTestHandlerWithOSS builds the k8s client with NO Team CRs,
	// so teamProjectPrefixes only scans the global prefix — seed the
	// project there and use an admin caller to reach the backend-capability
	// check (a worker would hit the 403 team boundary first).
	key := metaCommitKey("", "pm1")
	original := []byte(`{"project_id":"pm1","status":"active","plan_type":"dag"}`)
	if err := store.PutObject(context.Background(), key, original); err != nil {
		t.Fatal(err)
	}
	o := &noStrictConditionalStore{oss.StorageClient: &mcLikeOSS{Memory: store}}
	h := newProjectTestHandlerWithOSS(t, o)
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/pm1/meta", strings.NewReader(`{"project_id":"pm1","status":"active","plan_type":"dag"}`))
	req.SetPathValue("id", "pm1")
	req.Header.Set("If-Match", read.ETag)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PutProjectMeta(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	// No fallback write happened.
	got, err := store.GetObject(context.Background(), key)
	if err != nil || string(got) != string(original) {
		t.Fatalf("non-strict backend wrote the object: %q (err=%v)", got, err)
	}
}

// TestHTTPServer_RegistersProjectMetaCommitRoute proves the route is wired
// into the real Mux: a PUT without If-Match reaches the handler (428, not
// 404) and a GET on the same path is a method mismatch (405, not 404) —
// the pattern binds PUT only.
func TestHTTPServer_RegistersProjectMetaCommitRoute(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).Build()
	var ossStore oss.StorageClient = &mcLikeOSS{Memory: store}
	srv := NewHTTPServer(":0", ServerDeps{
		Client:    k8s,
		Namespace: "default",
		OSS:       ossStore,
		AuthMw:    authpkg.NewMiddleware(nil, nil, nil, nil, ""),
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/p1/meta", strings.NewReader(`{"project_id":"p1"}`))
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("PUT meta status=%d, want 428 (route registered, handler reached); body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/meta", nil)
	rec2 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET meta status=%d, want 405 (PUT-only binding); body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestPutProjectMeta_WorkerHTTPChain pins the authorization boundary end
// to end through the full middleware chain: a worker may commit its own
// team's project meta (the single project-kind write opened to workers),
// but POST pause is STILL 403 — the regression nail that "project" never
// entered authorizeWorker.
func TestPutProjectMeta_WorkerHTTPChain(t *testing.T) {
	store := ossfake.NewMemory()
	key := "teams/alpha-team/shared/projects/p1/meta.json"
	putProject(store, key, map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	k8s := fake.NewClientBuilder().
		WithScheme(newProjectTestScheme(t)).
		WithRuntimeObjects(team("alpha-team")).
		Build()
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	mw := authpkg.NewMiddleware(&roleAuthenticator{identity: worker}, nil, authpkg.NewAuthorizer(), k8s, "default")
	var ossStore oss.StorageClient = &mcLikeOSS{Memory: store}
	srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", OSS: ossStore, AuthMw: mw})

	// POST pause: still 403 — "project" Update stays closed to workers.
	reqPause := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", nil)
	reqPause.Header.Set("Authorization", "Bearer sa-token")
	recPause := httptest.NewRecorder()
	srv.Mux.ServeHTTP(recPause, reqPause)
	if recPause.Code != http.StatusForbidden {
		t.Fatalf("worker pause status=%d, want 403; body=%s", recPause.Code, recPause.Body.String())
	}

	// PUT meta: 200 through the full chain (authorizer "project-meta" +
	// handler same-team check), raw bytes landed.
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"project_id":"p1","title":"P1","status":"active","plan_type":"dag","team_id":"alpha-team","synced":true}`
	reqMeta := httptest.NewRequest(http.MethodPut, "/api/v1/projects/p1/meta", strings.NewReader(body))
	reqMeta.Header.Set("Authorization", "Bearer sa-token")
	reqMeta.Header.Set("If-Match", read.ETag)
	recMeta := httptest.NewRecorder()
	srv.Mux.ServeHTTP(recMeta, reqMeta)
	if recMeta.Code != http.StatusOK {
		t.Fatalf("worker meta commit status=%d, want 200; body=%s", recMeta.Code, recMeta.Body.String())
	}
	got, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("stored bytes differ from commit body:\n stored=%q\n commit=%q", got, body)
	}
}
