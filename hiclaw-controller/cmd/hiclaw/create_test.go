package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultWorkerModel(t *testing.T) {
	t.Run("falls back to qwen3.6-plus when env var unset", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_DEFAULT_MODEL", "")
		if got := defaultWorkerModel(); got != "qwen3.6-plus" {
			t.Fatalf("defaultWorkerModel() = %q, want qwen3.6-plus", got)
		}
	})
	t.Run("prefers AGENTTEAMS_DEFAULT_MODEL when set", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_DEFAULT_MODEL", "claude-sonnet-4-6")
		if got := defaultWorkerModel(); got != "claude-sonnet-4-6" {
			t.Fatalf("defaultWorkerModel() = %q, want claude-sonnet-4-6", got)
		}
	})
	t.Run("trims whitespace before falling back", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_DEFAULT_MODEL", "   ")
		if got := defaultWorkerModel(); got != "qwen3.6-plus" {
			t.Fatalf("defaultWorkerModel() = %q, want qwen3.6-plus", got)
		}
	})
}

func TestCreateTeamDefaultsLeaderAndWorkerModels(t *testing.T) {
	t.Setenv("AGENTTEAMS_DEFAULT_MODEL", "qwen3.7-max")

	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/teams" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"alpha"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)

	cmd := createTeamCmd()
	cmd.SetArgs([]string{
		"--name", "alpha",
		"--leader-name", "alpha-lead",
		"--workers", "alpha-dev,alpha-qa",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("create team command failed: %v", err)
	}

	if _, ok := got["leader"]; ok {
		t.Fatalf("unexpected legacy leader payload: %#v", got["leader"])
	}
	if _, ok := got["workers"]; ok {
		t.Fatalf("unexpected legacy workers payload: %#v", got["workers"])
	}

	members := got["members"].([]any)
	if len(members) != 3 {
		t.Fatalf("members len=%d, want 3", len(members))
	}
	leader := members[0].(map[string]any)
	if leader["name"] != "alpha-lead" {
		t.Fatalf("leader.name=%v, want alpha-lead", leader["name"])
	}
	if leader["role"] != "team_leader" {
		t.Fatalf("leader.role=%v, want team_leader", leader["role"])
	}
	if leader["model"] != "qwen3.7-max" {
		t.Fatalf("leader.model=%v, want qwen3.7-max", leader["model"])
	}
	if leader["runtime"] != "qwenpaw" {
		t.Fatalf("leader.runtime=%v, want qwenpaw", leader["runtime"])
	}
	for _, raw := range members[1:] {
		worker := raw.(map[string]any)
		if worker["role"] != "worker" {
			t.Fatalf("worker %v role=%v, want worker", worker["name"], worker["role"])
		}
		if worker["model"] != "qwen3.7-max" {
			t.Fatalf("worker %v model=%v, want qwen3.7-max", worker["name"], worker["model"])
		}
	}
}

func TestCreateTeamAcceptsExplicitLeaderRuntime(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"alpha"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)

	cmd := createTeamCmd()
	cmd.SetArgs([]string{
		"--name", "alpha",
		"--leader-name", "alpha-lead",
		"--leader-runtime", "copaw",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("create team command failed: %v", err)
	}

	members := got["members"].([]any)
	leader := members[0].(map[string]any)
	if leader["runtime"] != "copaw" {
		t.Fatalf("leader.runtime=%v, want copaw", leader["runtime"])
	}
}

func TestCreateTeamUsesDecoupledHeartbeatAndIdleTimeout(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"alpha"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)

	cmd := createTeamCmd()
	cmd.SetArgs([]string{
		"--name", "alpha",
		"--leader-name", "alpha-lead",
		"--workers", "alpha-dev",
		"--leader-heartbeat-every", "30m",
		"--worker-idle-timeout", "12h",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("create team command failed: %v", err)
	}

	if got["heartbeatEvery"] != "30m" {
		t.Fatalf("heartbeatEvery=%v, want 30m", got["heartbeatEvery"])
	}
	members := got["members"].([]any)
	leader := members[0].(map[string]any)
	if _, ok := leader["idleTimeout"]; ok {
		t.Fatalf("leader idleTimeout should be omitted, got %#v", leader["idleTimeout"])
	}
	worker := members[1].(map[string]any)
	if worker["idleTimeout"] != "12h" {
		t.Fatalf("worker idleTimeout=%v, want 12h", worker["idleTimeout"])
	}
}

func TestCreateTeamRejectsInvalidLeaderRuntime(t *testing.T) {
	cmd := createTeamCmd()
	cmd.SetArgs([]string{
		"--name", "alpha",
		"--leader-name", "alpha-lead",
		"--leader-runtime", "openclaw",
	})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--leader-runtime must be qwenpaw or copaw") {
		t.Fatalf("expected invalid leader runtime error, got %v", err)
	}
}

func TestWaitForWorkerReady(t *testing.T) {
	var calls int32
	client := &APIClient{
		BaseURL: "http://controller.test",
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/api/v1/workers/alice/status" {
					return jsonResponse(http.StatusNotFound, `{"error":"not found"}`), nil
				}
				call := atomic.AddInt32(&calls, 1)
				if call < 3 {
					return jsonResponse(http.StatusOK, `{"name":"alice","phase":"Running","containerState":"running"}`), nil
				}
				return jsonResponse(http.StatusOK, `{"name":"alice","phase":"Ready","containerState":"running"}`), nil
			}),
			Timeout: 5 * time.Second,
		},
	}

	resp, err := waitForWorkerReady(client, "alice", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForWorkerReady returned error: %v", err)
	}
	if resp.Phase != "Ready" {
		t.Fatalf("expected Ready phase, got %q", resp.Phase)
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Fatalf("expected multiple polls, got %d", calls)
	}
}

func TestWaitForWorkerReadyTimeout(t *testing.T) {
	client := &APIClient{
		BaseURL: "http://controller.test",
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, `{"name":"alice","phase":"Running","containerState":"running","message":"booting"}`), nil
			}),
			Timeout: 5 * time.Second,
		},
	}

	_, err := waitForWorkerReady(client, "alice", 1500*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "did not become ready") {
		t.Fatalf("expected timeout error, got %q", msg)
	}
	if !strings.Contains(msg, "phase=Running") {
		t.Fatalf("expected last phase in error, got %q", msg)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}
