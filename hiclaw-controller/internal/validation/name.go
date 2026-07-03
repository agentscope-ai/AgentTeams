// Package validation holds shared validators for user-supplied input that
// must be enforced consistently across every entry point (HTTP API, CLI,
// etc.), rather than duplicated (and drifting) at each call site.
package validation

import (
	"fmt"
	"regexp"
)

// resourceNamePattern mirrors the CLI-side rule in cmd/hiclaw/create.go
// (workerNamePattern): lowercase alphanumeric, starting with an alphanumeric,
// followed by lowercase alphanumeric or hyphens. This is also the set of
// characters Kubernetes object names (RFC 1123 label-ish) and Matrix
// localparts derived from them can safely accept.
var resourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateResourceName reports an error if name does not match the required
// ^[a-z0-9][a-z0-9-]*$ shape. Call this on every user-supplied resource name
// before it is used as a Kubernetes ObjectMeta.Name (or propagated into
// derived identifiers such as Matrix usernames or container names).
func ValidateResourceName(name string) error {
	if !resourceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid name %q: must match ^[a-z0-9][a-z0-9-]*$ (lowercase alphanumeric and hyphens, starting with an alphanumeric character)", name)
	}
	return nil
}
