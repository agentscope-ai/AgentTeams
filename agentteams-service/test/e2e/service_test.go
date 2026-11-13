//go:build e2e

// Package e2e runs the real agentteams-service binary against a real MySQL
// schema and a fake controller (the controller's /internal/v1 routes are
// R1-2e; the fake implements the 58 §5 contract). It covers what package
// tests cannot: process boot/shutdown, the wired five-route table, the
// identity middleware, the outbox dispatcher, the status writeback route,
// and the store+handler path over HTTP.
//
// Run:
//
//	AGENTTEAMS_TEST_RES_DSN=... AGENTTEAMS_TEST_OPS_DSN=... \
//	go test -tags e2e -count=1 ./test/e2e/...
package e2e

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const putResponseBody = `{"name":"e2e-w1","workerName":"e2e-w1","phase":"Running","containerManaged":true,"state":"Running","model":"m2","runtime":"qwenpaw"}`

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("service did not become healthy at %s", url)
}

func get(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServiceBinaryEndToEnd(t *testing.T) {
	resDSN, opsDSN := os.Getenv("AGENTTEAMS_TEST_RES_DSN"), os.Getenv("AGENTTEAMS_TEST_OPS_DSN")
	if resDSN == "" || opsDSN == "" {
		t.Skip("e2e: AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN not set")
	}

	// Fake controller: token-review + provision (58 §5) + the public
	// PUT/DELETE the transparent proxy forwards to.
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/token-review":
			_ = json.NewEncoder(w).Encode(map[string]any{"role": "admin", "username": "admin"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/provision"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deprovision"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/execution"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "e2e-w1", "generation": 1, "phase": "Running"})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/workers/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(putResponseBody))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/workers/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer controller.Close()

	// Clean the business rows so the test is repeatable.
	db, err := sql.Open("mysql", resDSN)
	if err != nil {
		t.Fatalf("open res: %v", err)
	}
	defer db.Close()
	for _, stmt := range []string{"DELETE FROM workers", "DELETE FROM ops.outbox", "DELETE FROM ops.audit_events", "DELETE FROM ops.idempotency_keys"} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("clean %q: %v", stmt, err)
		}
	}

	bin := t.TempDir() + "/agentteams-service"
	build := exec.Command("go", "build", "-o", bin, "./cmd/agentteams-service")
	build.Dir = "../.." // test cwd is test/e2e; build from the module root
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"AGENTTEAMS_SERVICE_ENABLED=true",
		"AGENTTEAMS_SERVICE_LISTEN_ADDR="+addr,
		"AGENTTEAMS_SERVICE_RES_DSN="+resDSN,
		"AGENTTEAMS_SERVICE_OPS_DSN="+opsDSN,
		"AGENTTEAMS_SERVICE_CONTROLLER_URL="+controller.URL,
		"AGENTTEAMS_SERVICE_INTERNAL_TOKEN=test-internal",
		"AGENTTEAMS_SERVICE_LOG_LEVEL=error",
		"AGENTTEAMS_SERVICE_OUTBOX_INTERVAL=100ms",
		"AGENTTEAMS_SERVICE_SHUTDOWN_TIMEOUT=3s",
	)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://" + addr
	waitHTTP(t, base+"/healthz")

	resp, err := http.Get(base + "/readyz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %v %v", resp, err)
	}
	resp.Body.Close()

	// --- 401 without a bearer token ---------------------------------
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/workers", strings.NewReader(`{"name":"e2e-w1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create = %v %v", resp, err)
	}
	resp.Body.Close()

	// --- create (native) --------------------------------------------
	req, _ = http.NewRequest(http.MethodPost, base+"/api/v1/workers", strings.NewReader(`{"name":"e2e-w1","model":"m","runtime":"qwenpaw"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer public-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	createBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, createBody)
	}

	// --- list (native) ----------------------------------------------
	code, body := get(t, base+"/api/v1/workers", "public-token")
	if code != http.StatusOK || !strings.Contains(body, `"e2e-w1"`) || !strings.Contains(body, `"total":1`) {
		t.Fatalf("list = %d %s", code, body)
	}

	// --- outbox dispatcher delivered provision -----------------------
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status string
		err := db.QueryRow("SELECT status FROM ops.outbox WHERE worker_name='e2e-w1' AND event_type='provision'").Scan(&status)
		if err == nil && status == "published" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provision event not published (status=%q err=%v)\n%s", status, err, out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// --- PUT through the transparent proxy (byte passthrough) --------
	req, _ = http.NewRequest(http.MethodPut, base+"/api/v1/workers/e2e-w1", strings.NewReader(`{"soul":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer public-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	putBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(putBody) != putResponseBody {
		t.Fatalf("put = %d %q, want %q", resp.StatusCode, putBody, putResponseBody)
	}
	// Write-through: the controller's model m2 is in the service row.
	code, body = get(t, base+"/api/v1/workers/e2e-w1", "public-token")
	if code != http.StatusOK || !strings.Contains(body, `"model":"m2"`) {
		t.Fatalf("write-through not visible: %d %s", code, body)
	}

	// --- status writeback through the internal auth ------------------
	statusBody := `{"workers":[{"name":"e2e-w1","generation":99,"phase":"Running","containerState":"running"}]}`
	req, _ = http.NewRequest(http.MethodPost, base+"/internal/v1/workers/status", strings.NewReader(statusBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without internal auth = %v %v", resp, err)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodPost, base+"/internal/v1/workers/status", strings.NewReader(statusBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-internal")
	req.Header.Set("X-AT-Timestamp", time.Now().UTC().Format(time.RFC3339))
	req.Header.Set("X-AT-Request-Id", "e2e-status-1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	statusResp, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(statusResp), `"updated":1`) {
		t.Fatalf("status = %d %s", resp.StatusCode, statusResp)
	}

	// --- DELETE through the transparent proxy (204 -> soft delete) ---
	req, _ = http.NewRequest(http.MethodDelete, base+"/api/v1/workers/e2e-w1", nil)
	req.Header.Set("Authorization", "Bearer public-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if code, _ := get(t, base+"/api/v1/workers/e2e-w1", "public-token"); code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", code)
	}

	// --- graceful shutdown: SIGTERM -> exit 0 ------------------------
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("service exit: %v\n%s", err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("service did not exit after SIGTERM\n%s", out.String())
	}
}
