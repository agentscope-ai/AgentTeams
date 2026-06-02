// Package metrics defines hiclaw-specific Prometheus metrics and registers
// them with the controller-runtime metrics registry, which exposes them on
// the metrics endpoint configured by ctrl.Options.Metrics.BindAddress.
//
// controller-runtime already exports generic reconcile metrics (controller_
// runtime_reconcile_total etc.); these hiclaw_* metrics carry the CRD kind
// as a label so we can dashboard per-CRD signals without parsing the
// controller name.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const subsystem = "hiclaw"

var (
	// ReconcileDuration is a histogram of Reconcile latencies in seconds,
	// partitioned by CRD kind and outcome ("success" or "error").
	ReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: subsystem,
			Name:      "reconcile_duration_seconds",
			Help:      "Time spent in a single Reconcile call, partitioned by CRD kind and outcome.",
			Buckets:   prometheus.ExponentialBuckets(0.01, 2, 12),
		},
		[]string{"kind", "result"},
	)

	// ReconcileTotal counts Reconcile invocations, partitioned by CRD kind
	// and outcome. The success counter doubles as a liveness signal — if it
	// stops advancing for a kind that has live CRs, the controller is
	// either deadlocked or starved.
	ReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: subsystem,
			Name:      "reconcile_total",
			Help:      "Number of Reconcile calls, partitioned by CRD kind and outcome.",
		},
		[]string{"kind", "result"},
	)

	// ReconcileErrors counts Reconcile invocations that returned a non-nil
	// error. Redundant with ReconcileTotal{result="error"} but kept
	// separate so alerts can be written against a single-label series.
	ReconcileErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: subsystem,
			Name:      "reconcile_errors_total",
			Help:      "Number of Reconcile calls that returned an error.",
		},
		[]string{"kind"},
	)

	// MigrationPhase tracks the current migration phase for each Team.
	// The gauge is set to 1 for the current phase (team×phase pair);
	// consumers should use max_over_time or last_over_time to determine
	// the latest phase per team.
	MigrationPhase = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: subsystem,
			Name:      "team_migration_phase",
			Help:      "Current migration phase of a Team CR (1 = active phase).",
		},
		[]string{"team", "phase"},
	)

	// MigrationFailures counts migration step failures, partitioned by the
	// phase in which the failure occurred.
	MigrationFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: subsystem,
			Name:      "team_migration_failures_total",
			Help:      "Number of migration step failures, partitioned by phase.",
		},
		[]string{"phase"},
	)
)

func init() {
	metrics.Registry.MustRegister(ReconcileDuration, ReconcileTotal, ReconcileErrors, MigrationPhase, MigrationFailures)
}

// Observe records duration and outcome for a single Reconcile call. Intended
// to be invoked from a deferred closure so it sees the final value of the
// named error return.
func Observe(kind string, start time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
		ReconcileErrors.WithLabelValues(kind).Inc()
	}
	ReconcileDuration.WithLabelValues(kind, result).Observe(time.Since(start).Seconds())
	ReconcileTotal.WithLabelValues(kind, result).Inc()
}
