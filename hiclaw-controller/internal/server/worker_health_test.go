package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestProbeContainerHealth(t *testing.T) {
	t.Parallel()
	checkedAt := time.Now().UTC().Format(time.RFC3339)

	healthy := probeContainerHealth(workerHealthInput{ContainerState: string(backend.StatusRunning)}, checkedAt)
	if healthy.Status != HealthHealthy {
		t.Fatalf("running container = %q, want healthy", healthy.Status)
	}

	down := probeContainerHealth(workerHealthInput{Phase: "Sleeping"}, checkedAt)
	if down.Status != HealthDown {
		t.Fatalf("sleeping container = %q, want down", down.Status)
	}
}

func TestProbeTimestampHealth(t *testing.T) {
	t.Parallel()
	checkedAt := time.Now().UTC().Format(time.RFC3339)
	recent := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)

	check := probeTimestampHealth(recent, checkedAt, 10*time.Minute, 30*time.Minute)
	if check.Status != HealthHealthy {
		t.Fatalf("recent heartbeat = %q, want healthy", check.Status)
	}

	stale := probeTimestampHealth(
		time.Now().UTC().Add(-20*time.Minute).Format(time.RFC3339),
		checkedAt,
		10*time.Minute,
		30*time.Minute,
	)
	if stale.Status != HealthDegraded {
		t.Fatalf("stale heartbeat = %q, want degraded", stale.Status)
	}
}

func TestWorkerHealthProberUsesCache(t *testing.T) {
	t.Parallel()

	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(modelsServer.Close)

	gw := &healthTestGateway{
		mcpServers: []gateway.MCPServerInfo{
			{Name: "mcp-gitea-alpha", AllowedConsumers: []string{"worker-alpha"}},
		},
	}

	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hiclaw-creds-alpha", Namespace: "default"},
		Data:       map[string][]byte{"WORKER_GATEWAY_KEY": []byte("test-key")},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.WorkerSpec{
			McpServers: []v1beta1.MCPServer{{Name: "gitea", URL: "https://gw/mcp-servers/mcp-gitea-alpha/mcp"}},
		},
		Status: v1beta1.WorkerStatus{
			Phase:         "Running",
			LastHeartbeat: time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339),
			LastActiveAt:  time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339),
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(secret, worker).
		Build()

	prober := NewWorkerHealthProber(k8sClient, "default", gw, modelsServer.URL)
	resp := workerToResponse(worker)
	resp.ContainerState = string(backend.StatusRunning)

	prober.AttachHealthChecks(context.Background(), &resp, worker)
	if resp.HealthChecks == nil {
		t.Fatal("expected health checks")
	}
	if resp.HealthChecks.LLM.Status != HealthHealthy {
		t.Fatalf("LLM = %q (%s), want healthy", resp.HealthChecks.LLM.Status, resp.HealthChecks.LLM.Detail)
	}
	if resp.HealthChecks.Git.Status != HealthHealthy {
		t.Fatalf("Git = %q (%s), want healthy", resp.HealthChecks.Git.Status, resp.HealthChecks.Git.Detail)
	}

	gw.listCalls = 0
	modelsServer.CloseClientConnections()
	prober.AttachHealthChecks(context.Background(), &resp, worker)
	if gw.listCalls != 0 {
		t.Fatalf("expected cached health to skip MCP list, got %d calls", gw.listCalls)
	}
}

