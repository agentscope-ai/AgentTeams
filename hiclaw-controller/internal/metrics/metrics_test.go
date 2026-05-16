package metrics

import (
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

func TestObserve_RecordsDuration(t *testing.T) {
	ReconcileDuration.Reset()
	start := time.Now().Add(-50 * time.Millisecond)
	Observe("manager", start, nil)

	if got := testutil.CollectAndCount(ReconcileDuration); got == 0 {
		t.Error("ReconcileDuration histogram recorded no samples")
	}
}
