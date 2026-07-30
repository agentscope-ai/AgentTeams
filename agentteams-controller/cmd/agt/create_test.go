package main

import (
	"bytes"
	"io"
	"net/http"
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
