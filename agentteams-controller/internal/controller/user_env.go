package controller

import (
	"sort"

	"github.com/go-logr/logr"
)

// mergeUserEnv injects user-declared env vars into sysEnv with
// system-wins precedence: any key already present in sysEnv is kept,
// and the user's value is discarded with a single INFO-level warning
// per ignored key. Collisions are sorted before logging so identical
// inputs produce identical log output (makes tests deterministic).
//
// Both maps may be nil. sysEnv is mutated in place and must be non-nil
// when userEnv is non-empty; callers always pass the builder's output,
// which is guaranteed non-nil.
func mergeUserEnv(sysEnv, userEnv map[string]string, logger logr.Logger, subject string) {
	if len(userEnv) == 0 {
		return
	}

	var ignored []string
	for k, v := range userEnv {
		if _, taken := sysEnv[k]; taken {
			ignored = append(ignored, k)
			continue
		}
		sysEnv[k] = v
	}

	if len(ignored) == 0 {
		return
	}
	sort.Strings(ignored)
	logger.Info("user-defined env keys ignored (reserved by system)",
		"subject", subject,
		"keys", ignored)
}
