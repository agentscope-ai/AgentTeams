package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockDockerAPI creates a test HTTP server that simulates Docker Engine API responses.
func mockDockerAPI(t *testing.T) *httptest.Server {
	t.Helper()

	// In-memory container store
	containers := map[string]map[string]interface{}{}
	// In-memory image store (pre-populated with common test images)
	images := map[string]bool{
		"agentteams/worker-agent:latest": true,
		"agentteams/copaw-worker:latest": true,
		"img:latest":                     true,
	}

	mux := http.NewServeMux()

	// GET /images/{name}/json — check if image exists
	mux.HandleFunc("GET /images/", func(w http.ResponseWriter, r *http.Request) {
		// Extract image name from path (strip /images/ prefix and /json suffix)
		path := strings.TrimPrefix(r.URL.Path, "/images/")
		path = strings.TrimSuffix(path, "/json")
		if images[path] {
			json.NewEncoder(w).Encode(map[string]string{"Id": "sha256-" + path})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
	})

	// POST /images/create — pull image
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		fromImage := r.URL.Query().Get("fromImage")
		if fromImage != "" {
			images[fromImage] = true
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"Pull complete"}`))
	})

	// POST /containers/create?name=xxx
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if _, exists := containers[name]; exists {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"message": "conflict"})
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		id := fmt.Sprintf("sha256-%s", name)
		containers[name] = map[string]interface{}{
			"Id":    id,
			"Name":  "/" + name,
			"State": map[string]interface{}{"Status": "created"},
			"Image": body["Image"],
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"Id": id})
	})

	// POST /containers/{id}/start
	mux.HandleFunc("POST /containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for _, c := range containers {
			if c["Id"] == id || c["Name"] == "/"+id {
				state := c["State"].(map[string]interface{})
				state["Status"] = "running"
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
	})

	// POST /containers/{id}/stop
	mux.HandleFunc("POST /containers/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for _, c := range containers {
			if c["Id"] == id || c["Name"] == "/"+id {
				state := c["State"].(map[string]interface{})
				state["Status"] = "exited"
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
	})

	// GET /containers/{id}/json
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for _, c := range containers {
			if c["Id"] == id || c["Name"] == "/"+id {
				json.NewEncoder(w).Encode(c)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
	})

	// DELETE /containers/{id}
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for name, c := range containers {
			if c["Id"] == id || c["Name"] == "/"+id {
				delete(containers, name)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
	})

	// GET /containers/json (list)
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		var result []map[string]interface{}
		for name, c := range containers {
			state := c["State"].(map[string]interface{})
			result = append(result, map[string]interface{}{
				"Id":    c["Id"],
				"Names": []string{"/" + name},
				"State": state["Status"],
			})
		}
		if result == nil {
			result = []map[string]interface{}{}
		}
		json.NewEncoder(w).Encode(result)
	})

	return httptest.NewServer(mux)
}

func newTestDockerBackend(t *testing.T, serverURL string) *DockerBackend {
	t.Helper()
	b := &DockerBackend{
		config: DockerConfig{
			WorkerImage:      "agentteams/worker-agent:latest",
			CopawWorkerImage: "agentteams/copaw-worker:latest",
			DefaultNetwork:   "hiclaw-net",
		},
		containerPrefix: "agentteams-worker-",
		client: &http.Client{
			Transport: &testTransport{serverURL: serverURL},
		},
	}
	return b
}

// testTransport redirects requests from http://localhost/... to the test server.
type testTransport struct {
	serverURL string
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.serverURL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

func TestDockerCreate(t *testing.T) {
	srv := mockDockerAPI(t)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)

	result, err := b.Create(context.Background(), CreateRequest{
		Name:    "alice",
		Image:   "agentteams/worker-agent:latest",
		Network: "hiclaw-net",
		Env:     map[string]string{"AGENTTEAMS_WORKER_NAME": "alice"},
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if result.Name != "alice" {
		t.Errorf("expected name alice, got %s", result.Name)
	}
	if result.Backend != "docker" {
		t.Errorf("expected backend docker, got %s", result.Backend)
	}
	if result.DeploymentMode != DeployLocal {
		t.Errorf("expected deployment_mode local, got %s", result.DeploymentMode)
	}
	if result.Status != StatusRunning {
		t.Errorf("expected status running, got %s", result.Status)
	}
	if result.ContainerID == "" {
		t.Error("expected non-empty container ID")
	}
}

func TestDockerCreateConflict(t *testing.T) {
	srv := mockDockerAPI(t)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)

	_, err := b.Create(context.Background(), CreateRequest{Name: "alice", Image: "img:latest"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// Second create should succeed — auto-deletes existing container and retries
	result, err := b.Create(context.Background(), CreateRequest{Name: "alice", Image: "img:latest"})
	if err != nil {
		t.Fatalf("second create should succeed (auto-delete+retry), got: %v", err)
	}
	if result.Name != "alice" {
		t.Errorf("expected name alice, got %s", result.Name)
	}
}

func TestDockerCreatePullsImage(t *testing.T) {
	srv := mockDockerAPI(t)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)

	// Use an image that doesn't exist in the mock store — it should be pulled
	result, err := b.Create(context.Background(), CreateRequest{
		Name:  "puller",
		Image: "custom/image:v2",
	})
	if err != nil {
		t.Fatalf("Create with image pull failed: %v", err)
	}
	if result.Status != StatusRunning {
		t.Errorf("expected running, got %s", result.Status)
	}
}

// captureCreateImagesServer is a minimal Docker mock that records the Image
// field of every POST /containers/create request. Other endpoints return the
// minimum responses required to make DockerBackend.Create succeed.
type capturedCreateBodies struct {
	srv    *httptest.Server
	images []string
}

func (c *capturedCreateBodies) lastImage() string {
	if len(c.images) == 0 {
		return ""
	}
	return c.images[len(c.images)-1]
}

func captureCreateImagesServer(t *testing.T) *capturedCreateBodies {
	t.Helper()
	captured := &capturedCreateBodies{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"Id": "sha256-x"})
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if img, ok := body["Image"].(string); ok {
			captured.images = append(captured.images, img)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"Id": "sha256-test"})
	})
	mux.HandleFunc("POST /containers/{id}/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"Id":    "sha256-test",
			"State": map[string]interface{}{"Status": "running"},
		})
	})
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	captured.srv = httptest.NewServer(mux)
	return captured
}

func TestDockerStatus(t *testing.T) {
	srv := mockDockerAPI(t)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)

	// Create a worker first
	_, err := b.Create(context.Background(), CreateRequest{Name: "bob", Image: "img:latest"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	result, err := b.Status(context.Background(), "bob")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if result.Status != StatusRunning {
		t.Errorf("expected running, got %s", result.Status)
	}
}

func TestDockerStatusNotFound(t *testing.T) {
	srv := mockDockerAPI(t)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)

	result, err := b.Status(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if result.Status != StatusNotFound {
		t.Errorf("expected not_found, got %s", result.Status)
	}
}

func TestDockerStop(t *testing.T) {
	srv := mockDockerAPI(t)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)

	_, err := b.Create(context.Background(), CreateRequest{Name: "carol", Image: "img:latest"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if err := b.Stop(context.Background(), "carol"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	result, err := b.Status(context.Background(), "carol")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if result.Status != StatusStopped {
		t.Errorf("expected stopped, got %s", result.Status)
	}
}

func TestDockerStartStopped(t *testing.T) {
	srv := mockDockerAPI(t)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)

	_, err := b.Create(context.Background(), CreateRequest{Name: "dave", Image: "img:latest"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	b.Stop(context.Background(), "dave")

	if err := b.Start(context.Background(), "dave"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	result, err := b.Status(context.Background(), "dave")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if result.Status != StatusRunning {
		t.Errorf("expected running after start, got %s", result.Status)
	}
}

func TestDockerDelete(t *testing.T) {
	srv := mockDockerAPI(t)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)

	_, err := b.Create(context.Background(), CreateRequest{Name: "eve", Image: "img:latest"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if err := b.Delete(context.Background(), "eve"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	result, err := b.Status(context.Background(), "eve")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if result.Status != StatusNotFound {
		t.Errorf("expected not_found after delete, got %s", result.Status)
	}
}

func TestDockerDeleteNotFound(t *testing.T) {
	srv := mockDockerAPI(t)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)

	// Deleting a non-existent container should not error
	if err := b.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("Delete of non-existent should not error, got: %v", err)
	}
}

// capturedPayloadServer is a minimal Docker mock that records the full
// decoded dockerCreatePayload (including HostConfig) of every
// POST /containers/create request, for resource-limit / restart-policy
// assertions.
type capturedPayloadServer struct {
	srv      *httptest.Server
	payloads []dockerCreatePayload
}

func (c *capturedPayloadServer) last() dockerCreatePayload {
	return c.payloads[len(c.payloads)-1]
}

func capturePayloadServer(t *testing.T) *capturedPayloadServer {
	t.Helper()
	captured := &capturedPayloadServer{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"Id": "sha256-x"})
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		var payload dockerCreatePayload
		json.NewDecoder(r.Body).Decode(&payload)
		captured.payloads = append(captured.payloads, payload)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"Id": "sha256-test"})
	})
	mux.HandleFunc("POST /containers/{id}/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"Id":    "sha256-test",
			"State": map[string]interface{}{"Status": "running"},
		})
	})
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	captured.srv = httptest.NewServer(mux)
	return captured
}

func newCapturingDockerBackend(t *testing.T, serverURL string, cfg DockerConfig) *DockerBackend {
	t.Helper()
	if cfg.WorkerImage == "" {
		cfg.WorkerImage = "hiclaw/worker-agent:latest"
	}
	if cfg.DefaultNetwork == "" {
		cfg.DefaultNetwork = "hiclaw-net"
	}
	return &DockerBackend{
		config:          cfg,
		containerPrefix: "hiclaw-worker-",
		client: &http.Client{
			Transport: &testTransport{serverURL: serverURL},
		},
	}
}

// TestDockerCreateResourceLimitsFromRequest verifies that an explicit
// req.Resources override is converted to Docker Engine API units
// (NanoCpus / Memory bytes) and that HostConfig is actually attached to the
// create payload (guards against the HostConfig attach-condition trap in
// buildCreatePayload).
func TestDockerCreateResourceLimitsFromRequest(t *testing.T) {
	captured := capturePayloadServer(t)
	defer captured.srv.Close()

	b := newCapturingDockerBackend(t, captured.srv.URL, DockerConfig{
		WorkerCPU:    "1000m",
		WorkerMemory: "2Gi",
	})

	_, err := b.Create(context.Background(), CreateRequest{
		Name: "x",
		Resources: &ResourceRequirements{
			CPULimit:    "500m",
			MemoryLimit: "1Gi",
		},
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	payload := captured.last()
	if payload.HostConfig == nil {
		t.Fatal("expected HostConfig to be attached to the create payload, got nil")
	}
	wantNanoCPUs := int64(500) * 1e6 // 500m -> 500,000,000 nanocpus
	if payload.HostConfig.NanoCpus != wantNanoCPUs {
		t.Errorf("NanoCpus = %d, want %d", payload.HostConfig.NanoCpus, wantNanoCPUs)
	}
	wantMemoryBytes := int64(1) * 1024 * 1024 * 1024 // 1Gi
	if payload.HostConfig.Memory != wantMemoryBytes {
		t.Errorf("Memory = %d, want %d", payload.HostConfig.Memory, wantMemoryBytes)
	}
}

// TestDockerCreateResourceLimitsDefaults verifies that when req.Resources is
// nil, the Docker backend falls back to its configured defaults (mirroring
// kubernetes.go buildDefaultResources' 1000m/2Gi convention), and that an
// empty DockerConfig.WorkerCPU/WorkerMemory falls back further to the
// hardcoded 1000m/2Gi.
func TestDockerCreateResourceLimitsDefaults(t *testing.T) {
	t.Run("configured_defaults_used_when_resources_nil", func(t *testing.T) {
		captured := capturePayloadServer(t)
		defer captured.srv.Close()

		b := newCapturingDockerBackend(t, captured.srv.URL, DockerConfig{
			WorkerCPU:    "1000m",
			WorkerMemory: "2Gi",
		})

		_, err := b.Create(context.Background(), CreateRequest{Name: "x"})
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}

		payload := captured.last()
		if payload.HostConfig == nil {
			t.Fatal("expected HostConfig to be attached, got nil")
		}
		wantNanoCPUs := int64(1000) * 1e6
		if payload.HostConfig.NanoCpus != wantNanoCPUs {
			t.Errorf("NanoCpus = %d, want %d", payload.HostConfig.NanoCpus, wantNanoCPUs)
		}
		wantMemoryBytes := int64(2) * 1024 * 1024 * 1024
		if payload.HostConfig.Memory != wantMemoryBytes {
			t.Errorf("Memory = %d, want %d", payload.HostConfig.Memory, wantMemoryBytes)
		}
	})

	t.Run("hardcoded_fallback_when_config_empty", func(t *testing.T) {
		captured := capturePayloadServer(t)
		defer captured.srv.Close()

		// DockerConfig.WorkerCPU/WorkerMemory left empty — backend must
		// still apply the 1000m/2Gi convention shared with the K8s backend.
		b := newCapturingDockerBackend(t, captured.srv.URL, DockerConfig{})

		_, err := b.Create(context.Background(), CreateRequest{Name: "x"})
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}

		payload := captured.last()
		if payload.HostConfig == nil {
			t.Fatal("expected HostConfig to be attached, got nil")
		}
		wantNanoCPUs := int64(1000) * 1e6
		if payload.HostConfig.NanoCpus != wantNanoCPUs {
			t.Errorf("NanoCpus = %d, want %d", payload.HostConfig.NanoCpus, wantNanoCPUs)
		}
		wantMemoryBytes := int64(2) * 1024 * 1024 * 1024
		if payload.HostConfig.Memory != wantMemoryBytes {
			t.Errorf("Memory = %d, want %d", payload.HostConfig.Memory, wantMemoryBytes)
		}
	})

	t.Run("partial_override_falls_back_per_field", func(t *testing.T) {
		captured := capturePayloadServer(t)
		defer captured.srv.Close()

		b := newCapturingDockerBackend(t, captured.srv.URL, DockerConfig{
			WorkerCPU:    "1000m",
			WorkerMemory: "2Gi",
		})

		// Only CPULimit overridden; MemoryLimit must fall back to the default.
		_, err := b.Create(context.Background(), CreateRequest{
			Name: "x",
			Resources: &ResourceRequirements{
				CPULimit: "250m",
			},
		})
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}

		payload := captured.last()
		if payload.HostConfig == nil {
			t.Fatal("expected HostConfig to be attached, got nil")
		}
		wantNanoCPUs := int64(250) * 1e6
		if payload.HostConfig.NanoCpus != wantNanoCPUs {
			t.Errorf("NanoCpus = %d, want %d", payload.HostConfig.NanoCpus, wantNanoCPUs)
		}
		wantMemoryBytes := int64(2) * 1024 * 1024 * 1024
		if payload.HostConfig.Memory != wantMemoryBytes {
			t.Errorf("Memory = %d, want %d (should fall back to default when unset)", payload.HostConfig.Memory, wantMemoryBytes)
		}
	})
}

// TestDockerBuildCreatePayloadRestartPolicy verifies RestartPolicy is
// attached to HostConfig when set on the request (used by both the Manager
// path, applyEmbeddedConfig, and the member/worker path for Docker).
func TestDockerBuildCreatePayloadRestartPolicy(t *testing.T) {
	captured := capturePayloadServer(t)
	defer captured.srv.Close()

	b := newCapturingDockerBackend(t, captured.srv.URL, DockerConfig{})

	_, err := b.Create(context.Background(), CreateRequest{
		Name:          "x",
		RestartPolicy: "unless-stopped",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	payload := captured.last()
	if payload.HostConfig == nil {
		t.Fatal("expected HostConfig to be attached, got nil")
	}
	if payload.HostConfig.RestartPolicy == nil {
		t.Fatal("expected RestartPolicy to be set")
	}
	if payload.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("RestartPolicy.Name = %q, want %q", payload.HostConfig.RestartPolicy.Name, "unless-stopped")
	}
}

// TestDockerCreateConsolePortBindsLocalhost verifies that when
// AGENTTEAMS_CONSOLE_PORT is set, the worker console port is bound to
// 127.0.0.1 only (not 0.0.0.0).
func TestDockerCreateConsolePortBindsLocalhost(t *testing.T) {
	captured := capturePayloadServer(t)
	defer captured.srv.Close()

	b := newCapturingDockerBackend(t, captured.srv.URL, DockerConfig{})

	_, err := b.Create(context.Background(), CreateRequest{
		Name: "x",
		Env: map[string]string{
			"AGENTTEAMS_CONSOLE_PORT": "8088",
		},
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	payload := captured.last()
	if payload.HostConfig == nil {
		t.Fatal("expected HostConfig to be attached, got nil")
	}
	bindings, ok := payload.HostConfig.PortBindings["8088/tcp"]
	if !ok {
		t.Fatalf("expected PortBindings for 8088/tcp, got %v", payload.HostConfig.PortBindings)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 port binding, got %d", len(bindings))
	}
	if bindings[0].HostIP != "127.0.0.1" {
		t.Errorf("HostIP = %q, want %q", bindings[0].HostIP, "127.0.0.1")
	}
	if bindings[0].HostPort == "" {
		t.Error("expected non-empty HostPort for console binding")
	}
}

func TestMergeDockerResourceOverrides(t *testing.T) {
	cases := []struct {
		name         string
		defaultCPU   string
		defaultMem   string
		override     *ResourceRequirements
		wantNanoCPUs int64
		wantMemory   int64
	}{
		{
			name:         "nil_override_uses_defaults",
			defaultCPU:   "1000m",
			defaultMem:   "2Gi",
			override:     nil,
			wantNanoCPUs: 1000 * 1e6,
			wantMemory:   2 * 1024 * 1024 * 1024,
		},
		{
			name:       "full_override",
			defaultCPU: "1000m",
			defaultMem: "2Gi",
			override: &ResourceRequirements{
				CPULimit:    "2000m",
				MemoryLimit: "4Gi",
			},
			wantNanoCPUs: 2000 * 1e6,
			wantMemory:   4 * 1024 * 1024 * 1024,
		},
		{
			name:       "cpu_only_override",
			defaultCPU: "1000m",
			defaultMem: "2Gi",
			override: &ResourceRequirements{
				CPULimit: "500m",
			},
			wantNanoCPUs: 500 * 1e6,
			wantMemory:   2 * 1024 * 1024 * 1024,
		},
		{
			name:       "memory_only_override",
			defaultCPU: "1000m",
			defaultMem: "2Gi",
			override: &ResourceRequirements{
				MemoryLimit: "512Mi",
			},
			wantNanoCPUs: 1000 * 1e6,
			wantMemory:   512 * 1024 * 1024,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotNanoCPUs, gotMemory, err := mergeDockerResourceOverrides(tc.defaultCPU, tc.defaultMem, tc.override)
			if err != nil {
				t.Fatalf("mergeDockerResourceOverrides failed: %v", err)
			}
			if gotNanoCPUs != tc.wantNanoCPUs {
				t.Errorf("nanoCPUs = %d, want %d", gotNanoCPUs, tc.wantNanoCPUs)
			}
			if gotMemory != tc.wantMemory {
				t.Errorf("memory = %d, want %d", gotMemory, tc.wantMemory)
			}
		})
	}
}

func TestNormalizeDockerStatus(t *testing.T) {
	cases := []struct {
		input    string
		expected WorkerStatus
	}{
		{"running", StatusRunning},
		{"Running", StatusRunning},
		{"exited", StatusStopped},
		{"dead", StatusStopped},
		{"created", StatusStarting},
		{"restarting", StatusStarting},
		{"paused", StatusUnknown},
		{"", StatusUnknown},
	}
	for _, tc := range cases {
		got := normalizeDockerStatus(tc.input)
		if got != tc.expected {
			t.Errorf("normalizeDockerStatus(%q) = %s, want %s", tc.input, got, tc.expected)
		}
	}
}

// TestDockerCreateResolvesImageFromRuntime verifies that the backend selects
// the correct image based on req.Runtime when req.Image is empty, and that an
// empty req.Runtime resolves to the caller-provided RuntimeFallback (which
// the worker / manager reconciler populates from
// AGENTTEAMS_DEFAULT_WORKER_RUNTIME / AGENTTEAMS_MANAGER_RUNTIME respectively).
func TestDockerCreateResolvesImageFromRuntime(t *testing.T) {
	cases := []struct {
		name      string
		runtime   string // CreateRequest.Runtime
		fallback  string // CreateRequest.RuntimeFallback
		wantImage string
	}{
		{"explicit_copaw_uses_copaw_image", RuntimeCopaw, "", "agentteams/copaw-worker:latest"},
		{"explicit_hermes_uses_hermes_image", RuntimeHermes, "", "agentteams/hermes-worker:latest"},
		{"explicit_qwenpaw_uses_qwenpaw_image", RuntimeQwenPaw, "", "agentteams/qwenpaw-worker:latest"},
		{"explicit_openclaw_uses_worker_image", RuntimeOpenClaw, "", "agentteams/worker-agent:latest"},
		{"empty_runtime_with_no_fallback_uses_worker_image", "", "", "agentteams/worker-agent:latest"},
		{"empty_runtime_with_copaw_fallback_uses_copaw_image", "", RuntimeCopaw, "agentteams/copaw-worker:latest"},
		{"empty_runtime_with_hermes_fallback_uses_hermes_image", "", RuntimeHermes, "agentteams/hermes-worker:latest"},
		{"empty_runtime_with_qwenpaw_fallback_uses_qwenpaw_image", "", RuntimeQwenPaw, "agentteams/qwenpaw-worker:latest"},
		{"explicit_runtime_overrides_fallback", RuntimeOpenClaw, RuntimeHermes, "agentteams/worker-agent:latest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturedImages := captureCreateImagesServer(t)
			defer capturedImages.srv.Close()

			b := &DockerBackend{
				config: DockerConfig{
					WorkerImage:        "agentteams/worker-agent:latest",
					CopawWorkerImage:   "agentteams/copaw-worker:latest",
					HermesWorkerImage:  "agentteams/hermes-worker:latest",
					QwenPawWorkerImage: "agentteams/qwenpaw-worker:latest",
					DefaultNetwork:     "hiclaw-net",
				},
				containerPrefix: "agentteams-worker-",
				client: &http.Client{
					Transport: &testTransport{serverURL: capturedImages.srv.URL},
				},
			}

			_, err := b.Create(context.Background(), CreateRequest{
				Name:            "x",
				Runtime:         tc.runtime,
				RuntimeFallback: tc.fallback,
			})
			if err != nil {
				t.Fatalf("Create failed: %v", err)
			}
			if got := capturedImages.lastImage(); got != tc.wantImage {
				t.Fatalf("create body Image = %q, want %q", got, tc.wantImage)
			}
		})
	}
}
