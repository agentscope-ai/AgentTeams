package resources

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

// TestStructFieldCounts pins the frozen counts (58 §1.4 / types.go): 43
// top-level WorkerResponse fields (58's "45" wrongly counted the nested
// port/domain), a 27-field spec view, a 12-field status view and their
// 35-field union.
func TestStructFieldCounts(t *testing.T) {
	tests := []struct {
		name string
		typ  reflect.Type
		want int
	}{
		{"WorkerResponse", reflect.TypeOf(WorkerResponse{}), 43},
		{"WorkerSpecView", reflect.TypeOf(WorkerSpecView{}), 27},
		{"WorkerStatusView", reflect.TypeOf(WorkerStatusView{}), 12},
		{"WorkerSpecStatusView", reflect.TypeOf(WorkerSpecStatusView{}), 35},
	}
	for _, tt := range tests {
		if got := tt.typ.NumField(); got != tt.want {
			t.Fatalf("%s has %d fields, want %d", tt.name, got, tt.want)
		}
	}
}

// fullResponse builds a WorkerResponse with every field populated, so the
// goldens below pin the complete JSON order of all 43 fields.
func fullResponse() WorkerResponse {
	count := 3
	return WorkerResponse{
		EnvEditable:                  true,
		Env:                          map[string]string{"FOO": "bar"},
		Name:                         "w1",
		WorkerName:                   "Worker One",
		Phase:                        "Ready",
		ContainerManaged:             true,
		State:                        "Running",
		Model:                        "qwen3",
		ModelProvider:                "apig-qwen",
		SubagentModel:                "qwen3-mini",
		LlmStreamFirstContentTimeout: "1s",
		LlmStreamIdleTimeout:         "2s",
		LlmMaxRetries:                "3",
		LlmBackoffBase:               "1ms",
		LlmBackoffCap:                "2ms",
		LlmMaxConcurrent:             "4",
		LlmMaxQpm:                    "5",
		LlmRateLimitPause:            "6ms",
		LlmRateLimitJitter:           "7ms",
		LlmAcquireTimeout:            "8s",
		Runtime:                      "copaw",
		RuntimeDeprecated:            true,
		Image:                        "img:1",
		Identity:                     "id",
		Soul:                         "soul",
		Agents:                       "agents",
		Skills:                       []string{"s1", "s2"},
		McpServers:                   json.RawMessage(`[{"name":"mcp1","url":"https://mcp.example.com/mcp","transport":"http"}]`),
		Package:                      "pkg",
		BackendRuntime:               "sandbox",
		ChannelPolicy:                json.RawMessage(`{"groupAllowExtra":["@a:b"]}`),
		ContainerState:               "running",
		MatrixUserID:                 "@w1:mtx",
		RoomID:                       "!room:mtx",
		Message:                      "ok",
		LastActiveAt:                 "2026-01-02T03:04:05.123456Z",
		AgentStatus:                  "idle",
		RunningTaskCount:             &count,
		LastRunAt:                    "2026-01-02T04:05:06Z",
		LastFinishAt:                 "2026-01-02T05:06:07.5Z",
		ExposedPorts:                 []ExposedPortInfo{{Port: 8080, Domain: "w1.example.com"}},
		Team:                         "team-a",
		Role:                         "team_leader",
	}
}

