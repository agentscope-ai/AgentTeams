package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// etagRouteServer builds the production mux (NewHTTPServer) around a fake
// cluster so the tests exercise the exact route wiring of issue #1475
// PR-3: which routes are ETag-wrapped, and how fields=spec conditions the
// wrapper.
func etagRouteServer(t *testing.T, objs ...client.Object) (*HTTPServer, client.Client) {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(objs...).Build()
	mw := authpkg.NewMiddleware(&alwaysAdminAuth{}, authpkg.NewCREnricher(k8s, "default"), authpkg.NewAuthorizer(), k8s, "default")
	return NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", AuthMw: mw}), k8s
}

// etagDo performs an authenticated GET through the real mux as the admin
// (the "sa-token" every etagRouteServer accepts).
func etagDo(t *testing.T, srv *HTTPServer, path, ifNoneMatch string) *httptest.ResponseRecorder {
	return etagDoToken(t, srv, path, "sa-token", ifNoneMatch)
}

// etagDoToken performs an authenticated GET through the real mux with an
// explicit bearer token, so one server can serve several callers.
func etagDoToken(t *testing.T, srv *HTTPServer, path, token, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	return rec
}

// callerScopedAuth authenticates two callers with different visibility
// scopes (issue #1475 PR-3): "sa-token" is the admin (sees every worker)
// and "l2-token" is an L2 human scoped to alpha-team (sees only that
// team's workers).
type callerScopedAuth struct{}

func (a *callerScopedAuth) Authenticate(_ context.Context, token string) (*authpkg.CallerIdentity, error) {
	switch token {
	case "sa-token":
		return &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}, nil
	case "l2-token":
		return &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}, nil
	}
	return nil, errors.New("unknown token " + token)
}

// TestRouteETag_WorkersSpecView pins the route-level contract for
// GET /api/v1/workers?fields=spec (issue #1475 PR-3): the first request
// gets a quoted 32-hex ETag plus the caching headers; the same request
// replayed with If-None-Match gets 304 with an empty body; a
// status-only change (lastActiveAt) does not move the ETag; a spec change
// (model) does, and the stale validator then misses.
func TestRouteETag_WorkersSpecView(t *testing.T) {
	w := &v1beta1.Worker{}
	w.Name = "w1"
	w.Namespace = "default"
	w.Spec.WorkerName = "w1-display"
	w.Spec.Model = "model-a"
	w.Status.Phase = "Running"
	w.Status.LastActiveAt = "2026-10-31T00:00:00Z"

	srv, k8s := etagRouteServer(t, w)
	key := client.ObjectKey{Namespace: "default", Name: "w1"}

	rec1 := etagDo(t, srv, "/api/v1/workers?fields=spec", "")
	if rec1.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec1.Code, rec1.Body.String())
	}
	etagRe := regexp.MustCompile(`^"[0-9a-f]{32}"$`)
	etag1 := rec1.Header().Get("ETag")
	if !etagRe.MatchString(etag1) {
		t.Fatalf("ETag = %q, want a quoted 32-hex sha256-128", etag1)
	}
	if got := rec1.Header().Get("Vary"); got != "Authorization" {
		t.Errorf("Vary = %q, want Authorization", got)
	}
	if got := rec1.Header().Get("Cache-Control"); got != "private, max-age=0, must-revalidate" {
		t.Errorf("Cache-Control = %q, want the revalidate-only contract", got)
	}

	rec2 := etagDo(t, srv, "/api/v1/workers?fields=spec", etag1)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("replay status = %d, want 304 (body %s)", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 body = %q, want empty", rec2.Body.String())
	}
	if got := rec2.Header().Get("Content-Length"); got != "0" {
		t.Errorf("304 Content-Length = %q, want 0", got)
	}

	// Status-only change (lastActiveAt): the spec view is byte-identical,
	// so the ETag must not move and the cached validator still hits.
	fresh := &v1beta1.Worker{}
	if err := k8s.Get(context.Background(), key, fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Status.LastActiveAt = "2026-10-31T01:00:00Z"
	if err := k8s.Update(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	rec3 := etagDo(t, srv, "/api/v1/workers?fields=spec", "")
	if rec3.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec3.Code)
	}
	if got := rec3.Header().Get("ETag"); got != etag1 {
		t.Errorf("status-only change moved the spec ETag: %s -> %s", etag1, got)
	}
	rec4 := etagDo(t, srv, "/api/v1/workers?fields=spec", etag1)
	if rec4.Code != http.StatusNotModified {
		t.Errorf("stale-status replay status = %d, want 304", rec4.Code)
	}

	// Spec change (model): the ETag must move and the old validator must
	// miss (200 with the fresh body, not 304).
	if err := k8s.Get(context.Background(), key, fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Spec.Model = "model-b"
	if err := k8s.Update(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	rec5 := etagDo(t, srv, "/api/v1/workers?fields=spec", "")
	if rec5.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec5.Code)
	}
	etag2 := rec5.Header().Get("ETag")
	if etag2 == "" || etag2 == etag1 {
		t.Fatalf("spec change did not move the ETag (still %q)", etag1)
	}
	rec6 := etagDo(t, srv, "/api/v1/workers?fields=spec", etag1)
	if rec6.Code != http.StatusOK {
		t.Fatalf("stale validator after spec change: status = %d, want 200", rec6.Code)
	}
	if got := rec6.Header().Get("ETag"); got != etag2 {
		t.Errorf("stale-validator response ETag = %q, want the current %q", got, etag2)
	}
}

