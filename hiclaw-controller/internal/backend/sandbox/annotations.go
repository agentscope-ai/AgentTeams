package sandbox

// Annotations recorded on the underlying sandbox CR. Treated as the source
// of truth for the controller's "last applied" bookkeeping so that the
// Worker / Team / Manager CRDs do not need extra status fields just for
// sandbox-backend-specific signals.
const (
	// AnnotationLastAppliedSpecHash records the controller-computed hash of
	// the source spec (excluding State) at the time the sandbox CR was last
	// (re)created. The reconciler compares it against the current desired
	// hash to decide between Resume and Delete+Create when the CR is found
	// in StatusStopped (Hibernated) or StatusRunning.
	//
	// Current hash scope (fnv64a over json.Marshal with State=nil):
	//   Worker: Model, Runtime, Image, WorkerName, Identity, Soul, Agents,
	//           Skills, RemoteSkills, McpServers, Package, Expose,
	//           ChannelPolicy, AccessEntries, Labels, Env.
	//   Manager: Model, Runtime, Image, Soul, Agents, Skills, McpServers,
	//            Package, Config, AccessEntries, Labels, Env.
	//
	// TODO: When Agent-side hot-reload lands (file watcher / Matrix reload),
	// narrow the hash to pod-affecting fields only (Image, Runtime, Model,
	// Env, Labels, AccessEntries, Expose) and handle config-only changes
	// (Soul, Agents, Skills, McpServers, Package) via the reload channel.
	AnnotationLastAppliedSpecHash = "hiclaw.io/last-applied-spec-hash"

	// AnnotationLastPausedTime is an RFC3339 timestamp written when the
	// sandbox is paused (hibernated). Bookkeeping only; not consumed by
	// reconcile logic, but visible to operators via `kubectl describe sandbox`.
	AnnotationLastPausedTime = "hiclaw.io/last-paused-time"
)
