package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestManagerReadinessUsesModelConfiguredAfterInstallation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "dashboard-model" {
			t.Errorf("readiness still probes install-time model: %s", request.Model)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	p := &Provisioner{aiGatewayURL: upstream.URL, managerModel: "install-time-model"}
	ready, err := p.IsManagerLLMAuthReady(context.Background(), "synthetic-key", "dashboard-model")
	if err != nil || !ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
}
