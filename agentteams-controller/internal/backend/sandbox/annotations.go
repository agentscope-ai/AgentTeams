package sandbox

// Annotations recorded on the underlying sandbox CR.
const (
	// AnnotationLastAppliedSpecHash is kept only as a legacy migration
	// fallback for sandbox resources created by older controllers. New
	// resources no longer write it; owning CR status.specHash is authoritative.
	AnnotationLastAppliedSpecHash = "agentteams.io/last-applied-spec-hash"

	// AnnotationLastPausedTime is an RFC3339 timestamp written when the
	// sandbox is paused (hibernated). Bookkeeping only; not consumed by
	// reconcile logic, but visible to operators via `kubectl describe sandbox`.
	AnnotationLastPausedTime = "agentteams.io/last-paused-time"
)
