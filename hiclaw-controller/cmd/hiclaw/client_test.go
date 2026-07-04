package main

import (
	"net/http"
	"net/http/httptest"
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

// TestDiscoverClusterID verifies the discoverClusterID function reads from env.
func TestDiscoverClusterID(t *testing.T) {
	t.Setenv("HICLAW_CLUSTER_ID", "env-cluster")
	got := discoverClusterID()
	if got != "env-cluster" {
		t.Fatalf("expected env-cluster, got %q", got)
	}
}

// TestDiscoverClusterID_Empty verifies empty env returns empty string.
func TestDiscoverClusterID_Empty(t *testing.T) {
	t.Setenv("HICLAW_CLUSTER_ID", "")
	got := discoverClusterID()
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}
