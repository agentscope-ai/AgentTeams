package hermes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
)

// This file implements the hermes member-config plane: the controller
// reads and writes the worker's own native $HERMES_HOME/config.yaml — no
// field translation, no runtime-agnostic schema. Hermes records no
// applied hash (unlike the openclaw gateway), so reconcile is the weak
// read-back form: a write is "applied" only while the on-disk bytes still
// match what the controller last wrote.

const (
	// execTimeout bounds every exec into the worker container.
	execTimeout = 30 * time.Second

	// redactedValue is the fixed placeholder the GET projection shows for
	// a declared sensitive value. The real value never crosses the API.
	redactedValue = "***"
)

type (
	// UnknownFieldsError is the 422 write rejection: submitted keys the
	// hermes surface does not declare (exact key or wildcard pattern).
	UnknownFieldsError struct{ Fields []string }
	// StaleRevisionError is the 409 write rejection: the file changed
	// between the caller's read and this write.
	StaleRevisionError struct{ Current, Expected string }
	// NoWritableKeysError is the 422 write rejection: the patch contains
	// no concrete leaf at all.
	NoWritableKeysError struct {
		Unsupported []runtimeconfig.UnsupportedKey
	}
	// ValidationError is the 422 write rejection: a declared key whose
	// value fails type/enum validation against the surface.
	ValidationError struct {
		Key string
		Err error
	}
)

func (e *UnknownFieldsError) Error() string { return "unknown fields: " + strings.Join(e.Fields, ", ") }
func (e *StaleRevisionError) Error() string {
	return "revision conflict: current " + e.Current + ", expected " + e.Expected
}
func (e *NoWritableKeysError) Error() string { return "no writable keys" }
func (e *ValidationError) Error() string     { return "key " + e.Key + ": " + e.Err.Error() }
func (e *ValidationError) Unwrap() error     { return e.Err }

// DesiredState is the controller's last requested configuration,
// persisted under agents/{worker}/config/desired.json — the same key
// openclaw uses: a worker has one config file and one runtime at a time.
// Hermes carries no gateway appliedConfigHash, so there is no baseline
// here; the weak read-back reconcile compares TargetRev (the hash of the
// content the controller meant to write) against the on-disk bytes.
//
// Rev is the revision observed by the post-write read-back, which can
// differ from TargetRev when another writer touched the file between
// activation and read-back (drift). Persisting the observed revision as
// the desired one would make the next GET compare the drifted file with
// itself and wrongly report "applied", so the two are kept apart.
type DesiredState struct {
	Rev         string         `json:"rev"`                 // sha256 hex observed by the post-write read-back
	TargetRev   string         `json:"targetRev,omitempty"` // sha256 hex of the intended content (what "applied" means)
	UpdatedAt   string         `json:"updatedAt"`
	WrittenKeys []string       `json:"writtenKeys"`
	RestartKeys []string       `json:"restartKeys,omitempty"`
	Content     map[string]any `json:"content"`
}

// ConfigManager performs read/write/rollback of a Hermes worker's native
// config.yaml inside the worker container, with desired state and
// pre-write snapshots in object storage.
type ConfigManager struct {
	validateChange runtimeconfig.ChangeValidator
	adapter        *Adapter
	exec           runtimeconfig.ExecBackend
	store          runtimeconfig.KeyStore
	surface        runtimeconfig.Surface
}

// NewConfigManager builds the hermes write plane. The adapter's surface
// is the single source of truth for declared keys, types, redaction and
// restart classification.
func NewConfigManager(adapter *Adapter, exec runtimeconfig.ExecBackend, store runtimeconfig.KeyStore) *ConfigManager {
	return &ConfigManager{
		adapter: adapter,
		exec:    exec,
		store:   store,
		surface: adapter.Surface(),
	}
}

var _ runtimeconfig.Manager = (*ConfigManager)(nil)

