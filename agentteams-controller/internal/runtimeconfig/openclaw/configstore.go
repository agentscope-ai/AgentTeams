package openclaw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
)

// execTimeout bounds every single container exec performed by the manager.
const execTimeout = 30 * time.Second

// ExecBackend is the container exec capability the write path needs
// (backend.WorkerExecBackend; embedded docker mode only).
type ExecBackend interface {
	Exec(ctx context.Context, name string, command []string, timeout time.Duration) (stdout, stderr string, exitCode int, err error)
	WriteFile(ctx context.Context, name, path, content string) error
}

// KeyStore is the object-storage capability for desired state + snapshots
// (oss.StorageClient satisfies it). Keys are relative to the storage
// prefix, same convention as the deployer's agents/{worker}/openclaw.json.
type KeyStore interface {
	PutObject(ctx context.Context, key string, data []byte) error
	GetObject(ctx context.Context, key string) ([]byte, error)
	Stat(ctx context.Context, key string) error
}

// Errors the server layer maps to HTTP statuses:
//
//	UnknownFieldsError   -> 422
//	ValidationError      -> 422
//	StaleRevisionError   -> 409
//	NoWritableKeysError  -> 422
//	UnknownRuntimeError  -> 404 (carries the supported list)
//	ConfigFileMissingError -> 502 (the worker's runtime is not in a
//	                       readable state; the controller must not guess)
type (
	UnknownFieldsError  struct{ Fields []string }
	StaleRevisionError  struct{ Current, Expected string }
	NoWritableKeysError struct{ Unsupported []UnsupportedKey }
	UnknownRuntimeError struct {
		Runtime   string
		Supported []string
	}
	ConfigFileMissingError struct{ Worker string }
	// ValidationError is a 422-class write rejection: the key is declared
	// (or sits below a declared container the surface refuses) but the
	// value fails type/enum/secret-shape validation.
	ValidationError struct {
		Key string
		Err error
	}
)

func (e *UnknownFieldsError) Error() string {
	return fmt.Sprintf("undeclared openclaw config keys: %s", strings.Join(e.Fields, ", "))
}
func (e *ValidationError) Error() string {
	return "key " + e.Key + ": " + e.Err.Error()
}
func (e *ValidationError) Unwrap() error { return e.Err }
func (e *StaleRevisionError) Error() string {
	return "config changed since it was read (stale if-rev); re-read and retry"
}
func (e *NoWritableKeysError) Error() string {
	return "no writable keys: every submitted key was unsupported"
}
func (e *UnknownRuntimeError) Error() string {
	return fmt.Sprintf("runtime %q has no native config surface; supported: %s", e.Runtime, strings.Join(e.Supported, ", "))
}
func (e *ConfigFileMissingError) Error() string {
	return "worker " + e.Worker + " has no readable openclaw.json"
}

// UnsupportedKey is a submitted key the write skipped, with the reason.
// Credential invariants: an unresolvable env ref is skipped and reported —
// it is NEVER downgraded to a plaintext value.
type UnsupportedKey struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// DesiredState is the controller's last requested configuration, persisted
// under agents/{worker}/config/desired.json. It carries the pre-write
// appliedConfigHash baseline that the baseline-diff reconcile (Read/Write)
// diffs the gateway's current hash against.
type DesiredState struct {
	Rev string `json:"rev"` // sha256 hex of the written content bytes
	// BaselineHash is the gateway's appliedConfigHash captured BEFORE the
	// write (null = the runtime had applied no config yet). The write is
	// "applied" when the gateway's hash moves away from it.
	BaselineHash *string `json:"baselineHash"`
	// BaselineKnown is false when the pre-write gateway query failed: the
	// write is then unverifiable and can never report "applied".
	BaselineKnown bool           `json:"baselineKnown"`
	UpdatedAt     string         `json:"updatedAt"`
	WrittenKeys   []string       `json:"writtenKeys"`           // concrete leaves of the last write
	RestartKeys   []string       `json:"restartKeys,omitempty"` // written leaves with applies=restart
	Content       map[string]any `json:"content"`               // full desired document
}

// Applied-state vocabulary (design §4.3). Reconcile is fail-closed
// (§5.2): a write is "applied" only when the worker's gateway reports a
// config it actually applied that moved since the pre-write baseline, and
// a reconcile that cannot be completed is "pending" — never "applied".
const (
	StateApplied            = "applied"
	StatePending            = "pending"
	StateUnapplied          = "unapplied"
	StateAppliedButInactive = "applied-but-inactive"
)