func TestGetWorkerRuntimeStatusIncludesHealthChecks(t *testing.T) {
	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(modelsServer.Close)

	scheme := newLifecycleTestScheme(t)
	_ = corev1.AddToScheme(scheme)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hiclaw-creds-alpha-dev", Namespace: "default"},
		Data:       map[string][]byte{"WORKER_GATEWAY_KEY": []byte("test-key")},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status: v1beta1.WorkerStatus{
			Phase:         "Running",
			LastHeartbeat: time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339),
			LastActiveAt:  time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339),
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(secret, worker).
		Build()

	backendStub := &stubWorkerBackend{status: backend.StatusRunning}
	health := NewWorkerHealthProber(k8sClient, "default", &healthTestGateway{}, modelsServer.URL)
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default", health)
	handler.setReady("alpha-dev", true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev/status", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.GetWorkerRuntimeStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.HealthChecks == nil {
		t.Fatal("expected healthChecks in status response")
	}
	if resp.HealthChecks.Container.Status != HealthHealthy {
		t.Fatalf("container health = %q, want healthy", resp.HealthChecks.Container.Status)
	}
	if resp.HealthChecks.Heartbeat.Status != HealthHealthy {
		t.Fatalf("heartbeat health = %q, want healthy", resp.HealthChecks.Heartbeat.Status)
	}
}

func TestAttachHealthChecksBatchRespectsListBudget(t *testing.T) {
	const probeDelay = 4 * time.Second
	const workerCount = 5

	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(probeDelay)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(modelsServer.Close)

	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	var objects []client.Object
	for i := 0; i < workerCount; i++ {
		name := fmt.Sprintf("worker-%d", i)
		objects = append(objects,
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "hiclaw-creds-" + name, Namespace: "default"},
				Data:       map[string][]byte{"WORKER_GATEWAY_KEY": []byte("test-key")},
			},
			&v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Status: v1beta1.WorkerStatus{
					Phase:         "Running",
					LastHeartbeat: time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339),
					LastActiveAt:  time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339),
				},
			},
		)
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()

	prober := NewWorkerHealthProber(k8sClient, "default", &healthTestGateway{}, modelsServer.URL)
	items := make([]healthAttachItem, workerCount)
	for i := 0; i < workerCount; i++ {
		name := fmt.Sprintf("worker-%d", i)
		resp := WorkerResponse{
			Name:           name,
			Phase:          "Running",
			ContainerState: string(backend.StatusRunning),
			LastHeartbeat:  time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339),
			LastActiveAt:   time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339),
		}
		items[i] = healthAttachItem{Resp: &resp}
	}

	start := time.Now()
	prober.AttachHealthChecksBatch(context.Background(), items)
	elapsed := time.Since(start)

	maxExpected := workerHealthListBudget + 1500*time.Millisecond
	if elapsed > maxExpected {
		t.Fatalf("batch attach took %v, want <= %v with shared list budget", elapsed, maxExpected)
	}

	for i, item := range items {
		if item.Resp.HealthChecks == nil {
			t.Fatalf("worker %d: expected health checks", i)
		}
		if item.Resp.HealthChecks.Container.Status != HealthHealthy {
			t.Fatalf("worker %d container = %q, want healthy", i, item.Resp.HealthChecks.Container.Status)
		}
	}
}

func TestListWorkersBatchHealthWithinBudget(t *testing.T) {
	const probeDelay = 4 * time.Second
	const workerCount = 5

	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(probeDelay)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(modelsServer.Close)

	scheme := newServerTestScheme(t)
	_ = corev1.AddToScheme(scheme)

	var objects []client.Object
	for i := 0; i < workerCount; i++ {
		name := fmt.Sprintf("solo-%d", i)
		objects = append(objects,
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "hiclaw-creds-" + name, Namespace: "default"},
				Data:       map[string][]byte{"WORKER_GATEWAY_KEY": []byte("test-key")},
			},
			&v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Status: v1beta1.WorkerStatus{
					Phase:         "Running",
					LastHeartbeat: time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339),
					LastActiveAt:  time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339),
				},
			},
		)
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()

	handler := NewResourceHandler(k8sClient, "default", nil, "")
	handler.SetWorkerHealthProber(NewWorkerHealthProber(k8sClient, "default", &healthTestGateway{}, modelsServer.URL))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rec := httptest.NewRecorder()

	start := time.Now()
	handler.ListWorkers(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	maxExpected := workerHealthListBudget + 1500*time.Millisecond
	if elapsed > maxExpected {
		t.Fatalf("ListWorkers took %v, want <= %v with shared list budget", elapsed, maxExpected)
	}

	var listResp WorkerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listResp.Workers) != workerCount {
		t.Fatalf("worker count = %d, want %d", len(listResp.Workers), workerCount)
	}
	for _, w := range listResp.Workers {
		if w.HealthChecks == nil {
			t.Fatalf("worker %q missing healthChecks", w.Name)
		}
	}
}

