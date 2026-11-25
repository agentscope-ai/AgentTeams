package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestVersionEndpointReportsBuildVersion pins that /api/v1/version returns
// the version threaded through ServerDeps (stamped into main.version at
// build time) instead of the historical hardcoded "dev" (issue #1707).
func TestVersionEndpointReportsBuildVersion(t *testing.T) {
	h := NewStatusHandler(nil, "default", "embedded", "1.2.9-test")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	rec := httptest.NewRecorder()

	h.Version(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp VersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Controller != "1.2.9-test" {
		t.Fatalf("expected controller version %q, got %q", "1.2.9-test", resp.Controller)
	}
	if resp.KubeMode != "embedded" {
		t.Fatalf("expected kubeMode embedded, got %q", resp.KubeMode)
	}
}

// TestVersionEndpointFallbackDev pins the historical value for handlers built
// without an explicit version (test-rig ServerDeps leave deps.Version empty).
func TestVersionEndpointFallbackDev(t *testing.T) {
	h := NewStatusHandler(nil, "default", "incluster", "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	rec := httptest.NewRecorder()

	h.Version(rec, req)

	var resp VersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Controller != "dev" {
		t.Fatalf("expected fallback controller version dev, got %q", resp.Controller)
	}
}