// ConfigState is the GET /config projection.
type ConfigState struct {
	Runtime string         `json:"runtime"`
	Rev     string         `json:"rev"` // sha256 hex of the current on-disk bytes (optimistic-lock token)
	Values  map[string]any `json:"values"`
	// Applied is the reconcile verdict: applied | pending | unapplied |
	// applied-but-inactive (State* constants).
	Applied string `json:"applied"`
	// PendingRestart lists the applies=restart keys of the last
	// controller write. The frontend shows the "restart required" banner
	// from it; the controller never restarts the worker itself.
	PendingRestart []string `json:"pendingRestart"`
	// Inactive lists written channel ids whose channel plugin is not
	// loaded (or has no plugin at all): the config is applied to the file
	// but inert at runtime — the "applied-but-inactive" state (§8.4).
	Inactive []string `json:"inactive,omitempty"`
}

// WriteResult is the PUT/rollback response.
type WriteResult struct {
	Runtime string `json:"runtime"`
	Rev     string `json:"rev"` // sha256 hex of the new on-disk bytes
	// Applied is the post-write reconcile verdict (State* constants):
	// "applied" when the gateway's applied hash moved within the poll
	// window, "pending" when it has not (or could not be observed).
	Applied        string           `json:"applied"`
	PendingRestart []string         `json:"pendingRestart"`
	Inactive       []string         `json:"inactive,omitempty"`
	Unsupported    []UnsupportedKey `json:"unsupported,omitempty"`
	// Snapshot is the storage key of the pre-write snapshot:
	// rollback = POST .../config/rollback?rev=<pre-write rev>.
	Snapshot string `json:"snapshot"`
}

// Reconcile timing. The gateway's file watch applies a changed
// openclaw.json within well under a second in practice; the window is a
// generous bound after which the write is reported "pending" (subsequent
// GETs settle it, and the grace window eventually flips it to
// "unapplied").
const (
	defaultReconcileWindow   = 10 * time.Second
	defaultReconcileInterval = 200 * time.Millisecond
	defaultReconcileGrace    = 30 * time.Second
)

// ConfigManager performs read/reconcile/write/rollback of one runtime's
// native config file inside a worker container, with desired state and
// pre-write snapshots in object storage. All reconcile outcomes are
// fail-closed: any read/verify failure is an error, never "applied".
type ConfigManager struct {
	adapter *Adapter
	exec    ExecBackend
	store   KeyStore

	reconcileWindow   time.Duration
	reconcileInterval time.Duration
	reconcileGrace    time.Duration
}

// Option tunes reconcile timing (tests use millisecond values).
type Option func(*ConfigManager)

// WithReconcileWindow bounds the post-write poll for the gateway's
// applied hash to move.
func WithReconcileWindow(d time.Duration) Option {
	return func(m *ConfigManager) { m.reconcileWindow = d }
}

// WithReconcileInterval is the delay between applied-hash probes.
func WithReconcileInterval(d time.Duration) Option {
	return func(m *ConfigManager) { m.reconcileInterval = d }
}

// WithReconcileGrace is how long a still-unchanged applied hash is
// reported "pending" on read before it is reported "unapplied".
func WithReconcileGrace(d time.Duration) Option {
	return func(m *ConfigManager) { m.reconcileGrace = d }
}

