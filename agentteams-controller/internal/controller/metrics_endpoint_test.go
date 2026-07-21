package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appmetrics "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestMetricsEndpointIncludesAgentTeamsAndRuntimeMetrics(t *testing.T) {
	appmetrics.ReconcileTotal.Reset()
	appmetrics.ReconcileErrors.Reset()
	appmetrics.ReconcileDuration.Reset()
	appmetrics.ControllerCRCount.Reset()
	appmetrics.ControllerHTTPRequestDuration.Reset()
	appmetrics.ControllerHTTPRequests.Reset()
	appmetrics.ControllerHTTPErrors.Reset()
	appmetrics.UpstreamRequestDuration.Reset()
	appmetrics.UpstreamRequests.Reset()
	appmetrics.UpstreamRequestErrors.Reset()

	appmetrics.Observe("worker", time.Now().Add(-25*time.Millisecond), nil)
	appmetrics.Observe("team", time.Now().Add(-10*time.Millisecond), io.ErrUnexpectedEOF)
	appmetrics.ControllerCRCount.WithLabelValues("worker", "Running").Set(2)
	appmetrics.ObserveControllerHTTP(http.MethodGet, "/api/v1/workers/{name}", time.Now().Add(-3*time.Millisecond), http.StatusNotFound)
	appmetrics.ObserveUpstream("matrix", "create_room", time.Now().Add(-5*time.Millisecond), http.StatusCreated, nil)

	server := httptest.NewServer(promhttp.HandlerFor(crmetrics.Registry, promhttp.HandlerOpts{}))
	defer server.Close()

	resp, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatalf("scrape metrics endpoint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(bodyBytes)

	for _, want := range []string{
		"agentteams_reconcile_total",
		"agentteams_reconcile_errors_total",
		"agentteams_reconcile_duration_seconds_bucket",
		"agentteams_controller_cr_count",
		"status=\"Running\"",
		"agentteams_controller_http_request_duration_seconds_bucket",
		"agentteams_controller_http_requests_total",
		"agentteams_controller_http_errors_total",
		"route=\"/api/v1/workers/{name}\"",
		"agentteams_upstream_request_duration_seconds_bucket",
		"agentteams_upstream_requests_total",
		"upstream=\"matrix\"",
		"operation=\"create_room\"",
		"go_goroutines",
		"go_gc_duration_seconds",
		"go_memstats_last_gc_time_seconds",
		"process_resident_memory_bytes",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q", want)
		}
	}

	for _, forbidden := range []string{
		"hiclaw_reconcile_total",
		"agentteams_team_migration_phase_total",
		"agentteams_team_migration_failures_total",
		"team=\"",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics output contains forbidden text %q", forbidden)
		}
	}
}
