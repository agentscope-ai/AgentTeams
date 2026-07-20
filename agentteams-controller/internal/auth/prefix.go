package auth

import (
	"fmt"
	"strings"
)

// ResourcePrefix is the tenant-level prefix used to derive all AgentTeams-managed
// resource names (Pods, ServiceAccounts, "app" labels, STS session names).
// Default "agentteams-". Override via AGENTTEAMS_RESOURCE_PREFIX on the controller
// Deployment to isolate multiple AgentTeams instances sharing one K8s namespace.
//
// The prefix intentionally does NOT propagate to:
//   - OPENCLAW_MDNS_HOSTNAME (OpenClaw-layer Matrix identifier; may encode
//     long-lived state that must stay stable across tenants)
//   - cms.serviceName (observability service identity should be stable)
//   - install/agentteams-install.sh hardcoded names (embedded-mode-only paths)
type ResourcePrefix string

// DefaultResourcePrefix is the baked-in default ("agentteams-") used when
// AGENTTEAMS_RESOURCE_PREFIX is unset. All production code paths receive this
// through configuration; the constant is exported primarily for tests.
const DefaultResourcePrefix ResourcePrefix = "agentteams-"

// Or returns the receiver if non-empty, else DefaultResourcePrefix. Useful
// at construction sites that accept an optional prefix via a config struct.
func (p ResourcePrefix) Or(fallback ResourcePrefix) ResourcePrefix {
	if p == "" {
		if fallback == "" {
			return DefaultResourcePrefix
		}
		return fallback
	}
	return p
}

// String returns the raw prefix string (e.g. "agentteams-").
func (p ResourcePrefix) String() string { return string(p) }

// WorkerNamePrefix returns the Pod/container/SA name prefix for workers,
// e.g. "agentteams-worker-". Always ends with "-".
func (p ResourcePrefix) WorkerNamePrefix() string {
	return p.effective() + "worker-"
}

// ManagerNamePrefix returns the Pod/container name prefix for non-default
// managers, e.g. "agentteams-manager-". Always ends with "-".
func (p ResourcePrefix) ManagerNamePrefix() string {
	return p.effective() + "manager-"
}

// ManagerDefaultName returns the Pod/container/SA name for the single shared
// Manager identity, e.g. "agentteams-manager". All Manager CRs in a namespace
// share this ServiceAccount regardless of CR name; the default Manager (CR
// name "default") also uses this as its Pod name for install-script compat.
func (p ResourcePrefix) ManagerDefaultName() string {
	return p.effective() + "manager"
}

// AdminName returns the admin ServiceAccount name, e.g. "agentteams-admin".
func (p ResourcePrefix) AdminName() string {
	return p.effective() + "admin"
}

// WorkerAppLabel returns the Pod "app" label value for workers, e.g.
// "agentteams-worker". Purely decorative — useful for ad-hoc kubectl grouping
// (`kubectl get pod -l app=agentteams-worker`) and external dashboards. No
// in-tree code selects on this label; isolation between controller
// instances is done via agentteams.io/controller. Kept stable for external
// tooling compatibility.
func (p ResourcePrefix) WorkerAppLabel() string {
	return p.effective() + "worker"
}

// ManagerAppLabel returns the Pod "app" label value for managers, e.g.
// "agentteams-manager". Same decorative-only semantics as WorkerAppLabel.
func (p ResourcePrefix) ManagerAppLabel() string {
	return p.effective() + "manager"
}

// WorkerSessionName returns the controller log label for a worker STS
// request, e.g. "agentteams-worker-alice". Cloud STS RoleSessionName uses a
// generated UUID to stay below provider length limits.
func (p ResourcePrefix) WorkerSessionName(name string) string {
	return p.WorkerNamePrefix() + name
}

// ManagerSessionName returns the controller log label for a manager STS
// request, e.g. "agentteams-manager-default".
func (p ResourcePrefix) ManagerSessionName(name string) string {
	return p.ManagerNamePrefix() + name
}

// ManagerPodName returns the Pod/container name for a Manager CR.
// The "default" Manager uses ManagerDefaultName (for install-script and
// CMS-service-name compatibility); other Managers use "${prefix}manager-<name>".
func (p ResourcePrefix) ManagerPodName(managerName string) string {
	if managerName == "default" {
		return p.ManagerDefaultName()
	}
	return p.ManagerNamePrefix() + managerName
}

// SAName returns the ServiceAccount name for the given role/name pair.
// Manager role returns a single shared SA regardless of manager CR name
// (historical invariant); worker/team-leader share the worker prefix.
func (p ResourcePrefix) SAName(role, name string) string {
	switch role {
	case RoleAdmin:
		return p.AdminName()
	case RoleManager:
		return p.ManagerDefaultName()
	default:
		return p.WorkerNamePrefix() + name
	}
}

// ParseSAUsername extracts identity from a K8s SA username of the form
// "system:serviceaccount:{namespace}:{sa-name}". Names are matched against
// the receiver prefix so multi-tenant controllers with different prefixes
// remain isolated.
func (p ResourcePrefix) ParseSAUsername(username string) (*CallerIdentity, error) {
	const saPrefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, saPrefix) {
		return nil, fmt.Errorf("unexpected username format: %q", username)
	}
	parts := strings.SplitN(username[len(saPrefix):], ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("cannot parse SA from username: %q", username)
	}
	saNamespace := parts[0]
	saName := parts[1]

	switch {
	case saName == p.AdminName():
		return &CallerIdentity{Role: RoleAdmin, Username: "admin", ServiceAccountNamespace: saNamespace, ServiceAccountName: saName}, nil
	case saName == p.ManagerDefaultName():
		return &CallerIdentity{Role: RoleManager, Username: "manager", ServiceAccountNamespace: saNamespace, ServiceAccountName: saName}, nil
	case strings.HasPrefix(saName, p.WorkerNamePrefix()):
		name := saName[len(p.WorkerNamePrefix()):]
		return &CallerIdentity{Role: RoleWorker, Username: name, WorkerName: name, ServiceAccountNamespace: saNamespace, ServiceAccountName: saName}, nil
	default:
		return nil, fmt.Errorf("unrecognized SA name pattern: %q", saName)
	}
}

// effective returns the receiver, defaulting to DefaultResourcePrefix when empty.
// Kept private so consumers can't silently construct an ambiguous zero-value
// prefix — explicit Or() at construction sites is clearer.
func (p ResourcePrefix) effective() string {
	if p == "" {
		return string(DefaultResourcePrefix)
	}
	return string(p)
}
