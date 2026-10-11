package skillscan

import (
	"context"
	"errors"
)

// k8sBackend is the k8s-mode scan backend.
//
// v1 does not implement SPDY exec against the manager pod: embedded (docker)
// clusters are the production deployment today, and a half-verified k8s
// exec path would be worse than a clean fail-closed (uploads mark
// scan.status="skipped"; assign materialization skips with a warning).
// The follow-up (client-go remotecommand against the manager pod, the
// payload streamed over the exec channel) is tracked as a known
// limitation of this PR.
type k8sBackend struct{}

func (b *k8sBackend) Run(ctx context.Context, name string, files map[string][]byte) (string, error) {
	return "", errors.New("skill scan: k8s exec backend not implemented (embedded/docker mode only in v1)")
}