func TestWorkerResponseJSONGolden(t *testing.T) {
	want := `{"envEditable":true,"env":{"FOO":"bar"},"name":"w1","workerName":"Worker One","phase":"Ready","containerManaged":true,"state":"Running","model":"qwen3","modelProvider":"apig-qwen","subagentModel":"qwen3-mini","llmStreamFirstContentTimeout":"1s","llmStreamIdleTimeout":"2s","llmMaxRetries":"3","llmBackoffBase":"1ms","llmBackoffCap":"2ms","llmMaxConcurrent":"4","llmMaxQpm":"5","llmRateLimitPause":"6ms","llmRateLimitJitter":"7ms","llmAcquireTimeout":"8s","runtime":"copaw","runtimeDeprecated":true,"image":"img:1","identity":"id","soul":"soul","agents":"agents","skills":["s1","s2"],"mcpServers":[{"name":"mcp1","url":"https://mcp.example.com/mcp","transport":"http"}],"package":"pkg","backendRuntime":"sandbox","channelPolicy":{"groupAllowExtra":["@a:b"]},"containerState":"running","matrixUserID":"@w1:mtx","roomID":"!room:mtx","message":"ok","lastActiveAt":"2026-01-02T03:04:05.123456Z","agentStatus":"idle","runningTaskCount":3,"lastRunAt":"2026-01-02T04:05:06Z","lastFinishAt":"2026-01-02T05:06:07.5Z","exposedPorts":[{"port":8080,"domain":"w1.example.com"}],"team":"team-a","role":"team_leader"}`
	got, err := json.Marshal(fullResponse())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Fatalf("WorkerResponse JSON drifted.\n got: %s\nwant: %s", got, want)
	}
}

func TestWorkerSpecViewGolden(t *testing.T) {
	want := `{"name":"w1","workerName":"Worker One","containerManaged":true,"state":"Running","model":"qwen3","subagentModel":"qwen3-mini","llmStreamFirstContentTimeout":"1s","llmStreamIdleTimeout":"2s","llmMaxRetries":"3","llmBackoffBase":"1ms","llmBackoffCap":"2ms","llmMaxConcurrent":"4","llmMaxQpm":"5","llmRateLimitPause":"6ms","llmRateLimitJitter":"7ms","llmAcquireTimeout":"8s","runtime":"copaw","runtimeDeprecated":true,"image":"img:1","identity":"id","package":"pkg","backendRuntime":"sandbox","matrixUserID":"@w1:mtx","roomID":"!room:mtx","exposedPorts":[{"port":8080,"domain":"w1.example.com"}],"team":"team-a","role":"team_leader"}`
	got, err := json.Marshal(fullResponse().SpecView())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Fatalf("WorkerSpecView JSON drifted.\n got: %s\nwant: %s", got, want)
	}
}

func TestWorkerStatusViewGolden(t *testing.T) {
	want := `{"name":"w1","workerName":"Worker One","phase":"Ready","containerState":"running","message":"ok","lastActiveAt":"2026-01-02T03:04:05.123456Z","agentStatus":"idle","runningTaskCount":3,"lastRunAt":"2026-01-02T04:05:06Z","lastFinishAt":"2026-01-02T05:06:07.5Z","team":"team-a","role":"team_leader"}`
	got, err := json.Marshal(fullResponse().StatusView())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Fatalf("WorkerStatusView JSON drifted.\n got: %s\nwant: %s", got, want)
	}
}

func TestWorkerSpecStatusViewGolden(t *testing.T) {
	want := `{"name":"w1","workerName":"Worker One","phase":"Ready","containerManaged":true,"state":"Running","model":"qwen3","subagentModel":"qwen3-mini","llmStreamFirstContentTimeout":"1s","llmStreamIdleTimeout":"2s","llmMaxRetries":"3","llmBackoffBase":"1ms","llmBackoffCap":"2ms","llmMaxConcurrent":"4","llmMaxQpm":"5","llmRateLimitPause":"6ms","llmRateLimitJitter":"7ms","llmAcquireTimeout":"8s","runtime":"copaw","runtimeDeprecated":true,"image":"img:1","identity":"id","package":"pkg","backendRuntime":"sandbox","containerState":"running","matrixUserID":"@w1:mtx","roomID":"!room:mtx","message":"ok","lastActiveAt":"2026-01-02T03:04:05.123456Z","agentStatus":"idle","runningTaskCount":3,"lastRunAt":"2026-01-02T04:05:06Z","lastFinishAt":"2026-01-02T05:06:07.5Z","exposedPorts":[{"port":8080,"domain":"w1.example.com"}],"team":"team-a","role":"team_leader"}`
	got, err := json.Marshal(fullResponse().SpecStatusView())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Fatalf("WorkerSpecStatusView JSON drifted.\n got: %s\nwant: %s", got, want)
	}
}

