package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/modelhealth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHarnessHealthScopesAndValidatesProducerEvidence(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name, worker.Namespace = "alice-cr", "default"
	worker.Spec.WorkerName, worker.Spec.Runtime = "alice", "example"
	store := ossfake.NewMemory()
	h := &harnessHealth{client: fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(worker).Build(), namespace: "default", store: store}
	request := func(method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/", strings.NewReader(`{"url":"http://injected.invalid","credential":"secret"}`))
		r.SetPathValue("name", "alice-cr")
		w := httptest.NewRecorder()
		if method == "GET" {
			h.Get(w, r)
		} else {
			h.Probe(w, r)
		}
		return w
	}
	if got := request("GET"); got.Code != 200 || !strings.Contains(got.Body.String(), `"status":"unknown"`) {
		t.Fatal(got.Code, got.Body.String())
	}
	queued := request("POST")
	if queued.Code != 202 || strings.Contains(queued.Body.String(), `"success":true`) {
		t.Fatal(queued.Code, queued.Body.String())
	}
	data, err := store.GetObject(context.Background(), "agents/alice/runtime/health-request.json")
	if err != nil || strings.Contains(string(data), "injected") || strings.Contains(string(data), "secret") {
		t.Fatal(string(data), err)
	}
	var probe modelhealth.Request
	json.Unmarshal(data, &probe)
	if probe.Version != 1 || len(probe.ID) != 32 || probe.ExpiresAt.Before(time.Now()) {
		t.Fatal(probe)
	}
	report := modelhealth.Report{Version: 1, Runtime: "example", Member: "alice", Status: "ok", Reason: "ok", CheckedAt: time.Now().UTC(), RequestID: probe.ID}
	save := func() {
		b, _ := json.Marshal(report)
		store.PutObject(context.Background(), "agents/alice/runtime/health.json", b)
	}
	save()
	if got := request("GET"); !strings.Contains(got.Body.String(), `"status":"ok"`) {
		t.Fatal(got.Body.String())
	}
	report.Runtime = "foreign"
	save()
	if got := request("GET"); !strings.Contains(got.Body.String(), `"status":"unknown"`) {
		t.Fatal("accepted foreign Harness", got.Body.String())
	}
	report.Runtime = "example"
	report.CheckedAt = time.Now().Add(-time.Hour)
	save()
	if got := request("GET"); !strings.Contains(got.Body.String(), `"reason":"stale_report"`) {
		t.Fatal(got.Body.String())
	}
}
