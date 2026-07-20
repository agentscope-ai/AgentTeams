// Package metrics defines AgentTeams-specific Prometheus metrics and registers
// them with the controller-runtime metrics registry, which exposes them on the
// metrics endpoint configured by ctrl.Options.Metrics.BindAddress.
//
// controller-runtime already exports generic reconcile metrics. These
// agentteams_* metrics carry bounded AgentTeams dimensions such as CRD kind and
// outcome so dashboards do not need to parse controller names.
package metrics

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "agentteams"

// ErrDecodeResponse lets upstream clients mark response decode failures for
// error-class metrics without depending on error message text.
var ErrDecodeResponse = errors.New("decode response")

// ErrInvalidUpstreamResponse marks structurally valid responses that are
// unusable for the caller, for example a credential-provider response missing
// required credential fields.
var ErrInvalidUpstreamResponse = errors.New("invalid upstream response")

var (
	reconcileKinds   = []string{"worker", "team", "human", "manager"}
	reconcileResults = []string{"success", "error"}
	crStatusesByKind = map[string][]string{
		"human": {
			"unknown",
			"Pending",
			"Active",
			"Failed",
		},
		"manager": {
			"unknown",
			"Pending",
			"Running",
			"Updating",
			"Failed",
		},
		"team": {
			"unknown",
			"Pending",
			"Active",
			"Degraded",
			"Failed",
		},
		"worker": {
			"unknown",
			"Pending",
			"Starting",
			"Running",
			"Stopping",
			"Sleeping",
			"Stopped",
			"Failed",
		},
	}
	httpRoutesByMethod = map[string][]string{
		"DELETE": {
			"/api/v1/gateway/consumers/{id}",
			"/api/v1/humans/{name}",
			"/api/v1/managers/{name}",
			"/api/v1/teams/{name}",
			"/api/v1/workers/{name}",
		},
		"GET": {
			"/_matrix/app/v1/rooms/{roomAlias}",
			"/_matrix/app/v1/users/{userId}",
			"/api/v1/humans",
			"/api/v1/humans/{name}",
			"/api/v1/managers",
			"/api/v1/managers/{name}",
			"/api/v1/status",
			"/api/v1/teams",
			"/api/v1/teams/{name}",
			"/api/v1/version",
			"/api/v1/workers",
			"/api/v1/workers/{name}",
			"/api/v1/workers/{name}/status",
			"/docker/",
			"/healthz",
			"unmatched",
		},
		"POST": {
			"/api/v1/credentials/sts",
			"/api/v1/gateway/consumers",
			"/api/v1/gateway/consumers/{id}/bind",
			"/api/v1/humans",
			"/api/v1/managers",
			"/api/v1/packages",
			"/api/v1/teams",
			"/api/v1/workers",
			"/api/v1/workers/{name}/ensure-ready",
			"/api/v1/workers/{name}/heartbeat",
			"/api/v1/workers/{name}/ready",
			"/api/v1/workers/{name}/sleep",
			"/api/v1/workers/{name}/wake",
			"unmatched",
		},
		"PUT": {
			"/_matrix/app/v1/transactions/{txnId}",
			"/api/v1/managers/{name}",
			"/api/v1/teams/{name}",
			"/api/v1/workers/{name}",
			"unmatched",
		},
	}
	upstreamOpsByTarget = map[string][]string{
		"matrix": {
			"create_room",
			"delete_room_alias",
			"invite",
			"join_room",
			"kick",
			"leave_room",
			"list_joined_rooms",
			"list_room_members",
			"login",
			"register_user",
			"resolve_room_alias",
			"send_message",
			"set_display_name",
			"set_room_name",
			"set_room_state",
			"sync_messages",
			"unknown",
		},
		"sts_provider": {
			"get_kubeconfig",
			"issue_token",
		},
	}
	upstreamStatusClasses = []string{"none", "2xx", "3xx", "4xx", "5xx"}
	upstreamErrorClasses  = []string{"canceled", "decode", "http", "invalid_response", "network", "timeout", "unknown"}
)

