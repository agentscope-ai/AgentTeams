package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// listRoutesConsole is a fake Higress console serving an AI route list plus
// per-route details, mirroring the real console's envelope shapes.
func listRoutesConsole(t *testing.T, listStatus int, routeDetails map[string]map[string]interface{}, listEntries []string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/ai/routes" && r.Method == "GET":
			w.WriteHeader(listStatus)
			if listStatus != http.StatusOK {
				return
			}
			entries := make([]map[string]interface{}, 0, len(listEntries))
			for _, name := range listEntries {
				entries = append(entries, map[string]interface{}{"name": name})
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"data": entries})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/ai/routes/"):
			name := strings.TrimPrefix(r.URL.Path, "/v1/ai/routes/")
			detail, ok := routeDetails[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"data": detail})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestListAIRoutes(t *testing.T) {
	client := newGatewayTestClient(listRoutesConsole(t, http.StatusOK, map[string]map[string]interface{}{
		"model-a": {
			"name": "model-a",
			"upstreams": []map[string]interface{}{
				{"provider": "provider-x", "weight": 60},
				{"provider": "provider-y", "weight": 40},
			},
			"authConfig": map[string]interface{}{
				"enabled":          true,
				"allowedConsumers": []string{"manager", "worker-alice"},
			},
		},
		"model-b": {"name": "model-b"},
		"":        {"name": ""},
	}, []string{"model-a", "", "model-b"}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	routes, err := c.ListAIRoutes(context.Background())
	if err != nil {
		t.Fatalf("ListAIRoutes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes (empty-name entry skipped), got %d", len(routes))
	}
	a := routes[0]
	if a.Name != "model-a" {
		t.Errorf("model-a name = %q", a.Name)
	}
	if len(a.Upstreams) != 2 || a.Upstreams[0].Provider != "provider-x" || a.Upstreams[0].Weight != 60 ||
		a.Upstreams[1].Provider != "provider-y" || a.Upstreams[1].Weight != 40 {
		t.Errorf("model-a upstreams = %+v", a.Upstreams)
	}
	if len(a.AllowedConsumers) != 2 || a.AllowedConsumers[0] != "manager" || a.AllowedConsumers[1] != "worker-alice" {
		t.Errorf("model-a allowedConsumers = %+v", a.AllowedConsumers)
	}
	b := routes[1]
	if b.Name != "model-b" || len(b.Upstreams) != 0 || len(b.AllowedConsumers) != 0 {
		t.Errorf("model-b mapping wrong: %+v", b)
	}
}

func TestListAIRoutes_ListError(t *testing.T) {
	client := newGatewayTestClient(listRoutesConsole(t, http.StatusInternalServerError, nil, nil))
	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	if _, err := c.ListAIRoutes(context.Background()); err == nil || !strings.Contains(err.Error(), "list AI routes: HTTP 500") {
		t.Fatalf("expected list HTTP 500 error, got %v", err)
	}
}

func TestListAIRoutes_RouteFetchError(t *testing.T) {
	client := newGatewayTestClient(listRoutesConsole(t, http.StatusOK, map[string]map[string]interface{}{
		"model-a": {"name": "model-a"},
	}, []string{"model-a", "model-missing"}))
	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	if _, err := c.ListAIRoutes(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "get AI route model-missing: HTTP 404") {
		t.Fatalf("expected per-route 404 error, got %v", err)
	}
}

func TestListAIRoutes_Empty(t *testing.T) {
	client := newGatewayTestClient(listRoutesConsole(t, http.StatusOK, nil, nil))
	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	routes, err := c.ListAIRoutes(context.Background())
	if err != nil {
		t.Fatalf("ListAIRoutes: %v", err)
	}
	if routes == nil || len(routes) != 0 {
		t.Fatalf("expected empty non-nil slice, got %#v", routes)
	}
}

func TestAIGatewayClient_ListAIRoutesUnsupported(t *testing.T) {
	a := &AIGatewayClient{}
	if _, err := a.ListAIRoutes(context.Background()); !errors.Is(err, ErrUnsupportedOp) {
		t.Fatalf("expected ErrUnsupportedOp, got %v", err)
	}
}
