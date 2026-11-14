package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskForgeProxyForwarding(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotAuth, gotRequestID, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotAuth, gotRequestID = r.Header.Get("Authorization"), r.Header.Get("X-Request-Id")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	api := &internalAPI{
		taskforgeURL: upstream.URL, taskforgeToken: "tf-token", taskforgeEnabled: true,
		taskforgeClient: upstream.Client(),
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/taskforge/v1/tasks?limit=5", strings.NewReader(`{"title":"x"}`))
	req.SetPathValue("path", "v1/tasks")
	req.Header.Set("X-Request-Id", "req-proxy-1")
	rec := httptest.NewRecorder()
	api.proxyTaskForge(rec, req)

	if rec.Code != http.StatusCreated || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/tasks" || gotQuery != "limit=5" {
		t.Fatalf("upstream = %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if gotAuth != "Bearer tf-token" || gotRequestID != "req-proxy-1" || gotBody != `{"title":"x"}` {
		t.Fatalf("headers/body = auth=%q request=%q body=%q", gotAuth, gotRequestID, gotBody)
	}
}

func TestTaskForgeProxyErrorAndDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream failed"))
	}))
	api := &internalAPI{taskforgeURL: upstream.URL, taskforgeEnabled: true, taskforgeClient: upstream.Client()}
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/taskforge/v1/tasks", nil)
	req.SetPathValue("path", "v1/tasks")
	rec := httptest.NewRecorder()
	api.proxyTaskForge(rec, req)
	if rec.Code != http.StatusBadGateway || rec.Body.String() != "upstream failed" {
		t.Fatalf("upstream error response = %d %q", rec.Code, rec.Body.String())
	}
	upstream.Close()

	req = httptest.NewRequest(http.MethodGet, "/internal/v1/taskforge/v1/tasks", nil)
	req.SetPathValue("path", "v1/tasks")
	rec = httptest.NewRecorder()
	api.proxyTaskForge(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable status = %d, want 503", rec.Code)
	}

	disabled := &internalAPI{}
	req = httptest.NewRequest(http.MethodGet, "/internal/v1/taskforge/v1/tasks", nil)
	req.SetPathValue("path", "v1/tasks")
	rec = httptest.NewRecorder()
	disabled.proxyTaskForge(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled status = %d, want 503", rec.Code)
	}
}

// TestTaskForgeProxyResponseCap pins that an upstream response larger than
// the proxy cap is rejected with 502 instead of streamed without bound.
func TestTaskForgeProxyResponseCap(t *testing.T) {
	big := strings.Repeat("x", internalMaxBodyBytes+1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(big))
	}))
	defer upstream.Close()

	api := &internalAPI{taskforgeURL: upstream.URL, taskforgeEnabled: true, taskforgeClient: upstream.Client()}
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/taskforge/v1/big", nil)
	req.SetPathValue("path", "v1/big")
	rec := httptest.NewRecorder()
	api.proxyTaskForge(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("oversized response status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("oversized response body = %q", rec.Body.String())
	}
}

// TestTaskForgeProxyResponseCapBoundary pins the exact-cap edge: an upstream
// response of exactly internalMaxBodyBytes passes (the check is >, not >=).
// If the limit check ever flips to >=, this test goes red.
func TestTaskForgeProxyResponseCapBoundary(t *testing.T) {
	exact := strings.Repeat("y", internalMaxBodyBytes)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(exact))
	}))
	defer upstream.Close()

	api := &internalAPI{taskforgeURL: upstream.URL, taskforgeEnabled: true, taskforgeClient: upstream.Client()}
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/taskforge/v1/exact", nil)
	req.SetPathValue("path", "v1/exact")
	rec := httptest.NewRecorder()
	api.proxyTaskForge(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("exact-cap response status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != internalMaxBodyBytes {
		t.Fatalf("exact-cap response body len = %d, want %d", rec.Body.Len(), internalMaxBodyBytes)
	}
}

// TestTaskForgeProxyUpstreamReadError pins that a broken upstream body
// (declared Content-Length, truncated data) yields the 502 envelope with no
// stale upstream headers: the response must not carry the upstream
// Content-Length, or strict clients fail reading the error body (EOF).
func TestTaskForgeProxyUpstreamReadError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
	}))
	defer upstream.Close()

	api := &internalAPI{taskforgeURL: upstream.URL, taskforgeEnabled: true, taskforgeClient: upstream.Client()}
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/taskforge/v1/truncated", nil)
	req.SetPathValue("path", "v1/truncated")
	rec := httptest.NewRecorder()
	api.proxyTaskForge(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("truncated upstream status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upstream read failed") {
		t.Fatalf("truncated upstream body = %q", rec.Body.String())
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Fatalf("truncated upstream response carries stale Content-Length %q", cl)
	}
}
