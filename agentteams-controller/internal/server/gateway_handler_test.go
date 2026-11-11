package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
)

type consumerGateway struct {
	gateway.Client
	requests  []gateway.ConsumerRequest
	status    string
	consumers []gateway.ConsumerSummary
}

func (g *consumerGateway) EnsureConsumer(_ context.Context, req gateway.ConsumerRequest) (*gateway.ConsumerResult, error) {
	g.requests = append(g.requests, req)
	status := g.status
	if status == "" {
		status = "created"
	}
	return &gateway.ConsumerResult{Status: status, APIKey: req.CredentialKey}, nil
}
func TestCreateConsumerGeneratesUniqueKeys(t *testing.T) {
	g := &consumerGateway{}
	h := NewGatewayHandler(g)
	for _, name := range []string{"first", "second"} {
		w := httptest.NewRecorder()
		h.CreateConsumer(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"`+name+`"}`)))
		if w.Code != http.StatusCreated {
			t.Fatalf("status=%d", w.Code)
		}
		var result ConsumerResponse
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.APIKey) != 64 || result.APIKey != g.requests[len(g.requests)-1].CredentialKey {
			t.Fatal("generated key must reach the gateway and response")
		}
	}
	if g.requests[0].CredentialKey == g.requests[1].CredentialKey {
		t.Fatal("keys must be unique")
	}
}
func TestCreateConsumerPreservesProvidedKey(t *testing.T) {
	g := &consumerGateway{}
	w := httptest.NewRecorder()
	NewGatewayHandler(g).CreateConsumer(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"custom","credential_key":"existing-key"}`)))
	if w.Code != http.StatusCreated || g.requests[0].CredentialKey != "existing-key" {
		t.Fatal("explicit key changed")
	}
}

func (g *consumerGateway) ListConsumers(_ context.Context) ([]gateway.ConsumerSummary, error) {
	return g.consumers, nil
}
func TestListConsumers(t *testing.T) {
	w := httptest.NewRecorder()
	NewGatewayHandler(&consumerGateway{consumers: []gateway.ConsumerSummary{{Name: "first"}}}).ListConsumers(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `[{"name":"first"}]` {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
	}
}
func TestListConsumersUnsupported(t *testing.T) {
	w := httptest.NewRecorder()
	NewGatewayHandler(nil).ListConsumers(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCreateConsumerExistingNameDoesNotReturnNewCredential(t *testing.T) {
	g := &consumerGateway{status: "exists"}
	w := httptest.NewRecorder()
	NewGatewayHandler(g).CreateConsumer(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"existing"}`)))
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), g.requests[0].CredentialKey) {
		t.Fatalf("incorrect conflict response: %d", w.Code)
	}
}

func TestCreateConsumerRejectsExistingNameBeforeUpstreamWrite(t *testing.T) {
	g := &consumerGateway{consumers: []gateway.ConsumerSummary{{Name: "existing"}}}
	w := httptest.NewRecorder()
	NewGatewayHandler(g).CreateConsumer(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"existing"}`)))
	if w.Code != http.StatusConflict || len(g.requests) != 0 {
		t.Fatalf("creation overwrote an existing Consumer: status=%d writes=%d", w.Code, len(g.requests))
	}
}
