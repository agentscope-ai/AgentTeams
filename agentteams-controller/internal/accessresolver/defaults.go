// Package accessresolver converts CR-layer AccessEntry declarations
// (with logical refs like bucketRef and template variables like
// ${self.name}) into the fully-resolved form that
// hiclaw-credential-provider expects.
//
// It is the single place that knows the workspace-local vocabulary:
//
//   - bucketRef: workspace  →  the cluster's OSS bucket (OSSBucket config)
//   - gatewayRef: default   →  the cluster's APIG gatewayId (GWGatewayID)
//   - ${self.name}          →  the Worker/Manager CR name
//   - ${self.kind}          →  "Worker" or "Manager"
//   - ${self.namespace}     →  the CR namespace
//
// This package is only compiled in and used when a credential-provider
// sidecar is configured. Local higress+minio deployments never reach it.
package accessresolver

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credprovider"
)

// DefaultEntriesForWorker returns the CR-layer default AccessEntry set
// applied when a Worker CR omits spec.accessEntries. Mirrors the
// embedded-mode MinIO policy produced by oss.MinIOAdminClient.buildWorkerPolicy
// (for a standalone worker): read/write/list/delete on the worker's own
// agent prefix and on the shared prefix, scoped to the workspace bucket.
//
// The returned entries still contain the ${self.name} template — they
// are resolved by Resolver.ResolveForCaller before leaving the
// controller.
func DefaultEntriesForWorker() []v1beta1.AccessEntry {
	return []v1beta1.AccessEntry{
		{
			Service:     credprovider.ServiceObjectStorage,
			Permissions: []string{"read", "write", "list", "delete"},
			Scope: jsonObj(map[string]any{
				"bucketRef": "workspace",
				"prefixes": []any{
					"agents/${self.name}/",
					"agents/${self.name}/*",
					"shared/",
					"shared/*",
				},
			}),
		},
	}
}

// DefaultEntriesForTeamMember returns the CR-layer default AccessEntry
// set applied when a LeaderSpec or TeamWorkerSpec omits accessEntries.
// Mirrors the embedded-mode policy produced by
// oss.MinIOAdminClient.buildWorkerPolicy when teamName != "": read/
// write/list/delete on the member's own agent prefix, on the shared
// prefix, and on the team-scoped prefix.
//
// Leader and team workers share the same default scope — the leader is
// not elevated with cross-member access. Cross-member collaboration is
// expected to flow through the shared teams/<team>/* prefix.
//
// The returned entries contain the ${self.name} / ${self.team} templates
// which are resolved by Resolver.resolveTeamMember before leaving the
// controller.
func DefaultEntriesForTeamMember() []v1beta1.AccessEntry {
	return []v1beta1.AccessEntry{
		{
			Service:     credprovider.ServiceObjectStorage,
			Permissions: []string{"read", "write", "list", "delete"},
			Scope: jsonObj(map[string]any{
				"bucketRef": "workspace",
				"prefixes": []any{
					"agents/${self.name}/",
					"agents/${self.name}/*",
					"shared/",
					"shared/*",
					"teams/${self.team}/",
					"teams/${self.team}/*",
				},
			}),
		},
	}
}

// DefaultEntriesForManager returns the CR-layer default AccessEntry set
// applied when a Manager CR omits spec.accessEntries. It mirrors the
// legacy hard-coded manager role: broad read/write on its own agent
// workspace, the shared prefix, and the manager prefix.
func DefaultEntriesForManager() []v1beta1.AccessEntry {
	return []v1beta1.AccessEntry{
		{
			Service:     credprovider.ServiceObjectStorage,
			Permissions: []string{"read", "write", "list", "delete"},
			Scope: jsonObj(map[string]any{
				"bucketRef": "workspace",
				"prefixes": []any{
					"agents/${self.name}/",
					"agents/${self.name}/*",
					"shared/",
					"shared/*",
					"manager/",
					"manager/*",
				},
			}),
		},
	}
}

// ControllerDefaults returns the RESOLVED AccessEntry set the controller
// itself requests from the sidecar for its own reconciliation work
// (OSS mc alias, APIG admin SDK). Unlike worker/manager defaults these
// are expressed directly in provider-layer form — no templates, no
// logical refs — because the controller always issues its own token
// before any CR is ever read.
//
// bucket is the configured OSS bucket (OSSBucket). gatewayID is the
// configured APIG gateway id; pass "" when UsesAIGateway() is false
// and the ai-gateway entry will be omitted.
func ControllerDefaults(bucket, gatewayID string) []credprovider.AccessEntry {
	entries := []credprovider.AccessEntry{
		{
			Service:     credprovider.ServiceObjectStorage,
			Permissions: []string{"read", "write", "list", "delete"},
			Scope: credprovider.AccessScope{
				Bucket:   bucket,
				Prefixes: []string{"*"},
			},
		},
	}
	if gatewayID != "" {
		entries = append(entries, credprovider.AccessEntry{
			Service:     credprovider.ServiceAIGateway,
			Permissions: []string{"read", "write"},
			Scope: credprovider.AccessScope{
				GatewayID: gatewayID,
				Resources: []string{"*"},
			},
		})
	}
	return entries
}

// jsonObj is a small helper to build *apiextensionsv1.JSON values in
// Go literals without going through k8s.io/apimachinery Unstructured.
func jsonObj(m map[string]any) *apiextensionsv1.JSON {
	raw, err := marshalJSONDeterministic(m)
	if err != nil {
		// impossible for the inputs we construct above
		panic(err)
	}
	return &apiextensionsv1.JSON{Raw: raw}
}
