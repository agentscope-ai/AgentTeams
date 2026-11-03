package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Keep argv intact; settings values and shell metacharacters must not become
// another shell command when adding container-side timeout supervision.
func TestDockerExecTimeoutPreservesArgumentsAndExitCode(t *testing.T) {
	command := []string{"printf", "%s", "a b; $(echo unexpected) 'quoted'"}
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /containers/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ Cmd []string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		got = payload.Cmd
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": "exec-id"})
	})
	mux.HandleFunc("POST /exec/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /exec/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"Running": false, "ExitCode": 137})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	b := newTestDockerBackend(t, srv.URL)
	_, _, code, err := b.Exec(context.Background(), "alice", command, 150*time.Millisecond)
	if err == nil || code != 137 {
		t.Fatalf("exit=%d err=%v, want termination error with exit 137", code, err)
	}
	want := append([]string{"timeout", "--signal=KILL", "--", "0.150000000s"}, command...)
	if !slicesEqual(got, want) {
		t.Fatalf("container argv=%q, want %q", got, want)
	}
}

// Opt in with a dedicated disposable fixture; this never chooses a live Worker.
func TestDockerExecTimeoutStopsProcessGroup(t *testing.T) {
	socket := os.Getenv("CODING_CLI_TEST_DOCKER_SOCKET")
	container := os.Getenv("CODING_CLI_TEST_DOCKER_CONTAINER")
	if socket == "" || container == "" {
		t.Skip("requires CODING_CLI_TEST_DOCKER_SOCKET and a disposable CODING_CLI_TEST_DOCKER_CONTAINER with GNU timeout")
	}
	b := NewDockerBackend(DockerConfig{SocketPath: socket}, "")
	for _, mode := range []string{"normal", "parent-ignores-TERM", "child-ignores-TERM"} {
		t.Run(mode, func(t *testing.T) {
			file := fmt.Sprintf("/tmp/timeout-result-%d", time.Now().UnixNano())
			command := "sleep 0.5; echo survived > " + file
			wait := 600 * time.Millisecond
			if mode == "parent-ignores-TERM" {
				command = "trap '' TERM; (sleep 1.5; echo survived > " + file + ") & wait"
				wait = 1600 * time.Millisecond
			} else if mode == "child-ignores-TERM" {
				command = "(trap '' TERM; sleep 0.5; echo survived > " + file + ") & wait"
			}
			_, _, code, err := b.Exec(context.Background(), container, []string{"sh", "-c", command}, 50*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), "exit 137") {
				t.Fatalf("container timeout must finish before the HTTP deadline: %v", err)
			}
			if code != 137 {
				t.Fatalf("timeout exit=%d, want 137", code)
			}
			time.Sleep(wait)
			_, _, code, err = b.Exec(context.Background(), container, []string{"test", "-e", file}, time.Second)
			if err != nil || code != 1 {
				t.Fatalf("timed-out process wrote a late file: exit=%d err=%v", code, err)
			}
		})
	}
}
