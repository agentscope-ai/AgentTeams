package sandbox

import "errors"

// Sentinel errors for sandbox plugin operations.
var (
	ErrCapabilityNotSupported = errors.New("capability not supported by provider")
	ErrProviderUnavailable    = errors.New("sandbox provider unavailable")
	ErrInvalidConfig          = errors.New("invalid sandbox provider config")

	// ErrAlreadyExists signals that the target sandbox name is currently
	// occupied — either a live CR with the same name, or a CR still in its
	// terminating/finalizer phase after a recent Delete. Callers in
	// backend/ translate this to backend.ErrConflict so the reconciler
	// treats it as "requeue and retry", matching K8sBackend's Pod
	// AlreadyExists handling.
	ErrAlreadyExists = errors.New("sandbox already exists or is being deleted")

	// ErrNotFound signals that the sandbox CR does not exist at all (as
	// opposed to "exists but phase==Terminated" or "exists but being
	// deleted"). The backend layer maps this to WorkerStatus=NotFound so
	// the reconciler can create a fresh CR. Any non-NotFound error from
	// the provider must NOT be collapsed into this sentinel — doing so
	// causes the reconciler to interpret transient API errors as "the
	// sandbox disappeared" and recreate it, which is exactly the bug
	// this sentinel is meant to prevent.
	ErrNotFound = errors.New("sandbox not found")
)