func NewConfigManager(adapter *Adapter, exec ExecBackend, store KeyStore, opts ...Option) *ConfigManager {
	m := &ConfigManager{
		adapter:           adapter,
		exec:              exec,
		store:             store,
		reconcileWindow:   defaultReconcileWindow,
		reconcileInterval: defaultReconcileInterval,
		reconcileGrace:    defaultReconcileGrace,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Each HTTP request constructs a ConfigManager. Share the mutation lock
// across managers so revision checks, snapshots, file replacement and desired
// state are one transaction per worker within the controller process.
var configMutationLocks = struct {
	sync.Mutex
	workers map[string]*configMutationLock
}{workers: make(map[string]*configMutationLock)}

type configMutationLock struct {
	sync.Mutex
	users int
}

func lockConfigMutation(worker string) func() {
	configMutationLocks.Lock()
	lock := configMutationLocks.workers[worker]
	if lock == nil {
		lock = &configMutationLock{}
		configMutationLocks.workers[worker] = lock
	}
	lock.users++
	configMutationLocks.Unlock()
	lock.Lock()
	return func() {
		lock.Unlock()
		configMutationLocks.Lock()
		lock.users--
		if lock.users == 0 {
			delete(configMutationLocks.workers, worker)
		}
		configMutationLocks.Unlock()
	}
}

// Runtime is the runtime id this manager writes.
func (m *ConfigManager) Runtime() string { return m.adapter.Surface().Runtime }

// revOf is the optimistic-lock token: sha256 of the exact file bytes.
func revOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

var revPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidRev reports whether rev is a well-formed optimistic-lock token
// (64-char lowercase sha256 hex).
func ValidRev(rev string) bool { return revPattern.MatchString(rev) }

func desiredKey(worker string) string {
	return "agents/" + worker + "/config/desired.json"
}

func snapshotKey(worker, rev string) string {
	return "agents/" + worker + "/config/previous/" + rev + ".json"
}

// --- container file access -----------------------------------------------

// resolveConfigPath finds the worker's openclaw.json. The workspace root
// (${QWENPAW_WORKING_DIR:-$HOME}, the coding-cli workerHome contract) is
// tried first; $HOME/.openclaw/openclaw.json second.
func (m *ConfigManager) resolveConfigPath(ctx context.Context, worker string) (string, error) {
	root, err := m.shellPrintf(ctx, worker, `printf %s "${QWENPAW_WORKING_DIR:-$HOME}"`)
	if err != nil {
		return "", err
	}
	if root != "" {
		if p := root + "/openclaw.json"; m.fileExists(ctx, worker, p) {
			return p, nil
		}
	}
	home, err := m.shellPrintf(ctx, worker, `printf %s "${HOME:-}"`)
	if err != nil {
		return "", err
	}
	if home != "" && home != root {
		if p := home + "/.openclaw/openclaw.json"; m.fileExists(ctx, worker, p) {
			return p, nil
		}
	}
	return "", &ConfigFileMissingError{Worker: worker}
}

// shellPrintf runs a shell printf and returns its trimmed stdout. The
// script is a fixed literal; only stdout varies.
func (m *ConfigManager) shellPrintf(ctx context.Context, worker, script string) (string, error) {
	out, stderr, exit, err := m.exec.Exec(ctx, worker, []string{"sh", "-c", script}, execTimeout)
	if err != nil {
		return "", fmt.Errorf("exec %s: %w", script, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("exec %s exit %d: %s", script, exit, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(out), nil
}

func (m *ConfigManager) fileExists(ctx context.Context, worker, path string) bool {
	_, _, exit, err := m.exec.Exec(ctx, worker, []string{"sh", "-c", `test -f "$1"`, "sh", path}, execTimeout)
	return err == nil && exit == 0
}

func (m *ConfigManager) readConfigFile(ctx context.Context, worker, path string) ([]byte, error) {
	out, stderr, exit, err := m.exec.Exec(ctx, worker, []string{"sh", "-c", `cat -- "$1"`, "sh", path}, execTimeout)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("read %s exit %d: %s", path, exit, strings.TrimSpace(stderr))
	}
	return []byte(out), nil
}

// --- read / reconcile ------------------------------------------------------

// Read returns the worker's current config state (GET /config
// projection). Reconcile semantics (fail-closed, design §5.2):
//
//   - rev is always the sha256 of the current on-disk bytes;
//   - with no controller desired state, applied is "applied" (nothing
//     is outstanding: the file is what the runtime sees);
//   - a desired state whose pre-write baseline is unknown (the gateway
//     was unreachable during the write) stays "pending" forever — a
//     reconcile that cannot be completed is never "applied";
//   - otherwise the gateway's current appliedConfigHash is diffed
//     against the pre-write baseline: unchanged within the grace window
//     is "pending", unchanged beyond it is "unapplied" (the write is NOT
//     retried, §5.2), changed is "applied" — refined to
//     "applied-but-inactive" when a written channel's plugin is not
//     loaded (§8.4: the hash proves the config was applied, not that the
//     channel plugin is running);
//   - pendingRestart lists the applies=restart keys of the last
//     controller write (the frontend shows the restart banner; the
//     controller cannot observe a manual restart and never performs one).
func (m *ConfigManager) Read(ctx context.Context, worker string) (ConfigState, error) {
	unlock := lockConfigMutation(worker)
	defer unlock()
	return m.read(ctx, worker)
}

func (m *ConfigManager) read(ctx context.Context, worker string) (ConfigState, error) {
	path, err := m.resolveConfigPath(ctx, worker)
	if err != nil {
		return ConfigState{}, err
	}
	raw, err := m.readConfigFile(ctx, worker, path)
	if err != nil {
		return ConfigState{}, err
	}
	doc, err := parseConfigDoc(raw)
	if err != nil {
		return ConfigState{}, fmt.Errorf("worker %s openclaw.json is not a valid JSON object: %w", worker, err)
	}
	desired, err := m.loadDesired(ctx, worker)
	if err != nil {
		return ConfigState{}, err
	}
	st := ConfigState{
		Runtime:        m.Runtime(),
		Rev:            revOf(raw),
		Values:         m.redactSensitive(doc),
		Applied:        StatePending,
		PendingRestart: []string{},
	}
	if desired == nil {
		// Nothing outstanding: the file is what the runtime sees.
		st.Applied = StateApplied
		return st, nil
	}
	st.PendingRestart = desired.RestartKeys
	if st.PendingRestart == nil {
		st.PendingRestart = []string{}
	}
	if !desired.BaselineKnown {
		return st, nil // Applied stays "pending" (fail-closed)
	}
	current, err := m.gatewayAppliedHash(ctx, worker)
	if err != nil {
		return ConfigState{}, err
	}
	if !hashMoved(desired.BaselineHash, current) {
		age, err := desiredAge(desired)
		if err != nil {
			return ConfigState{}, err
		}
		if age < m.reconcileGrace {
			return st, nil // Applied stays "pending"
		}
		st.Applied = StateUnapplied
		return st, nil
	}
	inactive, err := m.checkChannelPlugins(ctx, worker, desired.WrittenKeys)
	if err != nil {
		return ConfigState{}, err
	}
	st.Applied = StateApplied
	st.Inactive = inactive
	if len(inactive) > 0 {
		st.Applied = StateAppliedButInactive
	}
	return st, nil
}

func (m *ConfigManager) loadDesired(ctx context.Context, worker string) (*DesiredState, error) {
	raw, err := m.store.GetObject(ctx, desiredKey(worker))
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read desired state: %w", err)
	}
	var d DesiredState
	if err := json.Unmarshal(raw, &d); err != nil {
		// Corrupt desired state must not be silently treated as "no
		// desired state" (that would report applied=true): fail closed.
		return nil, fmt.Errorf("desired state for worker %s is corrupt: %w", worker, err)
	}
	return &d, nil
}

// checkChannelPlugins verifies, for every written channels.<id>.* key,
// that a plugin serving the channel is loaded. Returns the ids that are
// not (applied-but-inactive). A failed plugins-list exec is an error
// (fail-closed), never an empty list.
func (m *ConfigManager) checkChannelPlugins(ctx context.Context, worker string, writtenKeys []string) ([]string, error) {
	ids := channelIDsFromKeys(writtenKeys)
	if len(ids) == 0 {
		return nil, nil
	}
	out, stderr, exit, err := m.exec.Exec(ctx, worker, []string{"openclaw", "plugins", "list", "--json"}, execTimeout)
	if err != nil {
		return nil, fmt.Errorf("plugin state check failed: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("plugin state check failed: openclaw plugins list exit %d: %s", exit, strings.TrimSpace(stderr))
	}
	var payload struct {
		Plugins []struct {
			ID         string   `json:"id"`
			Enabled    bool     `json:"enabled"`
			Status     string   `json:"status"`
			ChannelIDs []string `json:"channelIds"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, fmt.Errorf("plugin state check failed: unparseable plugins list: %w", err)
	}
	var inactive []string
	for _, id := range ids {
		loaded := false
		for _, p := range payload.Plugins {
			if p.ID == id || contains(p.ChannelIDs, id) {
				loaded = p.Enabled && p.Status == "loaded"
				break
			}
		}
		if !loaded {
			inactive = append(inactive, id)
		}
	}
	return inactive, nil
}

// channelIDsFromKeys extracts the distinct channels.<id> first-level ids
// from written keys, excluding "defaults" (a shared policy section, not a
// channel) and top-level channels.* wildcard-ish entries.
func channelIDsFromKeys(keys []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, k := range keys {
		segs := strings.Split(k, ".")
		if len(segs) < 3 || segs[0] != "channels" {
			continue
		}
		id := segs[1]
		if id == "" || id == "defaults" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- gateway reconcile (appliedConfigHash baseline-diff) -------------------

// gatewayAppliedHash asks the worker's own gateway for the config it has
// actually applied: `openclaw gateway call config.get --json` →
// appliedConfigHash (string|null; null = the runtime has applied no
// config yet). The hash is an opaque token from the gateway's revision
// projector — the controller never recomputes it (re-implementing the
// gateway's hashing in Go would drift from the runtime); only
// equality/inequality matters.
func (m *ConfigManager) gatewayAppliedHash(ctx context.Context, worker string) (*string, error) {
	out, stderr, exit, err := m.exec.Exec(ctx, worker, []string{"openclaw", "gateway", "call", "config.get", "--json"}, execTimeout)
	if err != nil {
		return nil, fmt.Errorf("gateway applied-config query: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("gateway applied-config query exit %d: %s", exit, strings.TrimSpace(stderr))
	}
	var res struct {
		AppliedConfigHash *string `json:"appliedConfigHash"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return nil, fmt.Errorf("gateway applied-config query: unparseable config.get response: %w", err)
	}
	return res.AppliedConfigHash, nil
}

// hashMoved reports whether the gateway's applied hash differs from the
// pre-write baseline. A nil side normalizes to ""; the gateway's hashes
// are non-empty, so the comparison is unambiguous.
func hashMoved(baseline, current *string) bool {
	b, c := "", ""
	if baseline != nil {
		b = *baseline
	}
	if current != nil {
		c = *current
	}
	return b != c
}

// desiredAge is how long ago the last controller write was recorded. An
// unparseable timestamp is corrupt state: fail closed with an error
// rather than guessing freshness.
func desiredAge(desired *DesiredState) (time.Duration, error) {
	at, err := time.Parse(time.RFC3339, desired.UpdatedAt)
	if err != nil {
		return 0, fmt.Errorf("desired state updatedAt %q is not RFC3339: %w", desired.UpdatedAt, err)
	}
	return time.Since(at), nil
}

// pollApplied waits, bounded by the reconcile window, for the gateway's
// applied hash to move away from the pre-write baseline, then refines the
// verdict with the channel-plugin state (§8.4). It never fails the write:
// the file write is already byte-verified; an unfinished reconcile
// reports "pending" (the next GET re-probes, and the grace window
// eventually flips it to "unapplied") — never "applied" (§5.2).
func (m *ConfigManager) pollApplied(ctx context.Context, worker string, baseline *string, baselineKnown bool, writtenKeys []string) (string, []string, error) {
	if !baselineKnown {
		return StatePending, nil, nil
	}
	deadline := time.Now().Add(m.reconcileWindow)
	for {
		current, err := m.gatewayAppliedHash(ctx, worker)
		if err != nil {
			// Transient gateway trouble: stop probing, report "pending";
			// the next GET settles it. Never "applied" on a failed probe.
			return StatePending, nil, nil
		}
		if hashMoved(baseline, current) {
			inactive, err := m.checkChannelPlugins(ctx, worker, writtenKeys)
			if err != nil {
				return "", nil, err
			}
			if len(inactive) > 0 {
				return StateAppliedButInactive, inactive, nil
			}
			return StateApplied, nil, nil
		}
		if time.Now().After(deadline) {
			return StatePending, nil, nil
		}
		select {
		case <-ctx.Done():
			return StatePending, nil, nil
		case <-time.After(m.reconcileInterval):
		}
	}
}

// redactSensitive replaces every value at a sensitive field path with
// "***" unless it is already a secret ref (refs carry no material and are
// returned as-is — "sensitive keys read back as ref only").
func (m *ConfigManager) redactSensitive(doc map[string]any) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = m.redactNode(k, v)
	}
	return out
}

func (m *ConfigManager) redactNode(path string, v any) any {
	f, declared := m.adapter.FieldForKey(path)
	// Runtime plugins may add credentials outside the static surface (for
	// example channels.matrix.accessToken). Do not expose those strings on GET.
	if !declared {
		key := strings.ToLower(path[strings.LastIndex(path, ".")+1:])
		credential := key == "authorization" || key == "proxy-authorization" || key == "cookie" || key == "set-cookie"
		for _, part := range []string{"api_key", "apikey", "api-key", "token", "secret", "password", "access_key"} {
			credential = credential || strings.Contains(key, part)
		}
		if value, ok := v.(string); credential && ok && value != "" {
			return "***"
		}
	}
	if declared && IsSecret(f) {
		if _, err := ValidateSecretRef(v); err != nil {
			return "***"
		}
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		child := make(map[string]any, len(t))
		for k, cv := range t {
			child[k] = m.redactNode(path+"."+k, cv)
		}
		return child
	case []any:
		child := make([]any, len(t))
		for i, cv := range t {
			child[i] = m.redactNode(path+"."+strconv.Itoa(i), cv)
		}
		return child
	}
	return v
}

// --- write -----------------------------------------------------------------

// Write merges values (a partial document of declared keys) into the
// worker's openclaw.json:
//
//  1. resolve + read the current file; if-rev mismatch -> StaleRevisionError
//  2. every leaf must be a declared key (422), type/enum-validated;
//     sensitive leaves must be secret refs; env refs must resolve in the
//     container (printenv) or the key is skipped as unsupported — never
//     downgraded to plaintext;
//  3. pre-write snapshot -> agents/{worker}/config/previous/<current rev>.json
//  4. atomic write (tmp+rename in the backend) + read-back hash verify
//  5. capture the gateway's pre-write appliedConfigHash baseline; persist
//     desired state; post-write poll: "applied" when the gateway's hash
//     moves within the reconcile window, "pending" otherwise (fail-closed,
//     §5.2); written channels refine "applied" to "applied-but-inactive"
//     when their plugin is not loaded (§8.4)
func (m *ConfigManager) Write(ctx context.Context, worker string, values map[string]any, ifRev string) (WriteResult, error) {
	unlock := lockConfigMutation(worker)
	defer unlock()

	path, err := m.resolveConfigPath(ctx, worker)
	if err != nil {
		return WriteResult{}, err
	}
	raw, err := m.readConfigFile(ctx, worker, path)
	if err != nil {
		return WriteResult{}, err
	}
	curRev := revOf(raw)
	if ifRev != "" && ifRev != curRev {
		return WriteResult{}, &StaleRevisionError{Current: curRev, Expected: ifRev}
	}
	current, err := parseConfigDoc(raw)
	if err != nil {
		return WriteResult{}, fmt.Errorf("worker %s openclaw.json is not a valid JSON object: %w", worker, err)
	}

	leaves := collectLeaves("", values, m.adapter.isAtomicSecretLeaf)
	var unknown []string
	var validation *ValidationError
	sensitiveLeaves := map[string]SecretRef{}
	for _, leaf := range leaves {
		f, mode, ok := m.adapter.ResolveKey(leaf.key)
		if !ok {
			unknown = append(unknown, leaf.key)
			continue
		}
		if mode == KeyOpaque {
			// Opaque container: the surface does not decompose it, so the
			// value passes through unvalidated and OpenClaw's zod strict
			// validation is the final arbiter. A declared SENSITIVE
			// container is the one exception: nested writes through an
			// opaque bypass would silently weaken the ref-only rule, so
			// they are refused (fail-closed).
			if f.Sensitive {
				validation = &ValidationError{
					Key: leaf.key,
					Err: fmt.Errorf("nested writes below sensitive object %q are not supported", f.Key),
				}
				continue
			}
			continue
		}
		if err := m.adapter.ValidateValue(leaf.key, leaf.value); err != nil {
			if validation == nil {
				validation = &ValidationError{Key: leaf.key, Err: err}
			}
			continue
		}
		if IsSecret(f) {
			ref, err := ValidateSecretRef(leaf.value)
			if err != nil {
				if validation == nil {
					validation = &ValidationError{Key: leaf.key, Err: err}
				}
				continue
			}
			sensitiveLeaves[leaf.key] = *ref
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return WriteResult{}, &UnknownFieldsError{Fields: unknown}
	}
	if validation != nil {
		return WriteResult{}, validation
	}

	// Credential invariants: an env ref that does not resolve in the
	// container makes the key unsupported — skip it, report it, and never
	// fall back to writing anything else. file/exec/store refs are
	// structurally validated only (their providers run in the runtime).
	var unsupported []UnsupportedKey
	clone := func(src map[string]any) map[string]any {
		out := make(map[string]any, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	pruned := m.pruneUnsupported(ctx, worker, "", values, sensitiveLeaves, &unsupported, clone)
	if pruned == nil {
		return WriteResult{}, &NoWritableKeysError{Unsupported: unsupported}
	}

	// Reconcile baseline: the gateway's appliedConfigHash BEFORE the file
	// write (only the gateway's own reload moves it). A failed capture
	// does not abort the write — the file plane is independent — but it
	// makes the write unverifiable, so it can never report "applied"
	// (fail-closed, §5.2).
	var baseline *string
	baselineKnown := true
	if b, err := m.gatewayAppliedHash(ctx, worker); err != nil {
		baselineKnown = false
	} else {
		baseline = b
	}

	newDoc := deepMerge(current, pruned)
	newContent, err := json.MarshalIndent(newDoc, "", "  ")
	if err != nil {
		return WriteResult{}, fmt.Errorf("encode config: %w", err)
	}
	newContent = append(newContent, '\n')

	snapKey := snapshotKey(worker, curRev)
	if err := m.store.PutObject(ctx, snapKey, raw); err != nil {
		return WriteResult{}, fmt.Errorf("store pre-write snapshot: %w", err)
	}
	if err := m.exec.WriteFile(ctx, worker, path, string(newContent)); err != nil {
		return WriteResult{}, fmt.Errorf("write openclaw.json into worker: %w", err)
	}
	// Fail-closed verify: the on-disk bytes must be exactly what was meant
	// to be written, or the write is a failure (desired state is NOT
	// advanced, so the next read reports the drift).
	back, err := m.readConfigFile(ctx, worker, path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("read-back after write: %w", err)
	}
	if revOf(back) != revOf(newContent) {
		return WriteResult{}, errors.New("read-back after write does not match the intended content; state unknown")
	}

	writtenKeys := collectLeafKeys("", pruned, m.adapter.isAtomicSecretLeaf)
	restartKeys := make([]string, 0)
	for _, k := range writtenKeys {
		// Opaque leaves inherit the applies level of their declared
		// container.
		if f, _, ok := m.adapter.ResolveKey(k); ok && f.Applies == runtimeconfig.AppliesRestart {
			restartKeys = append(restartKeys, k)
		}
	}
	desired := DesiredState{
		Rev:           revOf(newContent),
		BaselineHash:  baseline,
		BaselineKnown: baselineKnown,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
		WrittenKeys:   writtenKeys,
		RestartKeys:   restartKeys,
		Content:       newDoc,
	}
	encoded, err := json.Marshal(desired)
	if err != nil {
		return WriteResult{}, fmt.Errorf("encode desired state: %w", err)
	}
	if err := m.store.PutObject(ctx, desiredKey(worker), encoded); err != nil {
		// The file was written but the desired state was not persisted:
		// the next read would report drift. Surface this as an error.
		return WriteResult{}, fmt.Errorf("persist desired state (file already written): %w", err)
	}
	applied, inactive, err := m.pollApplied(ctx, worker, baseline, baselineKnown, writtenKeys)
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{
		Runtime:        m.Runtime(),
		Rev:            revOf(newContent),
		Applied:        applied,
		PendingRestart: restartKeys,
		Inactive:       inactive,
		Unsupported:    unsupported,
		Snapshot:       snapKey,
	}, nil
}

type leaf struct {
	key   string
	value any
}

// isAtomicSecretLeaf reports whether v at key is the value of a declared
// secret field. Such a value is an OpenClaw SecretRef object and must stay
// atomic: decomposing it into .source/.provider/.id would make those keys
// unknown fields before ValidateSecretRef ever runs.
func (a *Adapter) isAtomicSecretLeaf(key string, v any) bool {
	if _, ok := v.(map[string]any); !ok {
		return false
	}
	f, mode, ok := a.ResolveKey(key)
	// Only a field declared with type "secret" carries a SecretRef value.
	// A sensitive *object* container (e.g. agents.defaults.sandbox) stays
	// decomposed on purpose: nested writes below it must keep failing
	// closed at their own key rather than being read as a malformed ref.
	return ok && mode == KeyExact && f.Type == "secret"
}

// collectLeaves walks a partial document to its concrete leaves (non-map
// values). Empty objects contribute no leaves. When atomic is non-nil, a
// value for which atomic(key, v) holds stays a single leaf at its full key
// instead of being decomposed (declared secret refs).
func collectLeaves(prefix string, doc map[string]any, atomic func(string, any) bool) []leaf {
	var out []leaf
	for k, v := range doc {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if atomic != nil && atomic(key, v) {
			out = append(out, leaf{key: key, value: v})
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			out = append(out, collectLeaves(key, sub, atomic)...)
			continue
		}
		out = append(out, leaf{key: key, value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// collectLeafKeys is the key projection of collectLeaves: same walk, same
// atomicity (atomic(key, v) values are reported under their full key).
func collectLeafKeys(prefix string, doc map[string]any, atomic func(string, any) bool) []string {
	out := make([]string, 0)
	for k, v := range doc {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if atomic != nil && atomic(key, v) {
			out = append(out, key)
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			out = append(out, collectLeafKeys(key, sub, atomic)...)
			continue
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// pruneUnsupported deep-copies doc, dropping every sensitive leaf whose env
// ref does not resolve in the container. Returns nil when nothing remains.
// Env resolution uses a direct `printenv <id>` exec (no shell: the id is a
// validated [A-Z][A-Z0-9_]* token and is passed as a single argument).
// Sensitive keys are checked BEFORE map recursion: a secret ref's value is
// itself a map (source/provider/id) that must never be decomposed.
func (m *ConfigManager) pruneUnsupported(ctx context.Context, worker, prefix string, doc map[string]any, sensitive map[string]SecretRef, unsupported *[]UnsupportedKey, clone func(map[string]any) map[string]any) map[string]any {
	out := clone(doc)
	empty := true
	for k, v := range out {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if ref, isSensitive := sensitive[key]; isSensitive {
			if ref.Source == "env" && !m.envResolves(ctx, worker, ref.ID) {
				*unsupported = append(*unsupported, UnsupportedKey{
					Key:    key,
					Reason: "env secret ref " + ref.ID + " does not resolve in the worker container; key skipped (plaintext fallback is forbidden)",
				})
				delete(out, k)
				continue
			}
			empty = false
			continue // atomic: never recurse into the ref
		}
		if sub, ok := v.(map[string]any); ok {
			child := m.pruneUnsupported(ctx, worker, key, sub, sensitive, unsupported, clone)
			if child == nil {
				delete(out, k)
				continue
			}
			out[k] = child
			empty = false
			continue
		}
		empty = false
	}
	if empty {
		return nil
	}
	return out
}

func (m *ConfigManager) envResolves(ctx context.Context, worker, id string) bool {
	out, _, exit, err := m.exec.Exec(ctx, worker, []string{"printenv", id}, execTimeout)
	if err != nil || exit != 0 {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// --- rollback ---------------------------------------------------------------

// Rollback rewrites the worker's openclaw.json with the pre-write snapshot
// taken when the state identified by rev was CURRENT (the snapshot key is
// previous/<rev>.json). It goes through the same snapshot + write +
// verify + desired-state + reconcile-baseline path as Write, so a rollback
// is itself reversible.
//
// Reload: OpenClaw's gateway watches openclaw.json and hot-applies
// writable sections automatically; restart keys stay pendingRestart.
func (m *ConfigManager) Rollback(ctx context.Context, worker, rev string) (WriteResult, error) {
	unlock := lockConfigMutation(worker)
	defer unlock()

	if !revPattern.MatchString(rev) {
		return WriteResult{}, fmt.Errorf("rev must be a 64-char sha256 hex string, got %q", rev)
	}
	snap, err := m.store.GetObject(ctx, snapshotKey(worker, rev))
	if err != nil {
		if isNotFound(err) {
			return WriteResult{}, fmt.Errorf("no pre-write snapshot for rev %s", rev)
		}
		return WriteResult{}, fmt.Errorf("read snapshot: %w", err)
	}
	doc, err := parseConfigDoc(snap)
	if err != nil {
		return WriteResult{}, fmt.Errorf("snapshot for rev %s is not a valid JSON object: %w", rev, err)
	}

	path, err := m.resolveConfigPath(ctx, worker)
	if err != nil {
		return WriteResult{}, err
	}
	curRaw, err := m.readConfigFile(ctx, worker, path)
	if err != nil {
		return WriteResult{}, err
	}
	curRev := revOf(curRaw)
	// A rollback to the current state is a no-op (still verified +
	// reported, but no snapshot churn).
	if curRev == rev {
		st, err := m.read(ctx, worker)
		if err != nil {
			return WriteResult{}, err
		}
		return WriteResult{
			Runtime:        st.Runtime,
			Rev:            st.Rev,
			Applied:        st.Applied,
			PendingRestart: st.PendingRestart,
			Inactive:       st.Inactive,
			Snapshot:       snapshotKey(worker, curRev),
		}, nil
	}

	// Reconcile baseline: the gateway's appliedConfigHash BEFORE the
	// rollback write (same fail-closed semantics as Write — a failed
	// capture does not abort, but makes the write unverifiable, §5.2).
	var baseline *string
	baselineKnown := true
	if b, err := m.gatewayAppliedHash(ctx, worker); err != nil {
		baselineKnown = false
	} else {
		baseline = b
	}

	// Snapshot the CURRENT state before overwriting it with the old one.
	snapKey := snapshotKey(worker, curRev)
	if err := m.store.PutObject(ctx, snapKey, curRaw); err != nil {
		return WriteResult{}, fmt.Errorf("store pre-write snapshot: %w", err)
	}
	if err := m.exec.WriteFile(ctx, worker, path, string(snap)); err != nil {
		return WriteResult{}, fmt.Errorf("write snapshot into worker: %w", err)
	}
	back, err := m.readConfigFile(ctx, worker, path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("read-back after rollback: %w", err)
	}
	if revOf(back) != rev {
		return WriteResult{}, errors.New("read-back after rollback does not match the snapshot; state unknown")
	}

	writtenKeys := collectLeafKeys("", doc, m.adapter.isAtomicSecretLeaf)
	restartKeys := make([]string, 0)
	for _, k := range writtenKeys {
		if f, _, ok := m.adapter.ResolveKey(k); ok && f.Applies == runtimeconfig.AppliesRestart {
			restartKeys = append(restartKeys, k)
		}
	}
	desired := DesiredState{
		Rev:           rev,
		BaselineHash:  baseline,
		BaselineKnown: baselineKnown,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
		WrittenKeys:   writtenKeys,
		RestartKeys:   restartKeys,
		Content:       doc,
	}
	encoded, err := json.Marshal(desired)
	if err != nil {
		return WriteResult{}, fmt.Errorf("encode desired state: %w", err)
	}
	if err := m.store.PutObject(ctx, desiredKey(worker), encoded); err != nil {
		return WriteResult{}, fmt.Errorf("persist desired state (file already written): %w", err)
	}
	applied, inactive, err := m.pollApplied(ctx, worker, baseline, baselineKnown, writtenKeys)
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{
		Runtime:        m.Runtime(),
		Rev:            rev,
		Applied:        applied,
		PendingRestart: restartKeys,
		Inactive:       inactive,
		Snapshot:       snapKey,
	}, nil
}

// --- shared helpers ----------------------------------------------------------

func parseConfigDoc(raw []byte) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errors.New("not a JSON object")
	}
	return doc, nil
}

// deepMerge merges incoming into current: objects merge recursively,
// arrays and scalars replace (OpenClaw config-patch semantics).
func deepMerge(current, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(current)+len(incoming))
	for k, v := range current {
		out[k] = v
	}
	for k, v := range incoming {
		if curSub, ok := out[k].(map[string]any); ok {
			if inSub, ok := v.(map[string]any); ok {
				out[k] = deepMerge(curSub, inSub)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// isNotFound reports whether an object-store error means "absent" rather
// than "failed". The MinIO client reports a missing object as
// os.ErrNotExist.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrNotExist)
}
