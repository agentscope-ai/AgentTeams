package server

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// Effective access modes for a caller against a human's file area
// (#1414 P2). The string values are the wire values used by the
// HumanSpec.WorkspaceAccess rule modes.
const (
	humanWorkspaceModeNone      = "none"
	humanWorkspaceModeRead      = "read"
	humanWorkspaceModeReadWrite = "readwrite"
)

// humanWorkspaceModeRank orders the modes so composition is a plain
// integer comparison.
func humanWorkspaceModeRank(mode string) int {
	switch mode {
	case humanWorkspaceModeReadWrite:
		return 2
	case humanWorkspaceModeRead:
		return 1
	default:
		return 0
	}
}

func humanWorkspaceMaxMode(a, b string) string {
	if humanWorkspaceModeRank(b) > humanWorkspaceModeRank(a) {
		return b
	}
	return a
}

func humanWorkspaceMinMode(a, b string) string {
	if humanWorkspaceModeRank(b) < humanWorkspaceModeRank(a) {
		return b
	}
	return a
}

// Rule-count bounds. They mirror the CRD schema; the API re-checks them so
// behavior does not depend on admission being present (kubectl apply with
// --validate=none, direct CR writes).
const (
	maxWorkspaceAccessRules = 32
	maxWorkspaceAccessPaths = 16
)

// validateHumanWorkspaceAccess validates a workspaceAccess payload (an
// UpdateHuman request body or any spec before write). It is the single
// source of truth outside the CRD schema, so unit tests exercise it
// directly without a cluster.
//
// Rules:
//   - every path must start with "files/" — memory/**, digest/**, and any
//     other area are never addressable through the files API and can never
//     be granted;
//   - no absolute paths, no backslashes, no control characters, no "." or
//     ".." segments, no empty segments, no reserved top-level names
//     (.trash/.sync/.control), no trailing "/";
//   - the only glob metacharacters are "*" (exactly one segment) and "**"
//     (zero or more whole segments);
//   - a grant mode is read|readwrite; a restriction mode is none|read;
//   - at most one rule per team within one list (the same team may appear
//     in both lists — at decision time the restriction wins);
//   - at most 32 rules per list and 16 paths per rule.
func validateHumanWorkspaceAccess(wa *v1beta1.HumanWorkspaceAccess) error {
	if wa == nil {
		return nil
	}
	if err := validateWorkspaceAccessRuleList(wa.Grants, "grants", []string{humanWorkspaceModeRead, humanWorkspaceModeReadWrite}); err != nil {
		return err
	}
	return validateWorkspaceAccessRuleList(wa.Restrictions, "restrictions", []string{humanWorkspaceModeNone, humanWorkspaceModeRead})
}

func validateWorkspaceAccessRuleList(rules []v1beta1.HumanWorkspaceAccessRule, list string, allowedModes []string) error {
	if len(rules) > maxWorkspaceAccessRules {
		return fmt.Errorf("workspaceAccess.%s: at most %d rules are allowed", list, maxWorkspaceAccessRules)
	}
	seen := make(map[string]struct{}, len(rules))
	for i, rule := range rules {
		where := fmt.Sprintf("workspaceAccess.%s[%d]", list, i)
		if rule.Team == "" {
			return fmt.Errorf("%s: team must not be empty", where)
		}
		if _, dup := seen[rule.Team]; dup {
			return fmt.Errorf("%s: duplicate team %q; at most one rule per team in %s", where, rule.Team, list)
		}
		seen[rule.Team] = struct{}{}
		if len(rule.Paths) == 0 {
			return fmt.Errorf("%s: paths must contain at least 1 entry", where)
		}
		if len(rule.Paths) > maxWorkspaceAccessPaths {
			return fmt.Errorf("%s: paths must contain at most %d entries", where, maxWorkspaceAccessPaths)
		}
		validMode := false
		for _, m := range allowedModes {
			if rule.Mode == m {
				validMode = true
				break
			}
		}
		if !validMode {
			return fmt.Errorf("%s: mode must be one of %s", where, strings.Join(allowedModes, ", "))
		}
		for j, p := range rule.Paths {
			if err := validateWorkspaceAccessPath(p); err != nil {
				return fmt.Errorf("%s: paths[%d] %q: %v", where, j, p, err)
			}
		}
	}
	return nil
}

// validateWorkspaceAccessPath checks one glob path against the files/**
// guardrails. The reserved-prefix check reuses the files-API reservation
// list so a rule can never point at the recycle bin or control objects.
func validateWorkspaceAccessPath(p string) error {
	if !utf8.ValidString(p) {
		return fmt.Errorf("path contains invalid UTF-8")
	}
	if p == "" {
		return fmt.Errorf("path must not be empty")
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return fmt.Errorf("control characters are not allowed in paths")
		}
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("absolute paths are not allowed")
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("backslashes are not allowed in paths")
	}
	if !strings.HasPrefix(p, "files/") {
		return fmt.Errorf("paths must start with files/ (memory/** and digest/** are never addressable)")
	}
	if strings.HasSuffix(p, "/") {
		return fmt.Errorf("paths must not end with /")
	}
	rel := strings.TrimPrefix(p, "files/")
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			return fmt.Errorf("path contains an empty segment")
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("path segment %q is not allowed", seg)
		}
	}
	for _, reserved := range reservedHumanFilePrefixes {
		if rel == reserved || strings.HasPrefix(rel, reserved+"/") {
			return fmt.Errorf("path %q is reserved", reserved)
		}
	}
	return nil
}

