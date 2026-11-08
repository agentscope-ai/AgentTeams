package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
)

// aiRoutesListStub implements only ListAIRoutes; any other interface call
// panics via the embedded nil interface, so a test that reaches the wrong
// method fails loudly.
type aiRoutesListStub struct {
	gateway.Client
	routes []gateway.AIRouteInfo
	err    error
}

func (s *aiRoutesListStub) ListAIRoutes(ctx context.Context) ([]gateway.AIRouteInfo, error) {
	return s.routes, s.err
}

// defaultDeploymentRoutes is the standard deployment shape: one
// default-ai-route (path prefix /v1) whose single upstream provider serves
// several models — the route name must come back verbatim as a route name,
// with no model-catalog semantics implied or manufactured.
func defaultDeploymentRoutes() []gateway.AIRouteInfo {
	return []gateway.AIRouteInfo{
		{
			Name:             "default-ai-route",
			Upstreams:        []gateway.AIRouteUpstream{{Provider: "sglang-local", Weight: 100}},
			AllowedConsumers: []string{"manager", "worker-sysdev-lead"},
		},
	}
}

func TestGatewayHandler_ListAIRoutes(t *testing.T) {
	cases := []struct {
		name       string
		gw         gateway.Client
		nilGW      bool
		wantStatus int
		wantBody   string
	}{
		{
			name: "standard deployment: default-ai-route catalog",
			gw:   &aiRoutesListStub{routes: defaultDeploymentRoutes()},
			wantStatus: http.StatusOK,
			// The response is a route catalog: routes, not models.
			wantBody: `"routes":[{"name":"default-ai-route"`,
		},
		{
			name:       "empty catalog",
			gw:         &aiRoutesListStub{routes: nil},
			wantStatus: http.StatusOK,
			wantBody:   `"routes":[]`,
		},
		{
			name:       "unsupported backend",
			gw:         &aiRoutesListStub{err: gateway.ErrUnsupportedOp},
			wantStatus: http.StatusNotImplemented,
			wantBody:   "not supported",
		},
		{
			name:       "upstream error",
			gw:         &aiRoutesListStub{err: errors.New("boom")},
			wantStatus: http.StatusBadGateway,
			wantBody:   "boom",
		},
		{
			name:       "no gateway configured",
			nilGW:      true,
			wantStatus: http.StatusNotImplemented,
			wantBody:   "no gateway backend available",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var h *GatewayHandler
			if tc.nilGW {
				h = &GatewayHandler{}
			} else {
				h = NewGatewayHandler(tc.gw)
			}
			rec := httptest.NewRecorder()
			h.ListAIRoutes(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gateway/ai-routes", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %s, want to contain %s", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestGatewayHandler_ListAIRoutes_RouteNameIsNotAModelID pins the contract
// distinction the review required: the default deployment's single route
// (default-ai-route, several models behind it) must surface its route name
// verbatim under "routes" — the endpoint neither renames routes into model
// aliases nor reports a model list.
func TestGatewayHandler_ListAIRoutes_RouteNameIsNotAModelID(t *testing.T) {
	h := NewGatewayHandler(&aiRoutesListStub{routes: defaultDeploymentRoutes()})
	rec := httptest.NewRecorder()
	h.ListAIRoutes(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gateway/ai-routes", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"models"`) {
		t.Fatalf("route catalog response must not carry a \"models\" field: %s", body)
	}
	if !strings.Contains(body, `"name":"default-ai-route"`) {
		t.Fatalf("route name must be returned verbatim: %s", body)
	}
	if !strings.Contains(body, `"total":1`) {
		t.Fatalf("total must count routes: %s", body)
	}
}
