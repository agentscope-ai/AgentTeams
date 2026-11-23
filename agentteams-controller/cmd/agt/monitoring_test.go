package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func monitoringCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := newRootCommand("agt")
	root.SilenceUsage, root.SilenceErrors = true, true
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func TestMonitoringCLIUsesOwnIdentityAndPreservesJSON(t *testing.T) {
	var request *http.Request
	payload := `{"items":[],"total":7,"page":2,"totalPages":4,"sampledAt":"2026-11-14T01:00:00Z","future_field":{"complete":false}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { request = r.Clone(r.Context()); w.Write([]byte(payload)) }))
	defer srv.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "")
	path := filepath.Join(t.TempDir(), "manager-token")
	if err := os.WriteFile(path, []byte("own-manager-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTTEAMS_AUTH_TOKEN_FILE", path)
	out, _, err := monitoringCommand(t, "monitoring", "--search", "worker prod", "--page", "2", "--limit", "2", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if request.Method != "GET" || request.URL.Path != "/api/v1/monitoring" || request.Header.Get("Authorization") != "Bearer own-manager-token" {
		t.Fatalf("wrong API/identity: %v", request)
	}
	if request.URL.Query().Get("search") != "worker prod" || request.URL.Query().Get("page") != "2" || request.URL.Query().Get("limit") != "2" {
		t.Fatalf("lost filters: %v", request.URL)
	}
	var actual, expected any
	json.Unmarshal([]byte(out), &actual)
	json.Unmarshal([]byte(payload), &expected)
	a, _ := json.Marshal(actual)
	b, _ := json.Marshal(expected)
	if !bytes.Equal(a, b) {
		t.Fatalf("JSON fields changed: %s", out)
	}
}

func TestMonitoringCLITableDistinguishesZeroUnknownAndQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"kind":"worker","name":"idle","availability":"available","metrics":{"state":"running","cpuPercent":0,"memoryUsageBytes":536870912,"memoryLimitBytes":null,"restartCount":1,"oomKilled":true}},{"kind":"worker","name":"missing","availability":"missing","reason":"Container does not exist","metrics":null}],"total":2,"page":1,"totalPages":1,"sampledAt":"2026-11-14T01:00:00Z"}`))
	}))
	defer srv.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
	out, _, err := monitoringCommand(t, "monitoring")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"0.00%", "512.0 MiB", "unlimited", "missing", "Container does not exist", "page 1/1", "2026-11-14T01:00:00Z"} {
		if !strings.Contains(out, value) {
			t.Errorf("missing %q: %s", value, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "missing") && strings.Contains(line, "0.00%") {
			t.Fatal("missing usage was turned into zero")
		}
	}
}

func TestLogsCLITimeScopeTextAndTruncation(t *testing.T) {
	var request *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = r.Clone(r.Context())
		w.Write([]byte(`{"text":"model timeout\nretry recovered\u001b[31m\r\n","tail":100,"sampledAt":"2026-11-14T01:00:00Z","truncated":true}`))
	}))
	defer srv.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "manager")
	out, stderr, err := monitoringCommand(t, "logs", "worker", "dev", "--since", "2026-11-14T09:00:00+08:00", "--tail", "100")
	if err != nil {
		t.Fatal(err)
	}
	if request.Method != "GET" || request.URL.Path != "/api/v1/monitoring/worker/dev/logs" || request.Header.Get("Authorization") != "Bearer manager" || request.URL.Query().Get("since") != "2026-11-14T01:00:00Z" || request.URL.Query().Get("tail") != "100" {
		t.Fatalf("wrong log query: %v", request)
	}
	if !strings.Contains(out, "model timeout\nretry recovered") || strings.ContainsAny(out, "\x1b\r") {
		t.Fatalf("unsafe or missing log text: %q", out)
	}
	if !strings.Contains(stderr, "truncated") || !strings.Contains(stderr, "2026-11-14T01:00:00Z") {
		t.Fatalf("lost coverage/freshness metadata: %s", stderr)
	}
	before := time.Now().Add(-15 * time.Minute)
	_, _, err = monitoringCommand(t, "logs", "component", "controller", "--since", "15m", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	since, err := time.Parse(time.RFC3339Nano, request.URL.Query().Get("since"))
	if err != nil || since.Before(before) || since.After(time.Now().Add(-15*time.Minute)) {
		t.Fatalf("wrong relative time: %s", request.URL)
	}
}

func TestMonitoringCLIRejectsInvalidArgumentsBeforeIO(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
	for _, args := range [][]string{{"monitoring", "--page", "0"}, {"monitoring", "--limit", "51"}, {"monitoring", "-o", "xml"}, {"logs", "host", "anything"}, {"logs", "worker", ".."}, {"logs", "worker", "dev/other"}, {"logs", "worker", "dev", "--tail", "1001"}, {"logs", "worker", "dev", "--tail", "0"}, {"logs", "worker", "dev", "--since", "yesterday"}, {"logs", "worker", "dev", "--since", "-1m"}, {"logs", "worker", "dev", "-o", "table"}} {
		if _, _, err := monitoringCommand(t, args...); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid options reached Controller %d times", calls)
	}
}

func TestMonitoringCLIPropagatesSourceAndPermissionErrors(t *testing.T) {
	for _, status := range []int{403, 404, 501, 502} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.Write([]byte(`{"message":"source unavailable"}`))
			}))
			defer srv.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
			for _, args := range [][]string{{"monitoring"}, {"logs", "manager", "default"}} {
				out, _, err := monitoringCommand(t, args...)
				var apiError *APIError
				if !errors.As(err, &apiError) || apiError.StatusCode != status || out != "" {
					t.Fatalf("failure became successful empty data: out=%s err=%v", out, err)
				}
			}
		})
	}
}
