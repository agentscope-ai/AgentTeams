package server

import (
	"bytes"
	"context"
	"encoding/json"
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

// The Hermes bridge projects a gateway provider as "custom". Exercise the
// production PUT and rollback paths with the controller's pre-bridge config.
func TestNativeModelGrantHermesBridgeBinding(t *testing.T) {
	for _, action := range []string{"write", "rollback"} {
		for _, tc := range []struct {
			name, user, apiKey, desiredProvider, desiredEndpoint                            string
			missing, renamed, defaultProvider, envKey, spawn, customDefinition, otherWorker bool
			status                                                                          int
		}{
			{name: "bound-main", user: "alice", status: 200},
			{name: "bound-runtime-name", user: "alice", renamed: true, status: 200},
			{name: "default-provider", user: "alice", defaultProvider: true, status: 200},
			{name: "bridge-env-key", user: "alice", envKey: true, status: 200},
			{name: "spawn-inherits-route", user: "alice", spawn: true, status: 200},
			{name: "spawn-other-human", user: "bob", spawn: true, status: 403},
			{name: "custom-definition", user: "alice", customDefinition: true, status: 403},
			{name: "other-worker-binding", user: "alice", otherWorker: true, status: 403},
			{name: "different-human", user: "bob", status: 403},
			{name: "missing-binding", user: "alice", missing: true, status: 403},
			{name: "different-provider", user: "alice", desiredProvider: "other", status: 403},
			{name: "different-endpoint", user: "alice", desiredEndpoint: "https://other.invalid/v1", status: 403},
			{name: "different-credential", user: "alice", apiKey: "other-key", status: 403},
		} {
			t.Run(action+"/"+tc.name, func(t *testing.T) {
				home := t.TempDir()
				path := filepath.Join(home, "config.yaml")
				endpoint := "https://gateway.invalid/v1"
				key := "fixture-key"
				if tc.apiKey != "" {
					key = tc.apiKey
				}
				initial := "model:\n  default: old\n  provider: custom\n  base_url: " + endpoint + "\n  api_key: " + key + "\n"
				if tc.envKey {
					initial = strings.Replace(initial, "  api_key: "+key+"\n", "", 1)
				}
				if tc.customDefinition {
					initial += "providers:\n  custom:\n    base_url: https://other.invalid/v1\n"
				}
				if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
					t.Fatal(err)
				}
				store := ossfake.NewMemory()
				objects := checkpointTeamWithWorkers("team-a", "daily-carol")
				runtimeName := "daily-carol"
				if tc.renamed {
					runtimeName = "carol-runtime"
				}
				for _, obj := range objects {
					if worker, ok := obj.(*v1beta1.Worker); ok {
						worker.Spec.Runtime = "hermes"
						worker.Spec.WorkerName = runtimeName
						worker.Spec.Model = "old"
						worker.Spec.ModelProvider = "gw"
						if tc.defaultProvider {
							worker.Spec.ModelProvider = ""
						}
					}
				}
				provider := "gw"
				if tc.defaultProvider {
					provider = "agentteams-gateway"
				}
				if tc.desiredProvider != "" {
					provider = tc.desiredProvider
				}
				desiredEndpoint := endpoint
				if tc.desiredEndpoint != "" {
					desiredEndpoint = tc.desiredEndpoint
				}
				desired := map[string]any{"agents": map[string]any{"defaults": map[string]any{"model": map[string]any{"primary": provider + "/old"}}}, "models": map[string]any{"providers": map[string]any{provider: map[string]any{"baseUrl": desiredEndpoint, "apiKey": "fixture-key"}}}}
				data, err := json.Marshal(desired)
				if err != nil {
					t.Fatal(err)
				}
				if !tc.missing {
					if err := store.PutObject(context.Background(), "agents/"+func() string {
						if tc.otherWorker {
							return "other-worker"
						}
						return runtimeName
					}()+"/openclaw.json", data); err != nil {
						t.Fatal(err)
					}
				}
				human := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}, Spec: v1beta1.HumanSpec{ModelAccess: &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{Role: "worker", Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "granted"}}}}}}}}
				if tc.defaultProvider {
					human.Spec.ModelAccess.Scopes[0].Main.Entries[0].Provider = "agentteams-gateway"
				}
				if tc.spawn {
					human.Spec.ModelAccess.Scopes[0].Spawn = human.Spec.ModelAccess.Scopes[0].Main
				}
				objects = append(objects, human)
				h := newTestMemberConfigHandler(t, "embedded", nativeGrantFileExec{home}, store, objects...)
				method, url, body := http.MethodPut, "/config", `{"patch":{"model":{"default":"granted"}}}`
				if tc.spawn {
					body = `{"patch":{"delegation":{"model":"granted","provider":"custom"}}}`
				}
				if action == "rollback" {
					target := strings.Replace(initial, "default: old", "default: granted", 1)
					if tc.spawn {
						target = initial + "delegation:\n  model: granted\n  provider: custom\n"
					}
					rev := revOfBytes(t, target)
					if err := store.PutObject(context.Background(), "agents/daily-carol/config/previous/"+rev+".json", []byte(target)); err != nil {
						t.Fatal(err)
					}
					method, url, body = http.MethodPost, "/config/rollback?rev="+rev, ""
				}
				req := withCaller(codingCliRequest(method, url, body, "name", "daily-carol"), &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: tc.user, Teams: []string{"team-a"}})
				res := httptest.NewRecorder()
				if action == "write" {
					h.putConfig(res, req)
				} else {
					h.rollbackConfig(res, req)
				}
				if res.Code != tc.status {
					t.Fatalf("status=%d want=%d body=%s", res.Code, tc.status, res.Body)
				}
				back, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if tc.status == 403 && (string(back) != initial || store.Stat(context.Background(), "agents/daily-carol/config/previous/"+revOfBytes(t, initial)+".json") == nil) {
					t.Fatal("denied alias request changed config or snapshot")
				}
				if tc.status == 200 && !strings.Contains(string(back), "granted") {
					t.Fatal("authorized model was not written")
				}
			})
		}
	}
}
