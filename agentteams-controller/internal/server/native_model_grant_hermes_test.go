package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The real Hermes file commands run against an owned temporary home; this
// verifies native file writes, not a running Hermes process or Matrix login.
type nativeGrantFileExec struct{ home string }

func (e nativeGrantFileExec) Exec(ctx context.Context, _ string, command []string, _ time.Duration) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "HERMES_HOME="+e.home)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return out.String(), stderr.String(), exit.ExitCode(), nil
	}
	return out.String(), stderr.String(), 0, err
}

func (e nativeGrantFileExec) WriteFile(_ context.Context, _, path, body string) error {
	return os.WriteFile(path, []byte(body), 0600)
}

func TestNativeModelGrantHermesRealFiles(t *testing.T) {
	for _, action := range []string{"write", "rollback"} {
		for _, tc := range []struct {
			name, user, body, target string
			status                   int
		}{
			{"granted", "alice", `{"model":{"default":"granted","provider":"gw"}}`, "model:\n  default: granted\n  provider: gw\n", 200},
			{"other-human", "bob", `{"model":{"default":"granted","provider":"gw"}}`, "model:\n  default: granted\n  provider: gw\n", 403},
			{"provider", "alice", `{"model":{"provider":"other"}}`, "model:\n  default: old\n  provider: other\n", 403},
			{"definition", "alice", `{"model":{"base_url":"https://example.invalid"}}`, "model:\n  default: old\n  provider: gw\n  base_url: https://example.invalid\n", 403},
			{"delegation", "alice", `{"delegation":{"model":"denied","provider":"other"}}`, "model:\n  default: old\n  provider: gw\ndelegation:\n  model: denied\n  provider: other\n", 403},
			{"custom-is-not-provider-id", "alice", `{"model":{"default":"granted","provider":"custom"}}`, "model:\n  default: granted\n  provider: custom\n", 403},
			{"unresolved", "alice", `{"model":{"default":"granted","provider":"auto"}}`, "model:\n  default: granted\n  provider: auto\n", 403},
			{"unchanged", "bob", `{"model":{"default":"old","provider":"gw"}}`, "model:\n  default: old\n  provider: gw\n", 200},
			{"non-model", "bob", `{"agent":{"max_turns":10}}`, "model:\n  default: old\n  provider: gw\nagent:\n  max_turns: 10\n", 200},
		} {
			t.Run(action+"/"+tc.name, func(t *testing.T) {
				home := t.TempDir()
				path := filepath.Join(home, "config.yaml")
				initial := "model:\n  default: old\n  provider: gw\n"
				if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
					t.Fatal(err)
				}
				store := ossfake.NewMemory()
				objects := checkpointTeamWithWorkers("team-a", "daily-carol")
				for _, obj := range objects {
					if worker, ok := obj.(*v1beta1.Worker); ok {
						worker.Spec.Runtime = "hermes"
					}
				}
				human := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}, Spec: v1beta1.HumanSpec{ModelAccess: &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{Role: "worker", Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "granted"}}}}}}}}
				objects = append(objects, human)
				h := newTestMemberConfigHandler(t, "embedded", nativeGrantFileExec{home}, store, objects...)
				method, url, body := http.MethodPut, "/config", `{"patch":`+tc.body+`}`
				if action == "rollback" {
					rev := revOfBytes(t, tc.target)
					if err := store.PutObject(context.Background(), "agents/daily-carol/config/previous/"+rev+".json", []byte(tc.target)); err != nil {
						t.Fatal(err)
					}
					method, url, body = http.MethodPost, "/config/rollback?rev="+rev, ""
				}
				r := withCaller(codingCliRequest(method, url, body, "name", "daily-carol"), &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: tc.user, Teams: []string{"team-a"}})
				response := httptest.NewRecorder()
				if action == "write" {
					h.putConfig(response, r)
				} else {
					h.rollbackConfig(response, r)
				}
				if response.Code != tc.status {
					t.Fatalf("status=%d want=%d body=%s", response.Code, tc.status, response.Body)
				}
				back, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if tc.status == 403 && (string(back) != initial || store.Stat(context.Background(), "agents/daily-carol/config/previous/"+revOfBytes(t, initial)+".json") == nil) {
					t.Fatal("denied request mutated native file or created snapshot")
				}
				if tc.status == 200 && tc.name == "granted" && !strings.Contains(string(back), "granted") {
					t.Fatal("granted native value not written")
				}
			})
		}
	}
}
