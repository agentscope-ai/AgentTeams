package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseExposePortsProducesNumericJSONValues(t *testing.T) {
	ports, err := parseExposePorts("8080, 3000")
	if err != nil {
		t.Fatalf("parseExposePorts: %v", err)
	}
	payload, err := json.Marshal(ports)
	if err != nil {
		t.Fatalf("marshal expose ports: %v", err)
	}

	var decoded []struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("expose payload cannot be decoded by the controller API: %v (payload: %s)", err, payload)
	}
	if len(decoded) != 2 || decoded[0].Port != 8080 || decoded[1].Port != 3000 {
		t.Fatalf("decoded expose ports = %#v, want [8080 3000]", decoded)
	}
}

func TestParseExposePortsAcceptsBoundaryValues(t *testing.T) {
	ports, err := parseExposePorts("1,65535")
	if err != nil {
		t.Fatalf("parseExposePorts: %v", err)
	}
	if len(ports) != 2 || ports[0]["port"] != 1 || ports[1]["port"] != 65535 {
		t.Fatalf("parseExposePorts returned %#v, want ports 1 and 65535", ports)
	}
}

func TestParseExposePortsRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "   ", "not-a-port", "-1", "0", "65536", ","} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseExposePorts(value); err == nil {
				t.Fatalf("parseExposePorts(%q) returned nil error", value)
			}
		})
	}
}

func TestParseExposePortsRejectsEmptyCSVSegments(t *testing.T) {
	for _, value := range []string{"8080,", ",8080", "8080,,3000", "8080, ,3000"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseExposePorts(value); err == nil {
				t.Fatalf("parseExposePorts(%q) returned nil error", value)
			}
		})
	}
}

func TestParseExposePortsRejectsDuplicatePorts(t *testing.T) {
	for _, value := range []string{"8080,8080", "8080, 8080", "08080,8080"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseExposePorts(value); err == nil {
				t.Fatalf("parseExposePorts(%q) returned nil error", value)
			}
		})
	}
}

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

func TestCreateRuntimeHelpKeepsCoPawDuringQwenPawTransition(t *testing.T) {
	for name, usage := range map[string]string{
		"worker":  createWorkerCmd().Flags().Lookup("runtime").Usage,
		"manager": createManagerCmd().Flags().Lookup("runtime").Usage,
	} {
		for _, runtime := range []string{"copaw", "qwenpaw"} {
			if !strings.Contains(usage, runtime) {
				t.Errorf("%s runtime help %q does not include %q", name, usage, runtime)
			}
		}
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

func TestGetTeamWorkerStatusesMatchesSingleWorkerStatus(t *testing.T) {
	const statusJSON = `{
		"name":"alpha-dev",
		"phase":"Running",
		"containerState":"running",
		"runtime":"copaw",
		"team":"alpha-team",
		"role":"worker",
		"roomID":"!personal:matrix.org",
		"matrixUserID":"@alpha-dev:matrix.org",
		"message":"backend=stub status=running message=healthy"
	}`
	client := &APIClient{
		BaseURL: "http://controller.test",
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.URL.Path == "/api/v1/workers" && r.URL.Query().Get("team") == "alpha-team":
					return jsonResponse(http.StatusOK, `{"workers":[{"name":"alpha-dev","phase":"Running","team":"alpha-team","role":"worker"}],"total":1}`), nil
				case r.URL.Path == "/api/v1/workers/alpha-dev/status":
					return jsonResponse(http.StatusOK, statusJSON), nil
				default:
					return jsonResponse(http.StatusNotFound, `{"error":"not found"}`), nil
				}
			}),
			Timeout: 5 * time.Second,
		},
	}

	var byName workerResp
	if err := client.DoJSON("GET", "/api/v1/workers/alpha-dev/status", nil, &byName); err != nil {
		t.Fatalf("load --name response: %v", err)
	}
	byTeam, err := getTeamWorkerStatuses(client, "alpha-team")
	if err != nil {
		t.Fatalf("load --team response: %v", err)
	}
	if byTeam.Total != 1 || len(byTeam.Workers) != 1 {
		t.Fatalf("unexpected --team response: %+v", byTeam)
	}
	if !reflect.DeepEqual(byTeam.Workers[0], byName) {
		t.Fatalf("--team worker differs from --name response:\nteam=%+v\nname=%+v", byTeam.Workers[0], byName)
	}
	if byTeam.Workers[0].RoomID != "!personal:matrix.org" {
		t.Fatalf("roomID=%q, want personal communication room", byTeam.Workers[0].RoomID)
	}
	if byTeam.Workers[0].Message == "" {
		t.Fatal("--team JSON omitted runtime message")
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
