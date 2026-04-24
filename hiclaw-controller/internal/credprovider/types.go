// Package credprovider talks to the hiclaw-credential-provider sidecar to
// obtain short-lived Alibaba Cloud STS tokens.
//
// The sidecar is the only component in the controller that is allowed to
// hold (or derive) long-lived Alibaba Cloud identity material — an RRSA
// OIDC projected token, a pod-identity OIDC token, or (in the
// mock-credential-provider) a long-lived AccessKey pair. The controller
// itself is credential-less: whenever it needs to talk to APIG, OSS, or
// any other Alibaba Cloud service, it asks the sidecar for a fresh STS
// triple via this package.
//
// The request contract matches the accessEntries model documented in
// docs/design/hiclaw-credential-provider-arch.md §6.3: the caller sends
// an array of AccessEntry items whose scope is already fully resolved
// (no bucketRef / gatewayRef, no ${self.*} templates). The sidecar
// translates the entries into a provider-native inline policy and
// issues the STS triple.
package credprovider

// IssueRequest is the body sent to the sidecar's POST /issue endpoint.
//
// SessionName is propagated into AssumeRole's RoleSessionName and
// should uniquely identify the calling entity (e.g. "hiclaw-worker-alice",
// "hiclaw-manager", "hiclaw-controller"). Entries carries the resolved
// AccessEntry list and must be non-empty; the sidecar rejects empty
// requests with HTTP 400.
type IssueRequest struct {
	SessionName     string        `json:"session_name"`
	DurationSeconds int           `json:"duration_seconds,omitempty"`
	Entries         []AccessEntry `json:"entries"`
}

// AccessEntry is the RESOLVED form of a single permission grant as it
// crosses the controller → sidecar boundary. There is a matching but
// looser type in api/v1beta1 which is what users author in CRs; the
// controller's internal accessresolver package converts between them
// and expands logical refs and template variables.
type AccessEntry struct {
	Service     string      `json:"service"`
	Permissions []string    `json:"permissions,omitempty"`
	Scope       AccessScope `json:"scope"`
}

// AccessScope is the union of all resolved scope fields supported in
// v1. Only fields relevant to Service should be populated.
type AccessScope struct {
	// object-storage
	Bucket   string   `json:"bucket,omitempty"`
	Prefixes []string `json:"prefixes,omitempty"`
	// ai-gateway
	GatewayID string `json:"gatewayId,omitempty"`
	// Resources: ai-gateway API paths; for ai-registry, agentSpec/skill patterns (e.g. agentSpec/*, skill/*).
	Resources []string `json:"resources,omitempty"`
	// ai-registry
	NamespaceID string `json:"namespaceId,omitempty"`
}

// IssueResponse is the sidecar's reply to POST /issue.
//
// The sidecar returns only the STS triple; OSS endpoint is NOT part of
// this contract. Endpoint is a deployment-time static configuration
// (HICLAW_FS_ENDPOINT) and is sourced independently by each caller.
type IssueResponse struct {
	AccessKeyID     string `json:"access_key_id"`
	AccessKeySecret string `json:"access_key_secret"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
	ExpiresInSec    int    `json:"expires_in_sec"`
}

// Supported Service identifiers. Keep in sync with the CRD schema's
// `spec.accessEntries[].service` enum and with the sidecar's
// buildInlinePolicy dispatch.
const (
	ServiceObjectStorage = "object-storage"
	ServiceAIGateway     = "ai-gateway"
	ServiceAIRegistry    = "ai-registry"
)