// TestRouteETag_WorkersVolatileViewsUnprotected pins the red line
// (issue #1475 PR-3): the default all view, fields=status, and
// fields=spec,status are all poll-volatile and must never carry an ETag —
// and a volatile view must not collapse into a 304 even when the client
// offers a validator.
func TestRouteETag_WorkersVolatileViewsUnprotected(t *testing.T) {
	w := &v1beta1.Worker{}
	w.Name = "w1"
	w.Namespace = "default"
	w.Spec.WorkerName = "w1-display"
	w.Spec.Model = "model-a"
	w.Status.Phase = "Running"
	w.Status.LastActiveAt = "2026-10-31T00:00:00Z"

	srv, _ := etagRouteServer(t, w)

	for _, query := range []string{"", "fields=all", "fields=status", "fields=spec,status"} {
		rec := etagDo(t, srv, "/api/v1/workers?"+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("?%s: status = %d, want 200 (body %s)", query, rec.Code, rec.Body.String())
		}
		if etag := rec.Header().Get("ETag"); etag != "" {
			t.Errorf("?%s: ETag %q present on a volatile view", query, etag)
		}
	}

	rec := etagDo(t, srv, "/api/v1/workers?fields=all", `"anything"`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fields=all with If-None-Match: status = %d, want 200", rec.Code)
	}
}

// TestRouteETag_StableEndpoints pins that the whitelisted stable
// endpoints (issue #1475 PR-3) each return a 200 with an ETag on the
// first request and a 304 with an empty body on the validated replay.
func TestRouteETag_StableEndpoints(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).Build()
	mw := authpkg.NewMiddleware(&alwaysAdminAuth{}, authpkg.NewCREnricher(k8s, "default"), authpkg.NewAuthorizer(), k8s, "default")
	// The ai-routes endpoint needs a gateway backend; the stub returns a
	// fixed catalog (same stub the handler tests use).
	srv := NewHTTPServer(":0", ServerDeps{
		Client:  k8s,
		Namespace: "default",
		AuthMw:  mw,
		Gateway: &aiRoutesListStub{routes: defaultDeploymentRoutes()},
	})

	etagRe := regexp.MustCompile(`^"[0-9a-f]{32}"$`)
	for _, path := range []string{
		"/api/v1/version",
		"/api/v1/skills",
		"/api/v1/mcp-servers",
		"/api/v1/gateway/ai-routes",
	} {
		t.Run(path, func(t *testing.T) {
			rec1 := etagDo(t, srv, path, "")
			if rec1.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec1.Code, rec1.Body.String())
			}
			etag := rec1.Header().Get("ETag")
			if !etagRe.MatchString(etag) {
				t.Fatalf("ETag = %q, want a quoted 32-hex sha256-128", etag)
			}
			if got := rec1.Header().Get("Vary"); got != "Authorization" {
				t.Errorf("Vary = %q, want Authorization", got)
			}
			rec2 := etagDo(t, srv, path, etag)
			if rec2.Code != http.StatusNotModified {
				t.Fatalf("replay status = %d, want 304 (body %s)", rec2.Code, rec2.Body.String())
			}
			if rec2.Body.Len() != 0 {
				t.Errorf("304 body = %q, want empty", rec2.Body.String())
			}
			if got := rec2.Header().Get("Content-Length"); got != "0" {
				t.Errorf("304 Content-Length = %q, want 0", got)
			}
		})
	}
}

// TestRouteETag_NotGlobal pins that the middleware stays off volatile and
// high-volume routes (issue #1475 PR-3): /status, the list endpoints
// without a spec projection, and the default workers view all answer 200
// without an ETag. (Teams / humans / managers get no ETag until their own
// spec projections land; ?fields=spec there is ignored by the handler, so
// their response is the full view.)
func TestRouteETag_NotGlobal(t *testing.T) {
	srv, _ := etagRouteServer(t)
	for _, path := range []string{
		"/api/v1/status",
		"/api/v1/teams",
		"/api/v1/humans",
		"/api/v1/managers",
		"/api/v1/workers",
		"/api/v1/workers?fields=spec", // control: this one MUST be protected
	} {
		rec := etagDo(t, srv, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", path, rec.Code, rec.Body.String())
		}
		etag := rec.Header().Get("ETag")
		wantETag := path == "/api/v1/workers?fields=spec"
		if wantETag && etag == "" {
			t.Errorf("%s: ETag missing, want a quoted 32-hex (control route)", path)
		}
		if !wantETag && etag != "" {
			t.Errorf("%s: ETag %q present on an unprotected route", path, etag)
		}
	}
}