var (
	// ReconcileDuration is a histogram of Reconcile latencies in seconds,
	// partitioned by CRD kind and outcome ("success" or "error").
	ReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "reconcile_duration_seconds",
			Help:      "Time spent in a single Reconcile call, partitioned by CRD kind and outcome.",
			Buckets:   prometheus.ExponentialBuckets(0.01, 2, 12),
		},
		[]string{"kind", "result"},
	)

	// ReconcileTotal counts Reconcile invocations, partitioned by CRD kind and
	// outcome. The success counter doubles as a liveness signal: if it stops
	// advancing for a kind that has live CRs, the controller is either deadlocked
	// or starved.
	ReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "reconcile_total",
			Help:      "Number of Reconcile calls, partitioned by CRD kind and outcome.",
		},
		[]string{"kind", "result"},
	)

	// ReconcileErrors counts Reconcile invocations that returned a non-nil error.
	// It is redundant with ReconcileTotal{result="error"} but kept separate so
	// alerts can be written against a single-label series.
	ReconcileErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "reconcile_errors_total",
			Help:      "Number of Reconcile calls that returned an error.",
		},
		[]string{"kind"},
	)

	// ControllerCRCount reports the number of CRs currently visible to this
	// controller's cache, grouped by CR kind and bounded status phase.
	ControllerCRCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "controller_cr_count",
			Help:      "Number of CRs currently visible to this controller, partitioned by kind and bounded status phase.",
		},
		[]string{"kind", "status"},
	)

	// ControllerHTTPRequestDuration records inbound controller HTTP API latency
	// using normalized route patterns, never raw request paths.
	ControllerHTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "controller_http_request_duration_seconds",
			Help:      "Time spent serving controller HTTP API requests, partitioned by method, route, and result.",
			Buckets:   prometheus.ExponentialBuckets(0.005, 2, 13),
		},
		[]string{"method", "route", "result"},
	)

	// ControllerHTTPRequests counts inbound controller HTTP API requests.
	ControllerHTTPRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "controller_http_requests_total",
			Help:      "Number of controller HTTP API requests, partitioned by method, route, result, and HTTP status class.",
		},
		[]string{"method", "route", "result", "status_class"},
	)

	// ControllerHTTPErrors counts inbound controller HTTP API requests that
	// completed with HTTP status >= 400.
	ControllerHTTPErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "controller_http_errors_total",
			Help:      "Number of failed controller HTTP API requests, partitioned by method, route, and HTTP status class.",
		},
		[]string{"method", "route", "status_class"},
	)

	// UpstreamRequestDuration records controller outbound call latency with
	// bounded labels only. The operation label must be a stable enum, not a raw
	// URL path or resource name.
	UpstreamRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "upstream_request_duration_seconds",
			Help:      "Time spent on controller outbound upstream calls, partitioned by upstream, operation, and result.",
			Buckets:   prometheus.ExponentialBuckets(0.01, 2, 12),
		},
		[]string{"upstream", "operation", "result"},
	)

	// UpstreamRequests counts controller outbound upstream calls and their HTTP
	// status class. status_class is "none" when no HTTP response was received.
	UpstreamRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "upstream_requests_total",
			Help:      "Number of controller outbound upstream calls, partitioned by upstream, operation, result, and HTTP status class.",
		},
		[]string{"upstream", "operation", "result", "status_class"},
	)

	// UpstreamRequestErrors counts outbound upstream calls that failed at the
	// HTTP status, network, timeout, cancellation, or response decoding layer.
	UpstreamRequestErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "upstream_request_errors_total",
			Help:      "Number of failed controller outbound upstream calls, partitioned by upstream, operation, and error class.",
		},
		[]string{"upstream", "operation", "error_class"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		ReconcileDuration,
		ReconcileTotal,
		ReconcileErrors,
		ControllerCRCount,
		ControllerHTTPRequestDuration,
		ControllerHTTPRequests,
		ControllerHTTPErrors,
		UpstreamRequestDuration,
		UpstreamRequests,
		UpstreamRequestErrors,
	)
	initializeSeries()
}

