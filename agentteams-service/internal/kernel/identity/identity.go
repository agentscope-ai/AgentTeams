// Package identity carries the caller identity the service resolves per
// request through the controller token-review route, plus the
// 60s jti cache keyed on the token digest.
//
// R1 fact (source-verified against agentteams-controller @ 71dc233a2):
// the controller's CallerIdentity has NO token jti field, so the service
// derives a stable, non-reversible cache key from the bearer token
// (TokenDigest) and records that value as ops.audit_events.token_jti.
// R2 may replace it once the controller exposes a real jti.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
)

// Role mirrors agentteams-controller internal/auth role constants.
type Role string

const (
	RoleAdmin      Role = "admin"
	RoleManager    Role = "manager"
	RoleHuman      Role = "human"
	RoleTeamLeader Role = "team-leader"
	RoleWorker     Role = "worker"
)

// Identity is the JSON shape returned by POST /internal/v1/token-review.
// It mirrors the controller's CallerIdentity field for field.
type Identity struct {
	MatrixUserID         string   `json:"matrixUserID,omitempty"`
	HumanPermissionLevel int      `json:"humanPermissionLevel,omitempty"`
	Role                 Role     `json:"role"`
	Username             string   `json:"username,omitempty"`
	Team                 string   `json:"team,omitempty"`
	Teams                []string `json:"teams,omitempty"`
	Capabilities         []string `json:"capabilities,omitempty"`
	AccessibleWorkers    []string `json:"accessibleWorkers,omitempty"`
	WorkerName           string   `json:"workerName,omitempty"`
}

// TeamMatches mirrors CallerIdentity.TeamMatches: the L2 team set wins,
// otherwise the single legacy Team field is consulted.
func (i *Identity) TeamMatches(team string) bool {
	if team == "" {
		return false
	}
	if len(i.Teams) > 0 {
		for _, t := range i.Teams {
			if t == team {
				return true
			}
		}
		return false
	}
	return i.Team == team
}

// WorkerReadable mirrors CallerIdentity.WorkerReadable: an explicit worker
// assignment (L3 human) or team membership grants read access.
func (i *Identity) WorkerReadable(team, workerName string) bool {
	for _, w := range i.AccessibleWorkers {
		if w == workerName {
			return true
		}
	}
	return i.TeamMatches(team)
}

// IsWorkerScoped reports an L3 worker-scoped human (Robot? no: RoleHuman
// with an explicit worker assignment); its MCP URLs are sanitized on read.
func (i *Identity) IsWorkerScoped() bool {
	return i.Role == RoleHuman && len(i.AccessibleWorkers) > 0
}

// CanManageEnv reports admin/manager, the only roles that may write env or
// see it on read.
func (i *Identity) CanManageEnv() bool {
	return i.Role == RoleAdmin || i.Role == RoleManager
}

// TokenDigest is the cache key / audit token_jti value: sha256(token)
// hex, first 32 chars (128 bits). Never reversible, stable per token.
func TokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}
