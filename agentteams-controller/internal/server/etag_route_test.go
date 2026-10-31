package server

import (
	"context"
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

// etagDo performs an authenticated GET through the real mux.
func etagDo(t *testing.T, srv *HTTPServer, path, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer sa-token")
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	return rec
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
