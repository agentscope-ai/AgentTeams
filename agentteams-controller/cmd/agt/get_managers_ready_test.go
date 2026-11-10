package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/server"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Exercise the real API -> CLI JSON round trip: readiness polling must not
// silently lose the ready field when decoding a Manager response.
func TestGetManagersJSONPreservesReady(t *testing.T) {
	for _, ready := range []bool{false, true} {
		for _, detail := range []bool{false, true} {
			t.Run(fmtReadyCase(ready, detail), func(t *testing.T) {
				scheme := runtime.NewScheme()
				if err := v1beta1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				manager := &v1beta1.Manager{}
				manager.Name, manager.Namespace = "default", "default"
				manager.Status.Ready = ready
				manager.Status.Phase = "Running"
				client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(manager).Build()
				handler := server.NewResourceHandler(client, "default", nil, "", nil)
				mux := http.NewServeMux()
				mux.HandleFunc("GET /api/v1/managers", handler.ListManagers)
				mux.HandleFunc("GET /api/v1/managers/{name}", handler.GetManager)
				srv := httptest.NewServer(mux)
				defer srv.Close()
				t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)

				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				original := os.Stdout
				os.Stdout = w
				defer func() { os.Stdout = original; r.Close(); w.Close() }()
				cmd := getManagersCmd()
				args := []string{"-o", "json"}
				if detail {
					args = append([]string{"default"}, args...)
				}
				cmd.SetArgs(args)
				err = cmd.Execute()
				w.Close()
				os.Stdout = original
				if err != nil {
					t.Fatal(err)
				}
				output, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(output, &body); err != nil {
					t.Fatal(err)
				}
				if !detail {
					var rows []map[string]json.RawMessage
					if err := json.Unmarshal(body["managers"], &rows); err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 {
						t.Fatalf("got %d managers", len(rows))
					}
					body = rows[0]
				}
				raw, exists := body["ready"]
				if !exists {
					t.Fatalf("ready missing from CLI JSON: %s", output)
				}
				var got bool
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if got != ready {
					t.Fatalf("ready = %v, want %v", got, ready)
				}
			})
		}
	}
}

func fmtReadyCase(ready, detail bool) string {
	name := "not-ready"
	if ready {
		name = "ready"
	}
	if detail {
		return name + "/detail"
	}
	return name + "/list"
}
