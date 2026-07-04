package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserve_SuccessIncrementsSuccessSeries(t *testing.T) {
	ReconcileTotal.Reset()
	ReconcileErrors.Reset()

	Observe("worker", time.Now().Add(-10*time.Millisecond), nil)

	if got := testutil.ToFloat64(ReconcileTotal.WithLabelValues("worker", "success")); got != 1 {
		t.Errorf("success counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(ReconcileTotal.WithLabelValues("worker", "error")); got != 0 {
		t.Errorf("error counter must stay at 0 on success, got %v", got)
	}
	if got := testutil.ToFloat64(ReconcileErrors.WithLabelValues("worker")); got != 0 {
		t.Errorf("ReconcileErrors must stay at 0 on success, got %v", got)
	}
}

func TestObserve_ErrorIncrementsBothErrorSeries(t *testing.T) {
	ReconcileTotal.Reset()
	ReconcileErrors.Reset()

	Observe("team", time.Now(), errors.New("boom"))

	if got := testutil.ToFloat64(ReconcileTotal.WithLabelValues("team", "error")); got != 1 {
		t.Errorf("reconcile_total{result=error} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(ReconcileErrors.WithLabelValues("team")); got != 1 {
		t.Errorf("reconcile_errors_total = %v, want 1", got)
	}
}

func TestObserveUpstream_HTTPErrorIncrementsErrorSeries(t *testing.T) {
	UpstreamRequestDuration.Reset()
	UpstreamRequests.Reset()
	UpstreamRequestErrors.Reset()

	ObserveUpstream("matrix", "create_room", time.Now(), 500, nil)

	if got := testutil.ToFloat64(UpstreamRequests.WithLabelValues("matrix", "create_room", "error", "5xx")); got != 1 {
		t.Errorf("upstream_requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(UpstreamRequestErrors.WithLabelValues("matrix", "create_room", "http")); got != 1 {
		t.Errorf("upstream_request_errors_total = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(UpstreamRequestDuration, "agentteams_upstream_request_duration_seconds"); got == 0 {
		t.Error("UpstreamRequestDuration histogram recorded no samples")
	}
}

func TestObserveControllerHTTP_RecordsRouteAndStatusClass(t *testing.T) {
	ControllerHTTPRequests.Reset()
	ControllerHTTPErrors.Reset()
	ControllerHTTPRequestDuration.Reset()

	ObserveControllerHTTP("GET", "/api/v1/workers/{name}", time.Now(), 404)

	if got := testutil.ToFloat64(ControllerHTTPRequests.WithLabelValues("GET", "/api/v1/workers/{name}", "error", "4xx")); got != 1 {
		t.Errorf("controller_http_requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(ControllerHTTPErrors.WithLabelValues("GET", "/api/v1/workers/{name}", "4xx")); got != 1 {
		t.Errorf("controller_http_errors_total = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(ControllerHTTPRequestDuration, "agentteams_controller_http_request_duration_seconds"); got == 0 {
		t.Error("ControllerHTTPRequestDuration histogram recorded no samples")
	}
}

func TestObserveUpstream_ClassifiesTimeout(t *testing.T) {
	UpstreamRequests.Reset()
	UpstreamRequestErrors.Reset()

	ObserveUpstream("matrix", "login", time.Now(), 0, context.DeadlineExceeded)

	if got := testutil.ToFloat64(UpstreamRequests.WithLabelValues("matrix", "login", "error", "none")); got != 1 {
		t.Errorf("upstream_requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(UpstreamRequestErrors.WithLabelValues("matrix", "login", "timeout")); got != 1 {
		t.Errorf("upstream_request_errors_total = %v, want 1", got)
	}
}

func TestObserveUpstream_ClassifiesInvalidResponse(t *testing.T) {
	UpstreamRequestErrors.Reset()

	ObserveUpstream("sts_provider", "issue_token", time.Now(), 200, ErrInvalidUpstreamResponse)

	if got := testutil.ToFloat64(UpstreamRequestErrors.WithLabelValues("sts_provider", "issue_token", "invalid_response")); got != 1 {
		t.Errorf("upstream_request_errors_total = %v, want 1", got)
	}
}

func TestObserve_RecordsDuration(t *testing.T) {
	ReconcileDuration.Reset()
	start := time.Now().Add(-50 * time.Millisecond)
	Observe("manager", start, nil)

	if got := testutil.CollectAndCount(ReconcileDuration); got == 0 {
		t.Error("ReconcileDuration histogram recorded no samples")
	}
}

func TestMetricsUseAgentTeamsNamespace(t *testing.T) {
	if got := testutil.CollectAndCount(ReconcileTotal, "agentteams_reconcile_total"); got == 0 {
		t.Fatal("agentteams_reconcile_total was not collected")
	}
}

func TestInitializeSeriesPublishesZeroValueMetrics(t *testing.T) {
	ReconcileTotal.Reset()
	ReconcileErrors.Reset()
	ReconcileDuration.Reset()
	initializeSeries()

	if got, want := testutil.CollectAndCount(ReconcileTotal, "agentteams_reconcile_total"), len(reconcileKinds)*len(reconcileResults); got != want {
		t.Fatalf("reconcile_total series count = %v, want %v", got, want)
	}
	if got, want := testutil.CollectAndCount(ReconcileErrors, "agentteams_reconcile_errors_total"), len(reconcileKinds); got != want {
		t.Fatalf("reconcile_errors_total series count = %v, want %v", got, want)
	}
	if got, want := testutil.CollectAndCount(ReconcileDuration, "agentteams_reconcile_duration_seconds"), len(reconcileKinds)*len(reconcileResults); got != want {
		t.Fatalf("reconcile_duration_seconds series count = %v, want %v", got, want)
	}
	if got, want := testutil.CollectAndCount(ControllerCRCount, "agentteams_controller_cr_count"), crStatusCount(); got != want {
		t.Fatalf("controller_cr_count series count = %v, want %v", got, want)
	}
	if got, want := testutil.CollectAndCount(ControllerHTTPRequestDuration, "agentteams_controller_http_request_duration_seconds"), httpRouteCount()*len(reconcileResults); got != want {
		t.Fatalf("controller_http_request_duration_seconds series count = %v, want %v", got, want)
	}
	if got, want := testutil.CollectAndCount(ControllerHTTPRequests, "agentteams_controller_http_requests_total"), httpRouteCount()*len(reconcileResults)*len(upstreamStatusClasses); got != want {
		t.Fatalf("controller_http_requests_total series count = %v, want %v", got, want)
	}
	if got, want := testutil.CollectAndCount(ControllerHTTPErrors, "agentteams_controller_http_errors_total"), httpRouteCount()*len(upstreamStatusClasses); got != want {
		t.Fatalf("controller_http_errors_total series count = %v, want %v", got, want)
	}
	if got, want := testutil.CollectAndCount(UpstreamRequestDuration, "agentteams_upstream_request_duration_seconds"), upstreamOperationCount()*len(reconcileResults); got != want {
		t.Fatalf("upstream_request_duration_seconds series count = %v, want %v", got, want)
	}
	if got, want := testutil.CollectAndCount(UpstreamRequests, "agentteams_upstream_requests_total"), upstreamOperationCount()*len(reconcileResults)*len(upstreamStatusClasses); got != want {
		t.Fatalf("upstream_requests_total series count = %v, want %v", got, want)
	}
	if got, want := testutil.CollectAndCount(UpstreamRequestErrors, "agentteams_upstream_request_errors_total"), upstreamOperationCount()*len(upstreamErrorClasses); got != want {
		t.Fatalf("upstream_request_errors_total series count = %v, want %v", got, want)
	}
}

func crStatusCount() int {
	count := 0
	for _, statuses := range crStatusesByKind {
		count += len(statuses)
	}
	return count
}

func httpRouteCount() int {
	count := 0
	for _, routes := range httpRoutesByMethod {
		count += len(routes)
	}
	return count
}

func upstreamOperationCount() int {
	count := 0
	for _, operations := range upstreamOpsByTarget {
		count += len(operations)
	}
	return count
}
