package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeMonitoringReader struct {
	mu    sync.Mutex
	names []string
	logs  string
}

func (f *fakeMonitoringReader) ReadContainerMetrics(_ context.Context, name string) (*backend.ContainerMetrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	if strings.Contains(name, "missing") {
		return nil, backend.ErrNotFound
	}
	return &backend.ContainerMetrics{State: "running"}, nil
}
func (f *fakeMonitoringReader) ReadContainerLogs(_ context.Context, name string, _ int, _ time.Time) (*backend.ContainerLogs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	return &backend.ContainerLogs{Text: f.logs}, nil
}
func monitoringTestHandler(t *testing.T) *monitoringHandler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(
		&v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"}},
		&v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "default"}},
		&v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}},
	).Build()
	return &monitoringHandler{deps: ServerDeps{Client: c, Namespace: "default", KubeMode: "embedded", ContainerPrefix: "custom-worker-", ResourcePrefix: "custom-"}, reader: &fakeMonitoringReader{}}
}

func TestMonitoringPageAndPartialFailure(t *testing.T) {
	h := monitoringTestHandler(t)
	r := httptest.NewRequest("GET", "/api/v1/monitoring?search=worker&limit=1&page=2", nil)
	w := httptest.NewRecorder()
	h.snapshot(w, r)
	var out struct {
		Items       []monitoringTarget
		Total, Page int
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || out.Total != 2 || out.Page != 2 || len(out.Items) != 1 || out.Items[0].Availability != "missing" {
		t.Fatalf("partial page: %s", w.Body.String())
	}
	f := h.reader.(*fakeMonitoringReader)
	if len(f.names) != 1 || f.names[0] != "custom-worker-missing" {
		t.Fatalf("must only sample requested page: %v", f.names)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("live telemetry must not be cached")
	}
	for _, q := range []string{"limit=0", "limit=51", "page=-1", "tail=abc", "since=garbage"} {
		w = httptest.NewRecorder()
		r = httptest.NewRequest("GET", "/api/v1/monitoring?"+q, nil)
		if strings.HasPrefix(q, "tail") || strings.HasPrefix(q, "since") {
			h.logs(w, r)
		} else {
			h.snapshot(w, r)
		}
		if w.Code != 400 {
			t.Errorf("%s: got %d", q, w.Code)
		}
	}
}

func TestMonitoringLogsAllowlistRedactionAndUnsupported(t *testing.T) {
	h := monitoringTestHandler(t)
	f := h.reader.(*fakeMonitoringReader)
	f.logs = "2026-11-14T00:00:00Z request timeout; Authorization: Bearer example-token\n{\"api_key\":\"secret-value\"} password=hidden"
	r := httptest.NewRequest("GET", "/logs", nil)
	r.SetPathValue("kind", "worker")
	r.SetPathValue("name", "dev")
	w := httptest.NewRecorder()
	h.logs(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "request timeout") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"example-token", "secret-value", "hidden"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("secret leaked: %s", secret)
		}
	}
	if len(f.names) != 1 || f.names[0] != "custom-worker-dev" {
		t.Fatalf("wrong resource mapping: %v", f.names)
	}
	r.SetPathValue("name", "unrelated-container")
	w = httptest.NewRecorder()
	h.logs(w, r)
	if w.Code != 404 || len(f.names) != 1 {
		t.Fatal("unknown resource must not reach Docker")
	}
	h.reader = nil
	h.deps.KubeMode = "incluster"
	r.SetPathValue("name", "dev")
	w = httptest.NewRecorder()
	h.logs(w, r)
	if w.Code != 501 {
		t.Fatalf("unsupported logs: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.snapshot(w, httptest.NewRequest("GET", "/monitoring", nil))
	var out struct{ Items []monitoringTarget }
	json.Unmarshal(w.Body.Bytes(), &out)
	for _, row := range out.Items {
		if row.Availability != "unsupported" || row.Metrics != nil {
			t.Fatal("unsupported source must not manufacture metrics")
		}
	}
}

func TestMonitoringRespectsDisabledWorkerPrefixAndExternalWorkers(t *testing.T) {
	h := monitoringTestHandler(t)
	h.deps.ContainerPrefix = ""
	rows, err := h.targets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Kind == "worker" && row.Name == "dev" && row.container != "dev" {
			t.Fatalf("disabled prefix was replaced: %s", row.container)
		}
	}
	var worker v1beta1.Worker
	if err := h.deps.Client.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "dev"}, &worker); err != nil {
		t.Fatal(err)
	}
	managed := false
	worker.Spec.ContainerManaged = &managed
	if err := h.deps.Client.Update(context.Background(), &worker); err != nil {
		t.Fatal(err)
	}
	rows, err = h.targets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Kind == "worker" && row.Name == "dev" && row.Availability != "unsupported" {
			t.Fatal("external Worker cannot be mapped to a host container")
		}
	}
}

func TestMonitoringRoutesRequireAdministratorOrManager(t *testing.T) {
	h := monitoringTestHandler(t)
	for _, role := range []string{authpkg.RoleAdmin, authpkg.RoleManager, authpkg.RoleHuman, authpkg.RoleTeamLeader, authpkg.RoleWorker} {
		t.Run(role, func(t *testing.T) {
			mw := authpkg.NewMiddleware(&roleAuthenticator{identity: &authpkg.CallerIdentity{Role: role, Username: "tester", Team: "alpha"}}, nil, authpkg.NewAuthorizer(), h.deps.Client, "default")
			deps := h.deps
			deps.AuthMw = mw
			srv := NewHTTPServer(":0", deps)
			for _, path := range []string{"/api/v1/monitoring", "/api/v1/monitoring/worker/dev/logs"} {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.Header.Set("Authorization", "Bearer test")
				w := httptest.NewRecorder()
				srv.Mux.ServeHTTP(w, r)
				if role == authpkg.RoleAdmin || role == authpkg.RoleManager {
					if w.Code == 403 || w.Code == 401 {
						t.Fatalf("authorized role rejected: %d", w.Code)
					}
				} else if w.Code != 403 {
					t.Fatalf("%s can read host telemetry: %d", role, w.Code)
				}
			}
		})
	}
}