// TestRouteETag_CallerScopedValidators pins the caller-scoping guarantee
// (issue #1475 PR-3): the validator is computed over the bytes THIS call
// serialized, so two callers with different visibility get different
// ETags; each caller's own validator still 304s, while the OTHER caller's
// validator misses (200 with the full body) — a 304 can only replay bytes
// the caller already holds, so no visibility difference can leak through
// a cross-caller match.
func TestRouteETag_CallerScopedValidators(t *testing.T) {
	alpha := team("alpha-team")
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "w1", Role: "worker"}}
	beta := team("beta-team")
	beta.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "w2", Role: "worker"}}
	w1 := &v1beta1.Worker{}
	w1.Name = "w1"
	w1.Namespace = "default"
	w1.Spec.WorkerName = "w1-display"
	w1.Spec.Model = "model-a"
	w2 := &v1beta1.Worker{}
	w2.Name = "w2"
	w2.Namespace = "default"
	w2.Spec.WorkerName = "w2-display"
	w2.Spec.Model = "model-b"

	k8s := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(alpha, beta, w1, w2).Build()
	mw := authpkg.NewMiddleware(&callerScopedAuth{}, authpkg.NewCREnricher(k8s, "default"), authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", AuthMw: mw})
	const path = "/api/v1/workers?fields=spec"

	admin := etagDoToken(t, srv, path, "sa-token", "")
	if admin.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200 (body %s)", admin.Code, admin.Body.String())
	}
	etagRe := regexp.MustCompile(`^"[0-9a-f]{32}"$`)
	etagAdmin := admin.Header().Get("ETag")
	if !etagRe.MatchString(etagAdmin) {
		t.Fatalf("admin ETag = %q, want a quoted 32-hex sha256-128", etagAdmin)
	}

	l2 := etagDoToken(t, srv, path, "l2-token", "")
	if l2.Code != http.StatusOK {
		t.Fatalf("L2 status = %d, want 200 (body %s)", l2.Code, l2.Body.String())
	}
	etagL2 := l2.Header().Get("ETag")
	if !etagRe.MatchString(etagL2) {
		t.Fatalf("L2 ETag = %q, want a quoted 32-hex sha256-128", etagL2)
	}

	// The two callers really see different representations: the admin
	// both workers, the L2 human only alpha-team's.
	var adminList, l2List workerListProjection
	if err := json.Unmarshal(admin.Body.Bytes(), &adminList); err != nil {
		t.Fatalf("decode admin list: %v", err)
	}
	if err := json.Unmarshal(l2.Body.Bytes(), &l2List); err != nil {
		t.Fatalf("decode L2 list: %v", err)
	}
	if adminList.Total != 2 || l2List.Total != 1 {
		t.Fatalf("visibility = admin %d / L2 %d workers, want 2 / 1", adminList.Total, l2List.Total)
	}
	if etagAdmin == etagL2 {
		t.Fatalf("different visibility scopes shared one ETag: %q", etagAdmin)
	}

	// Each caller's own validator still hits: 304 with an empty body.
	for _, tc := range []struct {
		name  string
		token string
		etag  string
	}{
		{"admin-replay", "sa-token", etagAdmin},
		{"l2-replay", "l2-token", etagL2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := etagDoToken(t, srv, path, tc.token, tc.etag)
			if rec.Code != http.StatusNotModified {
				t.Fatalf("status = %d, want 304 (body %s)", rec.Code, rec.Body.String())
			}
			if rec.Body.Len() != 0 {
				t.Errorf("304 body = %q, want empty", rec.Body.String())
			}
		})
	}

	// Cross-caller: the other caller's validator must MISS (200 with the
	// full body) — never a 304, which would hide the visibility difference.
	if rec := etagDoToken(t, srv, path, "sa-token", etagL2); rec.Code != http.StatusOK {
		t.Fatalf("admin with the L2 validator: status = %d, want 200 (a 304 would hide the wider view)", rec.Code)
	}
	if rec := etagDoToken(t, srv, path, "l2-token", etagAdmin); rec.Code != http.StatusOK {
		t.Fatalf("L2 with the admin validator: status = %d, want 200 (a 304 would leak the narrower view)", rec.Code)
	}
}
