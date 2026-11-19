package initializer

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
)

// unifiedEntryDefaultControllerURL is the in-container controller address used
// when the unified entry is enabled but no controller URL is configured. It
// matches the embedded stack's controller port.
const unifiedEntryDefaultControllerURL = "http://127.0.0.1:8090"

// initUnifiedEntryRoutes registers the single-entry front door on the
// gateway so clients only need one AgentTeams address:
//
//	/api       -> controller
//	/dashboard -> dashboard upstream when configured
//
// This change keeps /api on the controller, the only HTTP business plane that
// exists in this tree. Moving /api to agentteams-service later is a follow-up
// that changes only the selected upstream; it does not change the route,
// authentication, or the single-entry contract.
func (i *Initializer) initUnifiedEntryRoutes(ctx context.Context) error {
	logger := ctrl.Log.WithName("initializer").WithName("unified-entry")
	cfg := i.Config
	if !cfg.UnifiedEntryEnabled {
		return nil
	}

	// The gateway resource name must not collide with an existing Kubernetes
	// Service in the embedded stack, so the source is named after the entry
	// stage.
	const apiSource = "unified-entry-api"
	apiURL := strings.TrimSpace(cfg.ControllerURL)
	if apiURL == "" && cfg.IsEmbedded {
		apiURL = unifiedEntryDefaultControllerURL
	}
	if apiURL == "" {
		logger.Info("unified entry: no /api controller URL configured; skipping stage",
			"controller_url_set", cfg.ControllerURL != "")
	} else {
		host, port, protocol, err := parseEntryURL(apiURL)
		if err != nil {
			return fmt.Errorf("/api backend %q: %w", apiURL, err)
		}
		host, err = resolveEntryHost(host)
		if err != nil {
			return fmt.Errorf("/api backend host: %w", err)
		}
		if err := i.ensureCustomServiceSource(ctx, apiSource, host, port, protocol); err != nil {
			return fmt.Errorf("service source %s: %w", apiSource, err)
		}
		if err := i.Gateway.EnsureRoute(ctx, "unified-entry-api", nil, apiSource+"."+i.unifiedEntrySuffix(host), port, "/api"); err != nil {
			return fmt.Errorf("route unified-entry-api: %w", err)
		}
		logger.Info("unified entry /api route ready",
			"backend", "agentteams-controller", "internal", cfg.EntryInternalURL, "external", cfg.EntryExternalURL)
	}

	if dashboardURL := strings.TrimSpace(cfg.DashboardURL); dashboardURL != "" {
		host, port, protocol, err := parseEntryURL(dashboardURL)
		if err != nil {
			return fmt.Errorf("/dashboard backend %q: %w", dashboardURL, err)
		}
		host, err = resolveEntryHost(host)
		if err != nil {
			return fmt.Errorf("/dashboard backend host: %w", err)
		}
		if err := i.ensureCustomServiceSource(ctx, "unified-entry-dashboard", host, port, protocol); err != nil {
			return fmt.Errorf("service source unified-entry-dashboard: %w", err)
		}
		if err := i.Gateway.EnsureRoute(ctx, "unified-entry-dashboard", nil, "unified-entry-dashboard."+i.unifiedEntrySuffix(host), port, "/dashboard"); err != nil {
			return fmt.Errorf("route unified-entry-dashboard: %w", err)
		}
		logger.Info("unified entry /dashboard route ready", "url", dashboardURL)
	}

	return nil
}

// unifiedEntrySuffix mirrors the embedded/incluster service-source suffix
// convention: embedded static (IP) sources end in ".static", DNS ones in ".dns".
func (i *Initializer) unifiedEntrySuffix(host string) string {
	if i.Config.IsEmbedded || net.ParseIP(host) != nil {
		return "static"
	}
	return "dns"
}

// entryLookupHost resolves single-label hostnames (docker/pod DNS names such
// as "agentteams-controller"). It is a variable so tests can stub resolution.
var entryLookupHost = net.LookupHost

// resolveEntryHost turns a unified-entry backend host into a form Higress can
// register: IPs and dotted DNS names are used as-is, while single-label
// container/pod names are resolved to their first IPv4 address. Higress
// rejects non-FQDN DNS service sources ("Invalid domain format: <name>"), which
// is exactly what embedded docker names look like.
func resolveEntryHost(host string) (string, error) {
	if net.ParseIP(host) != nil || strings.Contains(host, ".") {
		return host, nil
	}
	ips, err := entryLookupHost(host)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed != nil && parsed.To4() != nil {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no IPv4 address for %q", host)
}

// parseEntryURL validates a unified-entry backend URL and returns its host,
// port and scheme (used as the service-source protocol).
func parseEntryURL(raw string) (host string, port int, protocol string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", 0, "", fmt.Errorf("must be an http(s) URL")
	}
	if u.Hostname() == "" {
		return "", 0, "", fmt.Errorf("missing host")
	}
	host, port, err = parseHostPort(raw)
	if err != nil {
		return "", 0, "", err
	}
	return host, port, u.Scheme, nil
}
