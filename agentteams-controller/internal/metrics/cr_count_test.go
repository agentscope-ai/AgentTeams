package metrics

import (
	"context"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCRCountCollectorRefreshCountsByKindAndStatus(t *testing.T) {
	ControllerCRCount.Reset()
	initializeSeries()

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "w-running", Namespace: "ns1"},
				Status:     v1beta1.WorkerStatus{Phase: "Running"},
			},
			&v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "w-empty", Namespace: "ns1"},
			},
			&v1beta1.Team{
				ObjectMeta: metav1.ObjectMeta{Name: "t-active", Namespace: "ns1"},
				Status:     v1beta1.TeamStatus{Phase: "Active"},
			},
			&v1beta1.Human{
				ObjectMeta: metav1.ObjectMeta{Name: "h-custom", Namespace: "ns1"},
				Status:     v1beta1.HumanStatus{Phase: "Custom"},
			},
			&v1beta1.Manager{
				ObjectMeta: metav1.ObjectMeta{Name: "m-running", Namespace: "ns1"},
				Status:     v1beta1.ManagerStatus{Phase: "Running"},
			},
			&v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "w-other", Namespace: "ns2"},
				Status:     v1beta1.WorkerStatus{Phase: "Running"},
			},
		).
		Build()

	collector := &CRCountCollector{Client: cli, Namespace: "ns1"}
	if err := collector.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	assertGauge(t, "worker", "Running", 1)
	assertGauge(t, "worker", "unknown", 1)
	assertGauge(t, "team", "Active", 1)
	assertGauge(t, "human", "unknown", 1)
	assertGauge(t, "manager", "Running", 1)
}

func TestCRCountCollectorCanSkipManagers(t *testing.T) {
	ControllerCRCount.Reset()
	initializeSeries()

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "w-running", Namespace: "ns1"},
				Status:     v1beta1.WorkerStatus{Phase: "Running"},
			},
			&v1beta1.Manager{
				ObjectMeta: metav1.ObjectMeta{Name: "m-running", Namespace: "ns1"},
				Status:     v1beta1.ManagerStatus{Phase: "Running"},
			},
		).
		Build()

	collector := &CRCountCollector{Client: cli, Namespace: "ns1", SkipManagers: true}
	if err := collector.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	assertGauge(t, "worker", "Running", 1)
	assertGauge(t, "manager", "Running", 0)
}

func TestBoundedCRStatus(t *testing.T) {
	tests := []struct {
		kind   string
		status string
		want   string
	}{
		{kind: "worker", status: "Running", want: "Running"},
		{kind: "worker", status: "Bogus", want: "unknown"},
		{kind: "human", status: "", want: "unknown"},
	}
	for _, tt := range tests {
		if got := boundedCRStatus(tt.kind, tt.status); got != tt.want {
			t.Fatalf("boundedCRStatus(%q, %q) = %q, want %q", tt.kind, tt.status, got, tt.want)
		}
	}
}

func assertGauge(t *testing.T, kind, status string, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(ControllerCRCount.WithLabelValues(kind, status)); got != want {
		t.Fatalf("controller_cr_count{%s,%s} = %v, want %v", kind, status, got, want)
	}
}