// SetChangeValidator binds request authorization before this manager is used.
func (m *ConfigManager) SetChangeValidator(validate runtimeconfig.ChangeValidator) {
	m.validateChange = validate
}

// Runtime is the runtime id this manager writes.
func (m *ConfigManager) Runtime() string { return m.surface.Runtime }

// Each request constructs a ConfigManager. Share the mutation lock across
// managers so revision check, snapshot, file replacement and desired
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
	lk := configMutationLocks.workers[worker]
	if lk == nil {
		lk = &configMutationLock{}
		configMutationLocks.workers[worker] = lk
	}
	lk.users++
	configMutationLocks.Unlock()
	lk.Lock()
	return func() {
		lk.Unlock()
		configMutationLocks.Lock()
		lk.users--
		if lk.users == 0 {
			delete(configMutationLocks.workers, worker)
		}
		configMutationLocks.Unlock()
	}
}

var revPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidRev reports whether rev is a well-formed optimistic-lock token
// (64-char lowercase sha256 hex).
func ValidRev(rev string) bool { return revPattern.MatchString(rev) }

func revOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func desiredKey(worker string) string {
	return "agents/" + worker + "/config/desired.json"
}

func snapshotKey(worker, rev string) string {
	return "agents/" + worker + "/config/previous/" + rev + ".json"
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrNotExist)
}

// Read is GET /config: the on-disk revision, the surface projection of
// the document (sensitive values redacted, foreign keys left in the file
// but out of Values), and the reconcile verdict.
func (m *ConfigManager) Read(ctx context.Context, worker string) (runtimeconfig.ConfigState, error) {
	unlock := lockConfigMutation(worker)
	defer unlock()
	return m.read(ctx, worker)
}

func (m *ConfigManager) read(ctx context.Context, worker string) (runtimeconfig.ConfigState, error) {
	path, err := m.resolveConfigPath(ctx, worker)
	if err != nil {
		return runtimeconfig.ConfigState{}, err
	}
	var raw []byte
	if m.fileExists(ctx, worker, path) {
		raw, err = m.readFile(ctx, worker, path)
		if err != nil {
			return runtimeconfig.ConfigState{}, err
		}
	}
	var doc map[string]any
	if raw != nil {
		doc, err = parseYamlDoc(raw)
		if err != nil {
			return runtimeconfig.ConfigState{}, fmt.Errorf("worker %s config.yaml is not a valid YAML mapping: %w", worker, err)
		}
	} else {
		doc = map[string]any{}
	}
	desired, err := m.loadDesired(ctx, worker)
	if err != nil {
		return runtimeconfig.ConfigState{}, err
	}
	st := runtimeconfig.ConfigState{
		Runtime:        m.Runtime(),
		Rev:            revOf(raw),
		Values:         m.projectValues(doc),
		Applied:        runtimeconfig.StatePending,
		PendingRestart: []string{},
	}
	if desired == nil {
		// Nothing outstanding: the file is what the runtime sees.
		st.Applied = runtimeconfig.StateApplied
		return st, nil
	}
	st.PendingRestart = desired.RestartKeys
	if st.PendingRestart == nil {
		st.PendingRestart = []string{}
	}
	// Weak read-back reconcile (no applied hash in Hermes): the write is
	// "applied" only while the on-disk bytes still match what the
	// controller meant to write (TargetRev); any drift keeps it "pending"
	// until a later write settles it. Legacy desired records predate
	// TargetRev and fall back to the observed Rev.
	targetRev := desired.TargetRev
	if targetRev == "" {
		targetRev = desired.Rev
	}
	if targetRev == st.Rev {
		st.Applied = runtimeconfig.StateApplied
	}
	return st, nil
}

