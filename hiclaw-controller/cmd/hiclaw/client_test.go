package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestAPIClient_ClusterIDHeader verifies that the APIClient sends the
// X-HiClaw-Cluster-ID header when HICLAW_CLUSTER_ID is set.
func TestAPIClient_ClusterIDHeader(t *testing.T) {
	var receivedHeaders http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Test with ClusterID set.
	client := &APIClient{
		BaseURL:    ts.URL,
		Token:      "test-token",
		ClusterID:  "test-cluster",
		HTTPClient: ts.Client(),
	}

	resp, err := client.Do("GET", "/api/test", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	got := receivedHeaders.Get("X-HiClaw-Cluster-ID")
	if got != "test-cluster" {
		t.Fatalf("expected X-HiClaw-Cluster-ID=test-cluster, got %q", got)
	}

	// Verify Authorization header is also present.
	authHeader := receivedHeaders.Get("Authorization")
	if authHeader != "Bearer test-token" {
		t.Fatalf("expected Authorization=Bearer test-token, got %q", authHeader)
	}
}

// TestAPIClient_NoClusterIDHeader verifies that the X-HiClaw-Cluster-ID header
// is NOT sent when ClusterID is empty.
func TestAPIClient_NoClusterIDHeader(t *testing.T) {
	var receivedHeaders http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Test without ClusterID.
	client := &APIClient{
		BaseURL:    ts.URL,
		Token:      "test-token",
		ClusterID:  "",
		HTTPClient: ts.Client(),
	}

	resp, err := client.Do("GET", "/api/test", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	got := receivedHeaders.Get("X-HiClaw-Cluster-ID")
	if got != "" {
		t.Fatalf("expected no X-HiClaw-Cluster-ID header, got %q", got)
	}
}

func TestNewAPIClientPrefersAgentTeamsControllerURL(t *testing.T) {
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", "http://agentteams-controller:8090")
	t.Setenv("HICLAW_CONTROLLER_URL", "http://hiclaw-controller:8090")

	client := NewAPIClient()
	if client.BaseURL != "http://agentteams-controller:8090" {
		t.Fatalf("BaseURL=%q, want AgentTeams controller URL", client.BaseURL)
	}
}

func TestNewAPIClientUsesLegacyControllerURL(t *testing.T) {
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", "")
	t.Setenv("HICLAW_CONTROLLER_URL", "http://hiclaw-controller:8090")

	client := NewAPIClient()
	if client.BaseURL != "http://hiclaw-controller:8090" {
		t.Fatalf("BaseURL=%q, want legacy controller URL", client.BaseURL)
	}
}

// TestDiscoverClusterID verifies the discoverClusterID function reads from env.
func TestDiscoverClusterID(t *testing.T) {
	t.Setenv("AGENTTEAMS_CLUSTER_ID", "agentteams-cluster")
	t.Setenv("HICLAW_CLUSTER_ID", "env-cluster")
	got := discoverClusterID()
	if got != "agentteams-cluster" {
		t.Fatalf("expected agentteams-cluster, got %q", got)
	}
}

func TestDiscoverClusterIDLegacyFallback(t *testing.T) {
	t.Setenv("AGENTTEAMS_CLUSTER_ID", "")
	t.Setenv("HICLAW_CLUSTER_ID", "env-cluster")
	got := discoverClusterID()
	if got != "env-cluster" {
		t.Fatalf("expected env-cluster, got %q", got)
	}
}

// TestDiscoverClusterID_Empty verifies empty env returns empty string.
func TestDiscoverClusterID_Empty(t *testing.T) {
	t.Setenv("AGENTTEAMS_CLUSTER_ID", "")
	t.Setenv("HICLAW_CLUSTER_ID", "")
	got := discoverClusterID()
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestDiscoverTokenPrefersAgentTeamsEnv(t *testing.T) {
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "agentteams-token")
	t.Setenv("HICLAW_AUTH_TOKEN", "legacy-token")

	if got := discoverToken(); got != "agentteams-token" {
		t.Fatalf("discoverToken=%q, want AgentTeams env token", got)
	}
}

func TestDiscoverTokenLegacyEnvFallback(t *testing.T) {
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "")
	t.Setenv("AGENTTEAMS_AUTH_TOKEN_FILE", "")
	t.Setenv("HICLAW_AUTH_TOKEN", "legacy-token")

	if got := discoverToken(); got != "legacy-token" {
		t.Fatalf("discoverToken=%q, want legacy env token", got)
	}
}

func TestDiscoverTokenPrefersAgentTeamsFile(t *testing.T) {
	dir := t.TempDir()
	agentTeamsTokenFile := filepath.Join(dir, "agentteams-token")
	legacyTokenFile := filepath.Join(dir, "legacy-token")
	if err := os.WriteFile(agentTeamsTokenFile, []byte("agentteams-file-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyTokenFile, []byte("legacy-file-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTTEAMS_AUTH_TOKEN_FILE", agentTeamsTokenFile)
	t.Setenv("HICLAW_AUTH_TOKEN_FILE", legacyTokenFile)

	if got := discoverToken(); got != "agentteams-file-token" {
		t.Fatalf("discoverToken=%q, want AgentTeams file token", got)
	}
}

func TestDiscoverTokenLegacyFileFallback(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("legacy-file-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "")
	t.Setenv("AGENTTEAMS_AUTH_TOKEN_FILE", "")
	t.Setenv("HICLAW_AUTH_TOKEN", "")
	t.Setenv("HICLAW_AUTH_TOKEN_FILE", tokenFile)

	if got := discoverToken(); got != "legacy-file-token" {
		t.Fatalf("discoverToken=%q, want legacy file token", got)
	}
}
