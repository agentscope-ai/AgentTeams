package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/modelhealth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type modelHealthTestManager struct {
	ctrl.Manager
	client client.Client
}

func (m modelHealthTestManager) GetClient() client.Client { return m.client }

func TestServiceLayerManagerPreflightUsesConfiguredGateway(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer dummy-manager-key" {
			t.Errorf("unexpected probe request: %s", r.URL.Path)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()
	ctx := context.Background()
	store := ossfake.NewMemory()
	a := &App{
		cfg: &config.Config{KubeMode: "incluster", WorkerEnv: config.WorkerEnvDefaults{AIGatewayURL: upstream.URL}},
		mgr: modelHealthTestManager{client: fake.NewClientBuilder().Build()},
		oss: store, agentGen: agentconfig.NewGenerator(agentconfig.Config{}),
	}
	if err := a.initServiceLayer(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.deployer.DeployManagerConfig(ctx, service.ManagerDeployRequest{
		Name: "default", Spec: v1beta1.ManagerSpec{Runtime: "qwenpaw", Model: "fixture-model"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.deployer.CheckManagerModel(ctx, "qwenpaw", "dummy-manager-key", "", "", nil); err != nil {
		t.Fatal(err)
	}
	data, err := store.GetObject(ctx, "agents/manager/model-health.json")
	if err != nil {
		t.Fatal(err)
	}
	var state modelhealth.State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || state.Status != "ok" {
		t.Fatalf("configured gateway not checked: calls=%d state=%+v", calls, state)
	}
}
