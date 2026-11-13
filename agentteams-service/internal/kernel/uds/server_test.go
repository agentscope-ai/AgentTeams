package uds

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httpapi"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/module"
)

func unixClient(path string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}

func testServer(t *testing.T, path string) *Server {
	t.Helper()
	api := httpapi.New(nil)
	api.AddReadyCheck("modules", func(ctx context.Context) module.Health {
		return module.Health{Status: module.StatusOK}
	})
	srv := New(path, api.Handler(), nil)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	})
	return srv
}

func TestUDSHealthzAndReadyz(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentteams.sock")
	testServer(t, path)

	client := unixClient(path)
	for _, route := range []string{"/healthz", "/readyz"} {
		resp, err := client.Get("http://unix" + route)
		if err != nil {
			t.Fatalf("GET %s: %v", route, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", route, resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("GET %s: body: %v", route, err)
		}
		if body["status"] != "ok" {
			t.Fatalf("GET %s: body = %v", route, body)
		}
		resp.Body.Close()
	}
}

func TestUDSSocketPermission0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentteams.sock")
	testServer(t, path)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode() & 0o777; got != 0o600 {
		t.Fatalf("socket mode = %o, want 600", got)
	}
}

func TestUDSRemovesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.sock")
	if err := os.WriteFile(path, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	testServer(t, path)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("path is not a socket: %v", st.Mode())
	}
}

func TestUDSShutdownRemovesSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentteams.sock")
	api := httpapi.New(nil)
	srv := New(path, api.Handler(), nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket file still present (stat err = %v)", err)
	}
}

func TestUDSServeWithoutListen(t *testing.T) {
	srv := New(filepath.Join(t.TempDir(), "x.sock"), httpapi.New(nil).Handler(), nil)
	if err := srv.Serve(); err == nil {
		t.Fatal("Serve before Listen should fail")
	}
}
