package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	appmetrics "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestControllerHTTPMetricsUseMuxRoutePattern(t *testing.T) {
	appmetrics.ControllerHTTPRequests.Reset()
	appmetrics.ControllerHTTPErrors.Reset()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/workers/{name}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev", nil)
	rec := httptest.NewRecorder()
	withControllerHTTPMetrics(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := testutil.ToFloat64(appmetrics.ControllerHTTPRequests.WithLabelValues("GET", "/api/v1/workers/{name}", "error", "4xx")); got != 1 {
		t.Fatalf("controller_http_requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(appmetrics.ControllerHTTPErrors.WithLabelValues("GET", "/api/v1/workers/{name}", "4xx")); got != 1 {
		t.Fatalf("controller_http_errors_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(appmetrics.ControllerHTTPRequests.WithLabelValues("GET", "/api/v1/workers/alpha-dev", "error", "4xx")); got != 0 {
		t.Fatalf("raw path label must not be used, got %v", got)
	}
}