// Write is PUT /config: merge a partial document of declared keys into
// the on-disk config.yaml, guarded by ifRev, with a pre-write snapshot in
// object storage and the post-write reconcile verdict.
func (m *ConfigManager) Write(ctx context.Context, worker string, values map[string]any, ifRev string) (runtimeconfig.WriteResult, error) {
	unlock := lockConfigMutation(worker)
	defer unlock()

	path, err := m.resolveConfigPath(ctx, worker)
	if err != nil {
		return runtimeconfig.WriteResult{}, err
	}
	var raw []byte
	if m.fileExists(ctx, worker, path) {
		raw, err = m.readFile(ctx, worker, path)
		if err != nil {
			return runtimeconfig.WriteResult{}, err
		}
	}
	curRev := revOf(raw)
	if ifRev != "" && ifRev != curRev {
		return runtimeconfig.WriteResult{}, &StaleRevisionError{Current: curRev, Expected: ifRev}
	}
	var doc map[string]any
	if raw != nil {
		doc, err = parseYamlDoc(raw)
		if err != nil {
			return runtimeconfig.WriteResult{}, fmt.Errorf("worker %s config.yaml is not a valid YAML mapping: %w", worker, err)
		}
	} else {
		doc = map[string]any{}
	}

	if fallback, present := values["fallback_providers"]; present {
		if err := validateFallbackRoutes(fallback); err != nil {
			return runtimeconfig.WriteResult{}, &ValidationError{Key: "fallback_providers", Err: err}
		}
	}
	if delegation, ok := values["delegation"].(map[string]any); ok {
		if fallback, present := delegation["fallback_providers"]; present {
			if err := validateFallbackRoutes(fallback); err != nil {
				return runtimeconfig.WriteResult{}, &ValidationError{Key: "delegation.fallback_providers", Err: err}
			}
		}
	}
	leaves := collectLeaves("", values)
	if len(leaves) == 0 {
		return runtimeconfig.WriteResult{}, &NoWritableKeysError{}
	}
	var unknown []string
	var validation *ValidationError
	for _, lf := range leaves {
		if _, ok := FieldFor(m.surface, lf.key); !ok {
			unknown = append(unknown, lf.key)
			continue
		}
		if err := validateValue(m.surface, lf.key, lf.value); err != nil && validation == nil {
			validation = &ValidationError{Key: lf.key, Err: err}
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return runtimeconfig.WriteResult{}, &UnknownFieldsError{Fields: unknown}
	}
	if validation != nil {
		return runtimeconfig.WriteResult{}, validation
	}

	newDoc := deepMerge(doc, values)
	writtenKeys := collectLeafKeys("", values)
	// A patch touching the subagent-model selection re-projects the
	// delegation keys through ProjectDelegation so the written document
	// carries exactly the projection's keys — unset fields are omitted,
	// the only form that preserves Hermes' inheritance semantics.
	if delegationTouched(writtenKeys) {
		set := applyDelegationProjection(newDoc)
		for _, k := range set {
			if !contains(writtenKeys, k) {
				writtenKeys = append(writtenKeys, k)
			}
		}
		sort.Strings(writtenKeys)
	}

	if m.validateChange != nil {
		if err := m.validateChange(ctx, doc, newDoc); err != nil {
			return runtimeconfig.WriteResult{}, err
		}
	}

	newContent, err := yaml.Marshal(newDoc)
	if err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("encode config: %w", err)
	}

	snapKey := snapshotKey(worker, curRev)
	if err := m.store.PutObject(ctx, snapKey, raw); err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("store pre-write snapshot: %w", err)
	}
	if err := m.atomicReplace(ctx, worker, path, string(newContent)); err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("write config.yaml into worker: %w", err)
	}
	// Fail-closed verify: the on-disk bytes must be exactly what was meant
	// to be written, or the write reports pending with the actual rev.
	back, err := m.readFile(ctx, worker, path)
	if err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("read-back after write: %w", err)
	}
	applied := runtimeconfig.StatePending
	newRev := revOf(back)
	if newRev == revOf(newContent) {
		applied = runtimeconfig.StateApplied
	}

	restartKeys := RestartKeys(m.surface, writtenKeys)
	if restartKeys == nil {
		restartKeys = []string{}
	}
	desired := DesiredState{
		Rev:         newRev,
		TargetRev:   revOf(newContent),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
		WrittenKeys: writtenKeys,
		RestartKeys: restartKeys,
		Content:     newDoc,
	}
	encoded, err := json.Marshal(desired)
	if err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("encode desired state: %w", err)
	}
	if err := m.store.PutObject(ctx, desiredKey(worker), encoded); err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("persist desired state (file already written): %w", err)
	}
	return runtimeconfig.WriteResult{
		Runtime:        m.Runtime(),
		Rev:            newRev,
		Applied:        applied,
		PendingRestart: restartKeys,
		Snapshot:       snapKey,
	}, nil
}

