package sandbox

// Phase constants for sandbox status mapping.
// These are the provider-reported phases that the sandbox controller
// translates to WorkerStatus values in the backend package.
const (
	PhaseRunning     = "Running"
	PhaseStarting    = "Starting"
	PhaseResuming    = "Resuming"
	PhasePending     = "Pending"
	PhaseHibernating = "Hibernating"
	PhaseHibernated  = "Hibernated"
	PhaseFailed      = "Failed"
	PhaseTerminated  = "Terminated"

	// PhaseTerminating is a synthetic phase returned by GetStatus when the
	// CR still exists but carries a non-zero metadata.deletionTimestamp —
	// i.e. the CR is inside its finalizer window. The backend maps this
	// to WorkerStatus=Starting so the reconciler waits for GC to finish
	// instead of racing an AlreadyExists on Create.
	PhaseTerminating = "Terminating"
)
