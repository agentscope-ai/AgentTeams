package server

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// Team-scoped access to a human's file area (#1414 P2).
//
// The access policy, as composed in humanWorkspaceEffectiveMode:
//
//   - default access follows membership: a team in the human's default
//     set (spec.accessibleTeams ∪ the teams that list the human in
//     spec.humanMembers) has readwrite on files/**;
//   - workspaceAccess grants raise the mode for the teams they name
//     (maximum across the caller's teams);
//   - workspaceAccess restrictions are a global deny: a restriction from
//     any of the caller's teams applies to the path across ALL of the
//     caller's teams — the strongest wins ("none" revokes, "read" caps) —
//     and it is applied after the baseline/grant maximum, so membership
//     in a second, unrestricted team cannot override it (fail closed);
//   - memory/, digest/ and the control prefixes .control/, .sync/,
//     .trash/ are never addressable through the files API, no matter what
//     the rules say.
//
// Write attribution is audited against the team that produced the final
// effective mode (the restricting team when a restriction lowered it).
//
// The mode string values are the wire values used by the
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

// humanWorkspacePrivatePrefixes are the files-relative top-level prefixes
// that no workspaceAccess rule can ever authorize: the personal memory
// and digest areas, plus the control prefixes (the .control quota ledger,
// .sync state, and the .trash recycle bin). Validation already rejects
// rule paths inside them (memory/ and digest/ are not files/ paths at
// all); the set is re-checked at decision time so a hand-written or
// stale spec can never grant access to them through the files API.
var humanWorkspacePrivatePrefixes = append([]string{"memory", "digest"}, reservedHumanFilePrefixes...)

// humanWorkspacePrefixDenied reports whether the files-relative path
// ("" = the files root) is the prefix itself or lies under it.
func humanWorkspacePrefixDenied(rel string) bool {
	for _, p := range humanWorkspacePrivatePrefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// humanWorkspaceEffectiveMode composes the caller's effective mode on one
// files-relative path across all of the caller's teams, in the decision
// order baseline -> grants -> restrictions:
//
//  1. baseline (membership): the caller is in at least one of the
//     human's default teams and therefore has readwrite on files/**;
//  2. grants: a matching rule from any of the caller's teams raises the
//     mode (maximum across teams);
//  3. restrictions (global deny): after the maximum above is known, every
//     matching restriction from any of the caller's teams lowers it — the
//     strongest wins ("none" revokes, "read" caps). A restriction is not
//     confined to its own team: it denies access the caller could
//     otherwise get through another team, so membership in a second,
//     unrestricted default team cannot override it (fail closed).
//
// Private and control prefixes (memory/, digest/, .control/, .sync/,
// .trash/) are never addressable through the files API: the entry returns
// "none" for them no matter what the rules say.
//
// The returned team is the one that produced the final mode, for
// write-attribution auditing: the default team for the baseline, the
// granting team for a grant-raised mode, and — when a restriction
// lowered the mode — the team the restriction belongs to, so a capped
// write is traceable to the restriction that produced it. Ties resolve
// to the lexicographically first team (deterministic). Callers pass the
// human's spec, their own team set, and the human's default team set.
func humanWorkspaceEffectiveMode(spec *v1beta1.HumanSpec, teams []string, defaultTeams map[string]struct{}, rel string) (string, string) {
	if humanWorkspacePrefixDenied(rel) {
		return humanWorkspaceModeNone, ""
	}
	sorted := append([]string(nil), teams...)
	sort.Strings(sorted)

	// 1. Baseline: membership in any default team gives readwrite.
	mode, sourceTeam := humanWorkspaceModeNone, ""
	for _, t := range sorted {
		if _, ok := defaultTeams[t]; ok {
			mode = humanWorkspaceModeReadWrite
			sourceTeam = t
			break
		}
	}

	// 2. Grants: any matching rule from a caller team raises the mode.
	if wa := spec.WorkspaceAccess; wa != nil {
		for _, t := range sorted {
			for _, g := range wa.Grants {
				if g.Team != t || !humanWorkspacePathMatches(g.Paths, rel) {
					continue
				}
				if m := humanWorkspaceMaxMode(mode, g.Mode); m != mode {
					mode, sourceTeam = m, t
				}
			}
		}
	}

	// 3. Restrictions: global deny-wins, applied after the maximum above.
	//    restrictedBy is set only when the restriction actually lowered
	//    the mode, so attribution still points at the team that produced
	//    an unchanged mode.
	restrictedBy := ""
	if wa := spec.WorkspaceAccess; wa != nil {
		for _, t := range sorted {
			for _, r := range wa.Restrictions {
				if r.Team != t || !humanWorkspacePathMatches(r.Paths, rel) {
					continue
				}
				switch r.Mode {
				case humanWorkspaceModeNone:
					if mode != humanWorkspaceModeNone {
						mode, restrictedBy = humanWorkspaceModeNone, t
					}
				case humanWorkspaceModeRead:
					if humanWorkspaceModeRank(mode) > humanWorkspaceModeRank(humanWorkspaceModeRead) {
						mode, restrictedBy = humanWorkspaceModeRead, t
					}
				}
			}
		}
	}
	if restrictedBy != "" {
		return mode, restrictedBy
	}
	return mode, sourceTeam
}