// Rollback restores the pre-write snapshot taken when the state identified
// by rev was current, taking a fresh snapshot of the state it replaces.
func (m *ConfigManager) Rollback(ctx context.Context, worker, rev string) (runtimeconfig.WriteResult, error) {
	unlock := lockConfigMutation(worker)
	defer unlock()

	if !revPattern.MatchString(rev) {
		return runtimeconfig.WriteResult{}, fmt.Errorf("rev must be a 64-char sha256 hex string, got %q", rev)
	}
	snap, err := m.store.GetObject(ctx, snapshotKey(worker, rev))
	if err != nil {
		if isNotFound(err) {
			return runtimeconfig.WriteResult{}, fmt.Errorf("no pre-write snapshot for rev %s", rev)
		}
		return runtimeconfig.WriteResult{}, fmt.Errorf("read snapshot: %w", err)
	}
	// The snapshot holds the exact pre-write bytes (possibly empty when
	// the config file did not exist yet); parse it only for the written-
	// key accounting, and a corrupt snapshot is fail-closed.
	var doc map[string]any
	if len(snap) > 0 {
		doc, err = parseYamlDoc(snap)
		if err != nil {
			return runtimeconfig.WriteResult{}, fmt.Errorf("snapshot for rev %s is not a valid YAML mapping: %w", rev, err)
		}
	}

	path, err := m.resolveConfigPath(ctx, worker)
	if err != nil {
		return runtimeconfig.WriteResult{}, err
	}
	var curRaw []byte
	if m.fileExists(ctx, worker, path) {
		curRaw, err = m.readFile(ctx, worker, path)
		if err != nil {
			return runtimeconfig.WriteResult{}, err
		}
	}
	if m.validateChange != nil {
		current := map[string]any{}
		if len(curRaw) > 0 {
			// An invalid current file remains recoverable by a policy that
			// permits recovery; policies requiring current values fail closed.
			current, _ = parseYamlDoc(curRaw)
		}
		if err := m.validateChange(ctx, current, doc); err != nil {
			return runtimeconfig.WriteResult{}, err
		}
	}
	curRev := revOf(curRaw)
	// A rollback to the current state is a no-op (still reported, but no
	// snapshot churn).
	if curRev == rev {
		st, err := m.read(ctx, worker)
		if err != nil {
			return runtimeconfig.WriteResult{}, err
		}
		return runtimeconfig.WriteResult{
			Runtime:        st.Runtime,
			Rev:            st.Rev,
			Applied:        st.Applied,
			PendingRestart: st.PendingRestart,
			Snapshot:       snapshotKey(worker, curRev),
		}, nil
	}

	snapKey := snapshotKey(worker, curRev)
	if err := m.store.PutObject(ctx, snapKey, curRaw); err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("store pre-write snapshot: %w", err)
	}
	if err := m.atomicReplace(ctx, worker, path, string(snap)); err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("write snapshot into worker: %w", err)
	}
	back, err := m.readFile(ctx, worker, path)
	if err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("read-back after rollback: %w", err)
	}
	applied := runtimeconfig.StatePending
	newRev := revOf(back)
	if newRev == rev {
		applied = runtimeconfig.StateApplied
	}

	writtenKeys := []string{}
	if doc != nil {
		writtenKeys = collectLeafKeys("", doc)
	}
	restartKeys := RestartKeys(m.surface, writtenKeys)
	if restartKeys == nil {
		restartKeys = []string{}
	}
	desired := DesiredState{
		Rev:         newRev,
		TargetRev:   rev,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
		WrittenKeys: writtenKeys,
		RestartKeys: restartKeys,
		Content:     doc,
	}
	encoded, err := json.Marshal(desired)
	if err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("encode desired state: %w", err)
	}
	if err := m.store.PutObject(ctx, desiredKey(worker), encoded); err != nil {
		return runtimeconfig.WriteResult{}, fmt.Errorf("persist desired state (file already written): %w", err)
	}
	return runtimeconfig.WriteResult{
		Runtime:        m.Runtime(),
		Rev:            newRev,
		Applied:        applied,
		PendingRestart: restartKeys,
		Snapshot:       snapKey,
	}, nil
}

