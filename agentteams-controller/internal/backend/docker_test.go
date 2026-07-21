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