func initializeSeries() {
	for _, kind := range reconcileKinds {
		ReconcileErrors.WithLabelValues(kind)
		for _, result := range reconcileResults {
			ReconcileDuration.WithLabelValues(kind, result)
			ReconcileTotal.WithLabelValues(kind, result)
		}
	}
	for kind, statuses := range crStatusesByKind {
		for _, status := range statuses {
			ControllerCRCount.WithLabelValues(kind, status)
		}
	}
	for method, routes := range httpRoutesByMethod {
		for _, route := range routes {
			for _, result := range reconcileResults {
				ControllerHTTPRequestDuration.WithLabelValues(method, route, result)
				for _, statusClass := range upstreamStatusClasses {
					ControllerHTTPRequests.WithLabelValues(method, route, result, statusClass)
				}
			}
			for _, statusClass := range upstreamStatusClasses {
				ControllerHTTPErrors.WithLabelValues(method, route, statusClass)
			}
		}
	}
	for upstream, operations := range upstreamOpsByTarget {
		for _, operation := range operations {
			for _, result := range reconcileResults {
				UpstreamRequestDuration.WithLabelValues(upstream, operation, result)
				for _, statusClass := range upstreamStatusClasses {
					UpstreamRequests.WithLabelValues(upstream, operation, result, statusClass)
				}
			}
			for _, errorClass := range upstreamErrorClasses {
				UpstreamRequestErrors.WithLabelValues(upstream, operation, errorClass)
			}
		}
	}
}

// Observe records duration and outcome for a single Reconcile call. Intended to
// be invoked from a deferred closure so it sees the final value of the named
// error return.
func Observe(kind string, start time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
		ReconcileErrors.WithLabelValues(kind).Inc()
	}
	ReconcileDuration.WithLabelValues(kind, result).Observe(time.Since(start).Seconds())
	ReconcileTotal.WithLabelValues(kind, result).Inc()
}

// ObserveControllerHTTP records latency and outcome for an inbound controller
// HTTP request. The route argument must be a normalized route pattern.
func ObserveControllerHTTP(method, route string, start time.Time, statusCode int) {
	if method == "" {
		method = "UNKNOWN"
	}
	if route == "" {
		route = "unmatched"
	}
	result := "success"
	statusClass := upstreamStatusClass(statusCode)
	if statusCode >= 400 {
		result = "error"
	}
	ControllerHTTPRequestDuration.WithLabelValues(method, route, result).Observe(time.Since(start).Seconds())
	ControllerHTTPRequests.WithLabelValues(method, route, result, statusClass).Inc()
	if result == "error" {
		ControllerHTTPErrors.WithLabelValues(method, route, statusClass).Inc()
	}
}

// ObserveUpstream records latency and outcome for a controller outbound call.
func ObserveUpstream(upstream, operation string, start time.Time, statusCode int, err error) {
	result := "success"
	statusClass := upstreamStatusClass(statusCode)
	errorClass := ""
	if err != nil {
		result = "error"
		errorClass = classifyUpstreamError(err)
	} else if statusCode >= 400 {
		result = "error"
		errorClass = "http"
	}
	UpstreamRequestDuration.WithLabelValues(upstream, operation, result).Observe(time.Since(start).Seconds())
	UpstreamRequests.WithLabelValues(upstream, operation, result, statusClass).Inc()
	if errorClass != "" {
		UpstreamRequestErrors.WithLabelValues(upstream, operation, errorClass).Inc()
	}
}

func upstreamStatusClass(statusCode int) string {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return "2xx"
	case statusCode >= 300 && statusCode < 400:
		return "3xx"
	case statusCode >= 400 && statusCode < 500:
		return "4xx"
	case statusCode >= 500:
		return "5xx"
	default:
		return "none"
	}
}

func classifyUpstreamError(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout"
		}
		return "network"
	}
	if errors.Is(err, ErrDecodeResponse) {
		return "decode"
	}
	if errors.Is(err, ErrInvalidUpstreamResponse) {
		return "invalid_response"
	}
	return "unknown"
}
