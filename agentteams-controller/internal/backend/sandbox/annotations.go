package sandbox

// Annotations recorded on the underlying sandbox CR.
const (
	// AnnotationLastPausedTime is an RFC3339 timestamp written when the
	// sandbox is paused (hibernated). Bookkeeping only; not consumed by
	// reconcile logic, but visible to operators via `kubectl describe sandbox`.
	AnnotationLastPausedTime = "agentteams.io/last-paused-time"
)