func TestGetWorkerIncludesHealthChecks(t *testing.T) {
	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(modelsServer.Close)

	scheme := newServerTestScheme(t)
	_ = corev1.AddToScheme(scheme)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hiclaw-creds-alpha-dev", Namespace: "default"},
		Data:       map[string][]byte{"WORKER_GATEWAY_KEY": []byte("test-key")},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status: v1beta1.WorkerStatus{
			Phase:         "Running",
			LastHeartbeat: time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339),
			LastActiveAt:  time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339),
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(secret, worker).
		Build()

	handler := NewResourceHandler(k8sClient, "default", nil, "")
	handler.SetWorkerHealthProber(NewWorkerHealthProber(k8sClient, "default", &healthTestGateway{}, modelsServer.URL))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()
	handler.GetWorker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.HealthChecks == nil {
		t.Fatal("expected healthChecks on GetWorker response")
	}
}

func TestHigressClientListMCPServers(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "ok"})
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/mcpServer":
			_, _ = w.Write([]byte(`{"data":[{"name":"mcp-gitea-bob","consumerAuthInfo":{"allowedConsumers":["worker-bob"]}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := gateway.NewHigressClient(gateway.Config{
		ConsoleURL:    server.URL,
		AdminUser:     "admin",
		AdminPassword: "admin",
	}, server.Client())

	servers, err := client.ListMCPServers(context.Background())
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "mcp-gitea-bob" {
		t.Fatalf("servers = %#v", servers)
	}
	if !strings.Contains(strings.Join(servers[0].AllowedConsumers, ","), "worker-bob") {
		t.Fatalf("allowed consumers = %#v", servers[0].AllowedConsumers)
	}
}

type healthTestGateway struct {
	mcpServers []gateway.MCPServerInfo
	listCalls  int
}

func (g *healthTestGateway) EnsureConsumer(context.Context, gateway.ConsumerRequest) (*gateway.ConsumerResult, error) {
	return &gateway.ConsumerResult{}, nil
}
func (g *healthTestGateway) DeleteConsumer(context.Context, string) error { return nil }
func (g *healthTestGateway) AuthorizeAIRoutes(context.Context, string, string) error {
	return nil
}
func (g *healthTestGateway) DeauthorizeAIRoutes(context.Context, string, string) error { return nil }
func (g *healthTestGateway) ExposePort(context.Context, gateway.PortExposeRequest) error {
	return nil
}
func (g *healthTestGateway) UnexposePort(context.Context, gateway.PortExposeRequest) error {
	return nil
}
func (g *healthTestGateway) EnsureServiceSource(context.Context, string, string, int, string) error {
	return nil
}
func (g *healthTestGateway) EnsureStaticServiceSource(context.Context, string, string, int) error {
	return nil
}
func (g *healthTestGateway) EnsureRoute(context.Context, string, []string, string, int, string) error {
	return nil
}
func (g *healthTestGateway) DeleteRoute(context.Context, string) error { return nil }
func (g *healthTestGateway) EnsureAIProvider(context.Context, gateway.AIProviderRequest) error {
	return nil
}
func (g *healthTestGateway) EnsureStreamIdleTimeout(context.Context, int) error { return nil }
func (g *healthTestGateway) EnsureAIRoute(context.Context, gateway.AIRouteRequest) error {
	return nil
}
func (g *healthTestGateway) ResolveModelProvider(context.Context, string) (*gateway.ModelProviderInfo, error) {
	return nil, gateway.ErrUnsupportedOp
}
func (g *healthTestGateway) Healthy(context.Context) error { return nil }
func (g *healthTestGateway) ListMCPServers(context.Context) ([]gateway.MCPServerInfo, error) {
	g.listCalls++
	return g.mcpServers, nil
}
