package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Exercise the production constructor, handler and config manager. The worker
// filesystem and Kubernetes objects are isolated; runtime hot-apply is not tested.
func TestNativeModelGrantWriteAndRollback(t *testing.T) {
	for _, action := range []string{"write", "rollback"} {
		for _, tc := range []struct {
			name, caller, target, code string
			status                     int
		}{
			{"ungranted", "alice", `{"agents":{"defaults":{"model":{"primary":"other/denied"}}}}`, "model_not_allowed", 403},
			{"caller-grant", "alice", `{"agents":{"defaults":{"model":{"primary":"gw/granted"}}}}`, "", 200},
			{"other-human", "bob", `{"agents":{"defaults":{"model":{"primary":"gw/granted"}}}}`, "model_not_allowed", 403},
			{"provider-definition", "alice", `{"models":{"providers":{"custom":{"baseUrl":"https://example.invalid/v1"}}}}`, "native_config_definition_l1_only", 403},
			{"model-catalog-definition", "alice", `{"models":{"providers":{"custom":{"models":[{"id":"granted","baseUrl":"https://example.invalid/v1"}]}}}}`, "native_config_definition_l1_only", 403},
			{"ambiguous-provider", "alice", `{"agents":{"defaults":{"model":{"primary":"granted"}}}}`, "model_pair_unresolvable", 403},
			{"granted-fallback-preserves-current", "alice", `{"agents":{"defaults":{"model":{"fallbacks":["gw/granted"]}}}}`, "", 200},
			{"fallback", "alice", `{"agents":{"defaults":{"model":{"fallbacks":["other/denied"]}}}}`, "model_not_allowed", 403},
			{"subagent", "alice", `{"agents":{"defaults":{"subagents":{"model":"other/denied"}}}}`, "model_not_allowed", 403},
			{"unchanged", "bob", `{"agents":{"defaults":{"model":{"primary":"agentteams-gateway/qwen3-max"}}}}`, "", 200},
			{"non-model", "bob", `{"agents":{"defaults":{"maxConcurrent":2}}}`, "", 200},
		} {
			t.Run(action+"/"+tc.name, func(t *testing.T) {
				eb := newMemberExec()
				store := ossfake.NewMemory()
				objects := checkpointTeamWithWorkers("team-a", "daily-carol")
				for _, object := range objects {
					if worker, ok := object.(*v1beta1.Worker); ok {
						worker.Annotations = map[string]string{modelGrantAnnotation: `[{"provider":"other","model":"denied"}]`}
					}
				}
				for _, name := range []string{"alice", "bob"} {
					human := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
					if name == "alice" {
						human.Spec.ModelAccess = &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{Role: "worker", Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "granted"}}}}}}
					}
					objects = append(objects, human)
				}
				h := newTestMemberConfigHandler(t, "embedded", eb, store, objects...)
				var patch map[string]any
				if err := json.Unmarshal([]byte(tc.target), &patch); err != nil {
					t.Fatal(err)
				}
				method, path, body := http.MethodPut, "/config", `{"patch":`+tc.target+`}`
				if action == "rollback" {
					current := memberConfigDoc(t, memberConfigInitial)
					target, err := json.Marshal(codingCliDeepMerge(current, patch))
					if err != nil {
						t.Fatal(err)
					}
					rev := revOfBytes(t, string(target))
					if err := store.PutObject(context.Background(), "agents/daily-carol/config/previous/"+rev+".json", target); err != nil {
						t.Fatal(err)
					}
					method, path, body = http.MethodPost, "/config/rollback?rev="+rev, ""
				}
				r := withCaller(codingCliRequest(method, path, body, "name", "daily-carol"), &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: tc.caller, Teams: []string{"team-a"}})
				response := httptest.NewRecorder()
				if action == "write" {
					h.putConfig(response, r)
				} else {
					h.rollbackConfig(response, r)
				}
				want, code := tc.status, tc.code
				if action == "write" && tc.name == "model-catalog-definition" {
					want, code = 422, "unknown_field"
				}
				if response.Code != want || !strings.Contains(response.Body.String(), code) {
					t.Fatalf("status=%d want=%d body=%s", response.Code, want, response.Body)
				}
				if want == 403 || want == 422 {
					if eb.fs[memberConfigPath] != memberConfigInitial {
						t.Fatal("denied write changed file")
					}
					if store.Stat(context.Background(), "agents/daily-carol/config/previous/"+revOfBytes(t, memberConfigInitial)+".json") == nil {
						t.Fatal("denied write created snapshot")
					}
				}
			})
		}
	}
}