func TestResponseProjectionRules(t *testing.T) {
	active := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	count := 3
	w := &Worker{
		Name:                         "w1",
		WorkerName:                   "Worker One",
		TeamSlug:                     "team-a",
		Model:                        "qwen3",
		ModelProvider:                "apig-qwen",
		SubagentModel:                "qwen3-mini",
		LLMStreamFirstContentTimeout: "1s",
		Runtime:                      "CoPaw",
		PackageRef:                   "pkg",
		Skills:                       []string{"s1"},
		MCPServers:                   json.RawMessage(`[{"name":"m","url":"https://mcp.example.com/mcp"}]`),
		Expose:                       json.RawMessage(`[{"port":8080,"protocol":"http"}]`),
		ChannelPolicy:                json.RawMessage(`{"groupAllowExtra":["@a:b"]}`),
		Env:                          map[string]string{"FOO": "bar"},
		ContainerManaged:             false,
		ObservedContainerState:       "running",
		MatrixUserID:                 "@w1:mtx",
		RoomID:                       "!room:mtx",
		Message:                      "ok",
		LastActiveAt:                 &active,
		AgentStatus:                  "idle",
		RunningTaskCount:             &count,
		LastRunAt:                    &active,
	}

	human := w.Response(&identity.Identity{Role: identity.RoleHuman})
	if human.EnvEditable || human.Env != nil {
		t.Fatalf("non-manager must not see env: editable=%v env=%v", human.EnvEditable, human.Env)
	}
	if human.Phase != "Pending" {
		t.Fatalf("empty phase = %q, want Pending", human.Phase)
	}
	if human.State != "Running" {
		t.Fatalf("empty desired state = %q, want Running", human.State)
	}
	if !human.RuntimeDeprecated {
		t.Fatal("copaw (case-insensitive) must be RuntimeDeprecated")
	}
	if human.Team != "team-a" {
		t.Fatalf("team = %q, want team-a", human.Team)
	}
	if human.Role != "" {
		t.Fatalf("role = %q, want empty in R1", human.Role)
	}
	if human.ModelProvider != "apig-qwen" {
		t.Fatalf("modelProvider = %q", human.ModelProvider)
	}
	// R1 has no observed-ports column: exposedPorts is always omitted, as the
	// controller does before reconcile populates status.exposedPorts (58 §11 I14).
	if human.ExposedPorts != nil {
		t.Fatalf("exposedPorts = %+v, want nil", human.ExposedPorts)
	}
	if human.LastActiveAt != "2026-01-02T03:04:05.123456Z" {
		t.Fatalf("lastActiveAt = %q", human.LastActiveAt)
	}
	if human.LastFinishAt != "" {
		t.Fatalf("nil lastFinishAt = %q, want empty", human.LastFinishAt)
	}

	admin := w.Response(&identity.Identity{Role: identity.RoleAdmin})
	if !admin.EnvEditable || admin.Env["FOO"] != "bar" {
		t.Fatalf("admin env: editable=%v env=%v", admin.EnvEditable, admin.Env)
	}

	unauth := w.Response(nil)
	if unauth.EnvEditable || unauth.Env != nil {
		t.Fatal("nil caller must not see env")
	}
}

func TestResponseBytesTrailingNewline(t *testing.T) {
	w := &Worker{Name: "w1", Runtime: "qwenpaw"}
	got, err := w.ResponseBytes(&identity.Identity{Role: identity.RoleManager})
	if err != nil {
		t.Fatalf("ResponseBytes: %v", err)
	}
	if len(got) == 0 || got[len(got)-1] != '\n' {
		t.Fatalf("ResponseBytes must end with a newline: %q", got)
	}
	var round WorkerResponse
	if err := json.Unmarshal(bytes.TrimSuffix(got, []byte{'\n'}), &round); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if round.Name != "w1" || round.Phase != "Pending" {
		t.Fatalf("round-trip = %+v", round)
	}
}