// ---- container file operations (absolute command paths, no PATH) ----

// resolveConfigPath finds the worker's Hermes config: $HERMES_HOME (the
// entrypoint exports HERMES_HOME=${WORKSPACE}/.hermes; the default is
// $HOME/.hermes) + /config.yaml. A file that does not exist yet is a
// valid state — the runtime starts from built-in defaults — so unlike
// openclaw there is no missing-file error here.
func (m *ConfigManager) resolveConfigPath(ctx context.Context, worker string) (string, error) {
	root, err := m.shellPrintf(ctx, worker, `printf %s "${HERMES_HOME:-$HOME/.hermes}"`)
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", fmt.Errorf("worker %s: HERMES_HOME and HOME are both unset", worker)
	}
	return filepath.Join(root, "config.yaml"), nil
}

func (m *ConfigManager) shellPrintf(ctx context.Context, worker, script string) (string, error) {
	out, stderr, exit, err := m.exec.Exec(ctx, worker, []string{"/bin/sh", "-c", script}, execTimeout)
	if err != nil {
		return "", fmt.Errorf("exec %s: %w", script, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("exec %s exit %d: %s", script, exit, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(out), nil
}

func (m *ConfigManager) fileExists(ctx context.Context, worker, path string) bool {
	_, stderr, exit, err := m.exec.Exec(ctx, worker, []string{
		"/bin/sh", "-c", `test -f "$1"`, "/bin/sh", path,
	}, execTimeout)
	if err != nil || exit != 0 {
		return false
	}
	_ = stderr
	return true
}

func (m *ConfigManager) readFile(ctx context.Context, worker, path string) ([]byte, error) {
	out, stderr, exit, err := m.exec.Exec(ctx, worker, []string{
		"/bin/sh", "-c", `cat -- "$1"`, "/bin/sh", path,
	}, execTimeout)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("read %s exit %d: %s", path, exit, strings.TrimSpace(stderr))
	}
	return []byte(out), nil
}

// atomicReplace replaces path with content via temp file + rename,
// preserving the existing file's permissions (a first-time file is 0600).
// The temp file is created through the backend's WriteFile (which prepares
// the parent directory); the activate step is one fixed shell script with
// the paths as arguments — never string-interpolated.
func (m *ConfigManager) atomicReplace(ctx context.Context, worker, path, content string) error {
	tmp := path + ".next"
	if err := m.exec.WriteFile(ctx, worker, tmp, content); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	_, stderr, exit, err := m.exec.Exec(ctx, worker, []string{
		"/bin/sh", "-c",
		`if [ -f "$2" ]; then chmod --reference="$2" "$1" || chmod 600 "$1"; else chmod 600 "$1"; fi; mv -f "$1" "$2"`,
		"/bin/sh", tmp, path,
	}, execTimeout)
	if err != nil {
		return fmt.Errorf("activate %s: %w", path, err)
	}
	if exit != 0 {
		return fmt.Errorf("activate %s exit %d: %s", path, exit, strings.TrimSpace(stderr))
	}
	return nil
}

// ---- projection, validation, document plumbing ----

// projectValues projects the on-disk document onto the declared surface:
// only keys the surface declares (exact key or wildcard pattern for the
// dynamic maps) appear in Values — unknown/foreign keys stay in the file
// but never in the GET projection. A declared sensitive value is replaced
// with the fixed placeholder; declared containers retain their contents with sensitive descendants masked.
func (m *ConfigManager) projectValues(doc map[string]any) map[string]any {
	out, _ := m.projectNode("", doc)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func (m *ConfigManager) projectNode(prefix string, v any) (map[string]any, bool) {
	mapping, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	out := map[string]any{}
	for k, cv := range mapping {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		f, declared := FieldFor(m.surface, key)
		switch {
		case declared && f.Sensitive:
			out[k] = redactedValue
		case declared:
			out[k] = m.redactDeclaredValue(key, cv)
		default:
			if sub, ok := m.projectNode(key, cv); ok {
				out[k] = sub
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// redactDeclaredValue preserves declared object values while masking any
// sensitive descendant; a declared parent must not bypass child redaction.
func (m *ConfigManager) redactDeclaredValue(prefix string, value any) any {
	switch node := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for key, child := range node {
			path := prefix + "." + key
			if field, ok := FieldFor(m.surface, path); ok && field.Sensitive {
				out[key] = redactedValue
			} else {
				out[key] = m.redactDeclaredValue(path, child)
			}
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, child := range node {
			out[i] = m.redactDeclaredValue(fmt.Sprintf("%s.%d", prefix, i), child)
		}
		return out
	default:
		return value
	}
}

// validateValue checks one concrete key/value pair against the declared
// hermes field (the surface's own type vocabulary:
// string|int|float|bool|enum|string-list|object).
func validateValue(s runtimeconfig.Surface, key string, value any) error {
	if key == "delegation.fallback_providers" || key == "fallback_providers" {
		return validateFallbackRoutes(value)
	}
	f, ok := FieldFor(s, key)
	if !ok {
		return fmt.Errorf("key %q is not declared in the hermes surface", key)
	}
	switch f.Type {
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("key %q must be a string", key)
		}
	case "int":
		n, ok := value.(float64)
		if !ok || n != math.Trunc(n) {
			return fmt.Errorf("key %q must be an integer", key)
		}
	case "float":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("key %q must be a number", key)
		}
	case "bool":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("key %q must be a boolean", key)
		}
	case "enum":
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("key %q must be a string", key)
		}
		for _, e := range f.Enum {
			if s == e {
				return nil
			}
		}
		// The declared default is a legal value even when it falls
		// outside the enum list (delegation.api_mode: "" = inherit).
		if d, ok := f.Default.(string); ok && s == d {
			return nil
		}
		return fmt.Errorf("key %q value %q must be one of %v", key, s, f.Enum)
	case "string-list":
		if value == nil {
			return nil // null = inherit the parent chain (clears the key)
		}
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("key %q must be a list of strings", key)
		}
		for _, it := range items {
			if _, ok := it.(string); !ok {
				return fmt.Errorf("key %q must be a list of strings", key)
			}
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("key %q must be an object", key)
		}
	default:
		return fmt.Errorf("key %q has undeclared type %q", key, f.Type)
	}
	return nil
}

// validateFallbackRoutes accepts Hermes' native provider/model route list.
// null means no override; an explicit empty list disables fallback.
func validateFallbackRoutes(value any) error {
	if value == nil {
		return nil
	}
	entries, ok := value.([]any)
	if !ok {
		return fmt.Errorf("fallback_providers must be a list of provider/model routes")
	}
	for _, entry := range entries {
		route, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("fallback_providers entries must be provider/model objects")
		}
		for _, key := range []string{"provider", "model"} {
			name, ok := route[key].(string)
			if !ok || strings.TrimSpace(name) == "" {
				return fmt.Errorf("fallback_providers route requires %s", key)
			}
		}
	}
	return nil
}

// parseYamlDoc parses a config.yaml document into a plain nested
// map[string]any form (empty/blank content = empty mapping).
func parseYamlDoc(raw []byte) (map[string]any, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return map[string]any{}, nil
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	m, ok := normalizeDoc(doc).(map[string]any)
	if !ok {
		return nil, errors.New("not a YAML mapping")
	}
	return m, nil
}

// normalizeDoc converts nested mappings into map[string]any form.
// yaml.v3 already yields that for all-string-key mappings; the walk
// guards against the v2-style map[interface{}]interface{} a different
// parser might yield (non-string keys are dropped — a hermes config.yaml
// never has any, and a document that does is not writable here).
func normalizeDoc(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, cv := range t {
			out[k] = normalizeDoc(cv)
		}
		return out
	case map[interface{}]interface{}:
		out := map[string]any{}
		for k, cv := range t {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			out[ks] = normalizeDoc(cv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, cv := range t {
			out[i] = normalizeDoc(cv)
		}
		return out
	default:
		return v
	}
}

// deepMerge merges incoming over current (objects merge recursively;
// arrays and scalars replace) and returns a new map — never mutating
// either input.
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

type leaf struct {
	key   string
	value any
}

// collectLeaves flattens a (possibly nested) patch into its concrete
// key/value leaves, key-sorted.
func collectLeaves(prefix string, doc map[string]any) []leaf {
	var out []leaf
	for k, v := range doc {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			out = append(out, collectLeaves(key, sub)...)
			continue
		}
		out = append(out, leaf{key: key, value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func collectLeafKeys(prefix string, doc map[string]any) []string {
	var out []string
	for k, v := range doc {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			out = append(out, collectLeafKeys(key, sub)...)
			continue
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func contains(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// ---- delegation projection on write ----

// delegationKeys are exactly the keys ProjectDelegation may write — the
// subagent-model selection as it appears in the native config.yaml.
var delegationKeys = []string{
	"delegation.model",
	"delegation.provider",
	"delegation.fallback_providers",
}

func delegationTouched(keys []string) bool {
	for _, k := range keys {
		for _, d := range delegationKeys {
			if k == d {
				return true
			}
		}
	}
	return false
}

// applyDelegationProjection rewrites the document's delegation model
// selection to exactly ProjectDelegation's output: keys the projection
// omits are deleted (omission is the only form that preserves Hermes'
// inheritance semantics), and it returns the keys the projection set.
func applyDelegationProjection(doc map[string]any) []string {
	del, ok := doc["delegation"].(map[string]any)
	if !ok {
		del = map[string]any{}
		doc["delegation"] = del
	}
	m := SubagentModel{
		Model:    asString(del["model"]),
		Provider: asString(del["provider"]),
		Fallback: asFallbackRoutes(del["fallback_providers"]),
	}
	proj := ProjectDelegation(m)
	set := make([]string, 0, len(proj))
	for _, full := range delegationKeys {
		short := strings.TrimPrefix(full, "delegation.")
		if v, ok := proj[full]; ok {
			del[short] = v
			set = append(set, full)
		} else {
			delete(del, short)
		}
	}
	return set
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asFallbackRoutes(value any) []map[string]any {
	entries, ok := value.([]any)
	if !ok {
		return nil
	}
	routes := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		route, ok := entry.(map[string]any)
		if !ok {
			return nil
		}
		routes = append(routes, route)
	}
	return routes
}

// ---- desired state ----

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
		return nil, fmt.Errorf("desired state for worker %s is corrupt: %w", worker, err)
	}
	return &d, nil
}