// humanWorkspacePathMatches reports whether any of the rule's patterns
// matches the requested files-relative path ("" = the files root). The
// request path is matched in its full "files/<rel>" form, the same form the
// validated rule paths use.
func humanWorkspacePathMatches(patterns []string, rel string) bool {
	// Full "files/<rel>" form; the root is the bare word "files" so that
	// "files/**" covers it while "files/*" (direct children only) does not.
	full := strings.TrimSuffix("files"+strings.TrimSuffix("/"+rel, "/"), "/")
	for _, p := range patterns {
		if humanWorkspaceGlobMatch(p, full) {
			return true
		}
	}
	return false
}

// humanWorkspaceGlobMatch matches a full files-area path against a pattern
// whose only metacharacters are "*" (exactly one segment) and "**" (zero or
// more whole segments). Everything else is literal.
func humanWorkspaceGlobMatch(pattern, path string) bool {
	return humanWorkspaceGlobSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func humanWorkspaceGlobSegments(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		rest := pattern[1:]
		// "**" may match zero segments.
		if humanWorkspaceGlobSegments(rest, path) {
			return true
		}
		if len(path) == 0 {
			return false
		}
		// Consume one segment and keep the "**" (it may match more).
		return humanWorkspaceGlobSegments(pattern, path[1:])
	}
	if len(path) == 0 {
		return false
	}
	if !humanWorkspaceSegmentMatch(pattern[0], path[0]) {
		return false
	}
	return humanWorkspaceGlobSegments(pattern[1:], path[1:])
}

// humanWorkspaceSegmentMatch matches one path segment against a pattern
// segment that may contain "*" wildcards. Segments contain no "/", so a
// single two-pointer backtracking matcher is exact.
func humanWorkspaceSegmentMatch(pattern, seg string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == seg
	}
	pi, si := 0, 0
	starPI, starSI := -1, -1
	for si < len(seg) {
		switch {
		case pi < len(pattern) && pattern[pi] == seg[si]:
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			starPI, starSI = pi, si
			pi++
		case starPI >= 0:
			pi = starPI + 1
			starSI++
			si = starSI
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// humanWorkspaceEffectiveMode composes the caller's effective mode on one
// files-relative path across all of the caller's teams, in the decision
// order baseline -> grants -> restrictions:
//
//  1. baseline: a team in defaultTeams has readwrite on files/**;
//  2. grants: a matching rule raises the mode (max);
//  3. restrictions: for default teams only, a matching rule lowers it —
//     "none" revokes, "read" caps. A restriction therefore always wins
//     over a grant on the same path (fail closed).
//
// The returned team is the one that produced the winning mode (the
// lexicographically first among ties — deterministic), for write-
// attribution auditing. Callers pass the human's spec, their own team
// set, and the human's default team set.
func humanWorkspaceEffectiveMode(spec *v1beta1.HumanSpec, teams []string, defaultTeams map[string]struct{}, rel string) (string, string) {
	mode, team := humanWorkspaceModeNone, ""
	sorted := append([]string(nil), teams...)
	sort.Strings(sorted)
	for _, t := range sorted {
		_, isDefault := defaultTeams[t]
		m := humanWorkspaceTeamMode(spec, t, isDefault, rel)
		if humanWorkspaceModeRank(m) > humanWorkspaceModeRank(mode) {
			mode, team = m, t
		}
	}
	return mode, team
}

// humanWorkspaceTeamMode is the per-team composition described by
// humanWorkspaceEffectiveMode.
func humanWorkspaceTeamMode(spec *v1beta1.HumanSpec, team string, isDefault bool, rel string) string {
	mode := humanWorkspaceModeNone
	if isDefault {
		mode = humanWorkspaceModeReadWrite
	}
	wa := spec.WorkspaceAccess
	if wa == nil {
		return mode
	}
	for _, g := range wa.Grants {
		if g.Team == team && humanWorkspacePathMatches(g.Paths, rel) {
			mode = humanWorkspaceMaxMode(mode, g.Mode)
		}
	}
	if isDefault {
		// Restrictions apply only to default teams (the intersection of
		// the human's default set and the caller's teams is exactly
		// "isDefault" here, since the caller's own team is t).
		for _, r := range wa.Restrictions {
			if r.Team != team || !humanWorkspacePathMatches(r.Paths, rel) {
				continue
			}
			if r.Mode == humanWorkspaceModeNone {
				mode = humanWorkspaceModeNone
			} else {
				mode = humanWorkspaceMinMode(mode, humanWorkspaceModeRead)
			}
		}
	}
	return mode
}
