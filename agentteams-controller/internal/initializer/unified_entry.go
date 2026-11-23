package initializer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// unifiedEntryDefaultControllerURL is the in-container controller address used
// when the unified entry is enabled but no controller URL is configured. It
// matches the embedded stack's controller port.
const unifiedEntryDefaultControllerURL = "http://127.0.0.1:8090"

const (
	unifiedAPIRoute       = "unified-entry-api"
	unifiedDashboardRoute = "unified-entry-dashboard"
)

type unifiedEntryRoute struct {
	name, path, backend string
	appliedBackend      string
}

// ReconcileUnifiedEntryRoutes retries startup and refreshes resolved container
// addresses after recreation. Only the controller leader should call it.
func (i *Initializer) ReconcileUnifiedEntryRoutes(ctx context.Context) {
	i.reconcileUnifiedEntryRoutes(ctx, 5*time.Second)
}

func (i *Initializer) reconcileUnifiedEntryRoutes(ctx context.Context, interval time.Duration) {
	if i.Gateway == nil || !i.Config.managesGatewayRoutes() {
		return
	}
	logger := ctrl.Log.WithName("initializer").WithName("unified-entry")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	routes := i.desiredUnifiedEntryRoutes()
	ready := false
	for attempts := 0; ctx.Err() == nil; attempts++ {
		var failures []error
		for index := range routes {
			route := &routes[index]
			if err := i.reconcileUnifiedEntryRoute(ctx, route); err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", route.name, err))
			}
		}
		if len(failures) == 0 {
			if !i.Config.UnifiedEntryEnabled {
				return
			}
			if !ready {
				logger.Info("unified entry routes reconciled", "internal", i.Config.EntryInternalURL,
					"external", i.Config.EntryExternalURL)
			}
		}
		ready = len(failures) == 0
		if len(failures) > 0 && ctx.Err() == nil && attempts%6 == 0 {
			logger.Error(errors.Join(failures...), "unified entry route reconciliation failed; retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// desiredUnifiedEntryRoutes includes only the two routes reserved for this feature.
func (i *Initializer) desiredUnifiedEntryRoutes() []unifiedEntryRoute {
	cfg := i.Config
	apiURL := strings.TrimSpace(cfg.ControllerURL)
	if apiURL == "" && cfg.IsEmbedded {
		apiURL = unifiedEntryDefaultControllerURL
	}
	return []unifiedEntryRoute{
		{name: unifiedAPIRoute, path: "/api", backend: apiURL},
		{name: unifiedDashboardRoute, path: "/dashboard", backend: strings.TrimSpace(cfg.DashboardURL)},
	}
}

func (i *Initializer) reconcileUnifiedEntryRoute(ctx context.Context, route *unifiedEntryRoute) error {
	name, path, backend := route.name, route.path, route.backend
	if !i.Config.UnifiedEntryEnabled || backend == "" {
		if route.appliedBackend == "disabled" {
			return nil
		}
		if err := i.Gateway.DeleteRoute(ctx, name); err != nil {
			return err
		}
		route.appliedBackend = "disabled"
		return nil
	}
	managed, ok := i.Gateway.(interface {
		EnsureManagedRoute(ctx context.Context, name, serviceName string, port int, pathPrefix string) error
	})
	if !ok {
		return fmt.Errorf("gateway does not support managed HTTP routes")
	}
	host, port, protocol, err := parseEntryURL(backend)
	if err != nil {
		return fmt.Errorf("%s backend URL: %w", path, err)
	}
	host, err = resolveEntryHost(ctx, host)
	if err != nil {
		return fmt.Errorf("%s backend host: %w", path, err)
	}
	resolvedBackend := fmt.Sprintf("%s://%s:%d", protocol, host, port)
	if route.appliedBackend == resolvedBackend {
		return nil
	}
	// A partially applied update must be retried even if DNS changes back.
	route.appliedBackend = ""
	if err := i.ensureCustomServiceSource(ctx, name, host, port, protocol); err != nil {
		return fmt.Errorf("service source %s: %w", name, err)
	}
	if err := managed.EnsureManagedRoute(ctx, name, customServiceName(name, host), port, path); err != nil {
		return fmt.Errorf("route %s: %w", name, err)
	}
	route.appliedBackend = resolvedBackend
	return nil
}

// entryLookupHost resolves single-label hostnames (docker/pod DNS names such
// as "agentteams-controller"). It is a variable so tests can stub resolution.
var entryLookupHost = net.DefaultResolver.LookupHost

// resolveEntryHost turns a unified-entry backend host into a form Higress can
// register: IPs and dotted DNS names are used as-is, while single-label
// container/pod names are resolved to their first IPv4 address. Higress
// rejects non-FQDN DNS service sources ("Invalid domain format: <name>"), which
// is exactly what embedded docker names look like.
func resolveEntryHost(ctx context.Context, host string) (string, error) {
	if net.ParseIP(host) != nil || strings.Contains(host, ".") {
		return host, nil
	}
	ips, err := entryLookupHost(ctx, host)
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
