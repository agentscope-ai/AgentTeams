package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpdateManagerCanClearModelProvider(t *testing.T) {
	var body map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/managers/default" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)

	cmd := updateManagerCmd()
	cmd.SetArgs([]string{"--name", "default", "--model", "known-good-model", "--model-provider="})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute update manager: %v", err)
	}

	if got := body["model"]; got != "known-good-model" {
		t.Fatalf("model=%v, want known-good-model", got)
	}
	provider, ok := body["modelProvider"]
	if !ok {
		t.Fatal("modelProvider is missing from request")
	}
	if provider != "" {
		t.Fatalf("modelProvider=%v, want empty", provider)
	}
}
