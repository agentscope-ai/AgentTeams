package initializer

import (
	"context"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
)

type recordedSource struct {
	name, address string
	port          int
	protocol      string
	static        bool
}

type recordedRoute struct {
	name    string
	service string
	port    int
	path    string
}

type recordingGateway struct {
	gateway.Client
	sources []recordedSource
	routes  []recordedRoute
}

func (g *recordingGateway) EnsureStaticServiceSource(_ context.Context, name, address string, port int, protocol string) error {
	g.sources = append(g.sources, recordedSource{name: name, address: address, port: port, protocol: protocol, static: true})
	return nil
}

func (g *recordingGateway) EnsureServiceSource(_ context.Context, name, domain string, port int, protocol string) error {
	g.sources = append(g.sources, recordedSource{name: name, address: domain, port: port, protocol: protocol})
	return nil
}

func (g *recordingGateway) EnsureRoute(_ context.Context, name string, _ []string, serviceName string, port int, pathPrefix string) error {
	g.routes = append(g.routes, recordedRoute{name: name, service: serviceName, port: port, path: pathPrefix})
	return nil
}

func newUnifiedEntryInitializer(cfg Config) (*Initializer, *recordingGateway) {
	g := &recordingGateway{}
	return &Initializer{Gateway: g, Config: cfg}, g
}

func TestUnifiedEntryDisabledRegistersNothing(t *testing.T) {
	i, g := newUnifiedEntryInitializer(Config{IsEmbedded: true})
	if err := i.initUnifiedEntryRoutes(context.Background()); err != nil {
		t.Fatalf("disabled unified entry returned error: %v", err)
	}
	if len(g.sources) != 0 || len(g.routes) != 0 {
		t.Fatalf("disabled unified entry must not touch the gateway: sources=%v routes=%v", g.sources, g.routes)
	}
}

func TestUnifiedEntryAPIRoutesController(t *testing.T) {
	i, g := newUnifiedEntryInitializer(Config{
		UnifiedEntryEnabled: true,
		IsEmbedded:          true,
		ControllerURL:       "http://127.0.0.1:8090",
		DashboardURL:        "http://127.0.0.1:3000",
	})
	if err := i.initUnifiedEntryRoutes(context.Background()); err != nil {
		t.Fatalf("unified entry: %v", err)
	}
	if len(g.sources) != 2 {
		t.Fatalf("want 2 service sources (controller, dashboard), got %v", g.sources)
	}
	api := g.sources[0]
	if api.name != "unified-entry-api" || !api.static || api.address != "127.0.0.1" || api.port != 8090 {
		t.Fatalf("unexpected /api source: %+v", api)
	}
	dash := g.sources[1]
	if dash.name != "unified-entry-dashboard" || !dash.static || dash.port != 3000 {
		t.Fatalf("unexpected /dashboard source: %+v", dash)
	}
	if len(g.routes) != 2 {
		t.Fatalf("want 2 routes, got %v", g.routes)
	}
	if g.routes[0].name != "unified-entry-api" || g.routes[0].path != "/api" ||
		g.routes[0].service != "unified-entry-api.static" || g.routes[0].port != 8090 {
		t.Fatalf("unexpected /api route: %+v", g.routes[0])
	}
	if g.routes[1].name != "unified-entry-dashboard" || g.routes[1].path != "/dashboard" ||
		g.routes[1].service != "unified-entry-dashboard.static" {
		t.Fatalf("unexpected /dashboard route: %+v", g.routes[1])
	}
}

func TestUnifiedEntryUsesEmbeddedControllerDefault(t *testing.T) {
	i, g := newUnifiedEntryInitializer(Config{UnifiedEntryEnabled: true, IsEmbedded: true})
	if err := i.initUnifiedEntryRoutes(context.Background()); err != nil {
		t.Fatalf("unified entry: %v", err)
	}
	if len(g.sources) != 1 || g.sources[0].name != "unified-entry-api" || g.sources[0].port != 8090 {
		t.Fatalf("want embedded controller default source, got %v", g.sources)
	}
	if len(g.routes) != 1 || g.routes[0].service != "unified-entry-api.static" || g.routes[0].path != "/api" {
		t.Fatalf("want controller /api route, got %v", g.routes)
	}
}

func TestUnifiedEntryInclusterUsesDNSSources(t *testing.T) {
	i, g := newUnifiedEntryInitializer(Config{
		UnifiedEntryEnabled: true,
		ControllerURL:       "http://agentteams-controller.agentteams.svc:8090",
	})
	if err := i.initUnifiedEntryRoutes(context.Background()); err != nil {
		t.Fatalf("unified entry: %v", err)
	}
	if len(g.sources) != 1 || g.sources[0].static || g.sources[0].address != "agentteams-controller.agentteams.svc" {
		t.Fatalf("want DNS source for incluster controller, got %v", g.sources)
	}
	if len(g.routes) != 1 || g.routes[0].service != "unified-entry-api.dns" {
		t.Fatalf("want DNS route suffix, got %v", g.routes)
	}
}

func TestUnifiedEntryResolvesSingleLabelHost(t *testing.T) {
	old := entryLookupHost
	entryLookupHost = func(string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	defer func() { entryLookupHost = old }()

	i, g := newUnifiedEntryInitializer(Config{
		UnifiedEntryEnabled: true,
		ControllerURL:       "http://d41-e2e-controller:8090",
	})
	if err := i.initUnifiedEntryRoutes(context.Background()); err != nil {
		t.Fatalf("unified entry: %v", err)
	}
	if len(g.sources) != 1 || !g.sources[0].static || g.sources[0].address != "127.0.0.1" || g.sources[0].port != 8090 {
		t.Fatalf("want resolved static source, got %v", g.sources)
	}
	if len(g.routes) != 1 || g.routes[0].service != "unified-entry-api.static" {
		t.Fatalf("want static route for resolved host, got %v", g.routes)
	}
}

func TestParseEntryURLRejectsUnsupportedScheme(t *testing.T) {
	if _, _, _, err := parseEntryURL("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("expected non-http(s) scheme to be rejected")
	}
	if _, _, _, err := parseEntryURL("not-a-url"); err == nil {
		t.Fatal("expected malformed URL to be rejected")
	}
}
