package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestAPIClient_DoesNotSendClusterIDHeader verifies that the OSS API client
// does not expose the remote-cluster routing header.
func TestAPIClient_DoesNotSendClusterIDHeader(t *testing.T) {
	var receivedHeaders http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := &APIClient{
		BaseURL:    ts.URL,
		Token:      "test-token",
		HTTPClient: ts.Client(),
	}

	resp, err := client.Do("GET", "/api/test", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if got := receivedHeaders.Get("X-AgentTeams-Cluster-ID"); got != "" {
		t.Fatalf("expected no X-AgentTeams-Cluster-ID header, got %q", got)
	}

	// Verify Authorization header is also present.
	authHeader := receivedHeaders.Get("Authorization")
	if authHeader != "Bearer test-token" {
		t.Fatalf("expected Authorization=Bearer test-token, got %q", authHeader)
	}
}

func TestNewAPIClientPrefersAgentTeamsControllerURL(t *testing.T) {
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", "http://agentteams-controller:8090")
	t.Setenv("HICLAW_CONTROLLER_URL", "http://legacy-controller:8090")

	client := NewAPIClient()
	if client.BaseURL != "http://agentteams-controller:8090" {
		t.Fatalf("BaseURL=%q, want AgentTeams controller URL", client.BaseURL)
	}
}

func TestNewAPIClientIgnoresLegacyControllerURL(t *testing.T) {
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", "")
	t.Setenv("HICLAW_CONTROLLER_URL", "http://legacy-controller:8090")

	client := NewAPIClient()
	if client.BaseURL != "http://localhost:8090" {
		t.Fatalf("BaseURL=%q, want default without HICLAW fallback", client.BaseURL)
	}
}

func TestDiscoverTokenPrefersAgentTeamsEnv(t *testing.T) {
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "agentteams-token")
	t.Setenv("HICLAW_AUTH_TOKEN", "legacy-token")

	if got := discoverToken(); got != "agentteams-token" {
		t.Fatalf("discoverToken=%q, want AgentTeams env token", got)
	}
}

func TestDiscoverTokenIgnoresLegacyEnvFallback(t *testing.T) {
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "")
	t.Setenv("AGENTTEAMS_AUTH_TOKEN_FILE", "")
	t.Setenv("HICLAW_AUTH_TOKEN", "legacy-token")

	if got := discoverToken(); got != "" {
		t.Fatalf("discoverToken=%q, want empty without HICLAW fallback", got)
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

func TestDiscoverTokenIgnoresLegacyFileFallback(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("legacy-file-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "")
	t.Setenv("AGENTTEAMS_AUTH_TOKEN_FILE", "")
	t.Setenv("HICLAW_AUTH_TOKEN", "")
	t.Setenv("HICLAW_AUTH_TOKEN_FILE", tokenFile)

	if got := discoverToken(); got != "" {
		t.Fatalf("discoverToken=%q, want empty without HICLAW file fallback", got)
	}
}
