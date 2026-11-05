package openclaw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

const (
	testConfigPath    = "/workspace/openclaw.json"
	testConfigInitial = `{"agents":{"defaults":{"model":{"primary":"agentteams-gateway/qwen3-max"}}}}`
	pluginsDiscordUp  = `{"plugins":[{"id":"discord","enabled":true,"status":"loaded","channelIds":["discord"]}]}`
	pluginsDiscordOff = `{"plugins":[{"id":"discord","enabled":false,"status":"loaded","channelIds":["discord"]}]}`
)

// fakeExec is a virtual worker container: a virtual filesystem, a plugins
// list, and a gateway appliedConfigHash that — with gwApplyOnWrite — moves
// to the sha256 of freshly written bytes, exactly like the real gateway's
// file watch applying a changed openclaw.json.
type fakeExec struct {
	fs             map[string]string
	plugins        string
	gwHash         *string
	gwErr          error
	gwApplyOnWrite bool
	// env is the worker's environment for printenv: a hit returns the
	// value (exit 0), a miss stays "not defined" (exit 1).
	env map[string]string
}

func (b *fakeExec) Exec(_ context.Context, _ string, command []string, _ time.Duration) (string, string, int, error) {
	switch {
	case len(command) == 3 && command[0] == "sh" && command[1] == "-c" && strings.Contains(command[2], "QWENPAW_WORKING_DIR"):
		return "/workspace", "", 0, nil
	case len(command) == 3 && command[0] == "sh" && command[1] == "-c" && strings.Contains(command[2], "${HOME}"):
		return "/home/worker", "", 0, nil
	case len(command) == 5 && command[0] == "sh" && command[1] == "-c" && command[2] == `test -f "$1"`:
		if _, ok := b.fs[command[4]]; ok {
			return "", "", 0, nil
		}
		return "", "no such file", 1, nil
	case len(command) == 5 && command[0] == "sh" && command[1] == "-c" && strings.HasPrefix(command[2], "cat"):
		c, ok := b.fs[command[4]]
		if !ok {
			return "", "cat: no such file", 1, nil
		}
		return c, "", 0, nil
	case len(command) >= 4 && command[0] == "openclaw" && command[1] == "plugins":
		return b.plugins, "", 0, nil
	case len(command) >= 4 && command[0] == "openclaw" && command[1] == "gateway":
		if b.gwErr != nil {
			return "", "gateway unavailable: " + b.gwErr.Error(), 1, nil
		}
		h := "null"
		if b.gwHash != nil {
			h = `"` + *b.gwHash + `"`
		}
		return `{"appliedConfigHash":` + h + `}`, "", 0, nil
	case len(command) == 2 && command[0] == "printenv":
		if v, ok := b.env[command[1]]; ok {
			return v, "", 0, nil
		}
		return "", "not defined", 1, nil
	}
	return "", "unhandled: " + strings.Join(command, " "), 127, nil
}

func (b *fakeExec) WriteFile(_ context.Context, _, path, content string) error {
	b.fs[path] = content
	if b.gwApplyOnWrite {
		sum := sha256.Sum256([]byte(content))
		h := hex.EncodeToString(sum[:])
		b.gwHash = &h
	}
	return nil
}

func newFakeExec(plugins string) *fakeExec {
	b := &fakeExec{
		fs:      map[string]string{testConfigPath: testConfigInitial},
		plugins: plugins,
	}
	h := "applied-baseline"
	b.gwHash = &h
	b.gwApplyOnWrite = true
	return b
}

// newTestManager builds a manager with millisecond reconcile timing so
// the tests never wait on the production 10s window.
func newTestManager(t *testing.T, exec *fakeExec) *ConfigManager {
	t.Helper()
	return NewConfigManager(New(), exec, ossfake.NewMemory(),
		WithReconcileWindow(50*time.Millisecond),
		WithReconcileInterval(5*time.Millisecond),
		WithReconcileGrace(30*time.Second),
	)
}

func subagentModelPatch() map[string]any {
	return map[string]any{
		"agents": map[string]any{
			"defaults": map[string]any{
				"subagents": map[string]any{"model": "agentteams-gateway/qwen3.5-flash"},
			},
		},
	}
}

func TestWrite_AppliedWhenGatewayReloads(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", subagentModelPatch(), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Applied != StateApplied {
		t.Fatalf("applied=%q, want applied", res.Applied)
	}
	if !ValidRev(res.Rev) {
		t.Fatalf("rev=%q, want a well-formed rev", res.Rev)
	}
}

func TestWrite_PendingWhenGatewayNeverReloads(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	exec.gwApplyOnWrite = false
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", subagentModelPatch(), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Applied != StatePending {
		t.Fatalf("applied=%q, want pending (the gateway hash never moved)", res.Applied)
	}
}

func TestWrite_PendingWhenBaselineUnknowable(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	exec.gwErr = errors.New("gateway not running")
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", subagentModelPatch(), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Applied != StatePending {
		t.Fatalf("applied=%q, want pending (baseline capture failed)", res.Applied)
	}
	// The file plane is independent of the gateway: the write landed.
	if exec.fs[testConfigPath] == testConfigInitial {
		t.Fatalf("file not written despite the 200-class result")
	}
}

func TestWrite_AppliedButInactive(t *testing.T) {
	exec := newFakeExec(pluginsDiscordOff)
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", map[string]any{
		"channels": map[string]any{"discord": map[string]any{"enabled": true}},
	}, "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Applied != StateAppliedButInactive {
		t.Fatalf("applied=%q, want applied-but-inactive", res.Applied)
	}
	if len(res.Inactive) != 1 || res.Inactive[0] != "discord" {
		t.Fatalf("inactive=%v, want [discord]", res.Inactive)
	}
}

func TestWrite_RestartKeysInPendingRestart(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", map[string]any{
		"gateway": map[string]any{"port": float64(19000)},
	}, "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.PendingRestart) != 1 || res.PendingRestart[0] != "gateway.port" {
		t.Fatalf("pendingRestart=%v, want [gateway.port]", res.PendingRestart)
	}
}

func TestRead_NoDesiredState(t *testing.T) {
	m := newTestManager(t, newFakeExec(pluginsDiscordUp))
	st, err := m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StateApplied {
		t.Fatalf("applied=%q, want applied with no desired state", st.Applied)
	}
	if len(st.PendingRestart) != 0 {
		t.Fatalf("pendingRestart=%v, want empty", st.PendingRestart)
	}
}

// TestRead_PendingThenUnappliedThenApplied walks the reconcile state
// machine of a write the gateway has not applied yet: fresh write with an
// unchanged hash -> pending; aged beyond the grace window -> unapplied;
// the gateway finally applies the bytes -> applied.
func TestRead_PendingThenUnappliedThenApplied(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	exec.gwApplyOnWrite = false
	m := newTestManager(t, exec)
	if _, err := m.Write(context.Background(), "w1", subagentModelPatch(), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Fresh desired state: unchanged hash within the grace -> pending.
	st, err := m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StatePending {
		t.Fatalf("applied=%q, want pending (fresh write, hash unchanged)", st.Applied)
	}

	// Age the desired state beyond the grace window -> unapplied.
	raw, err := m.store.GetObject(context.Background(), desiredKey("w1"))
	if err != nil {
		t.Fatalf("read desired: %v", err)
	}
	var d DesiredState
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("unmarshal desired: %v", err)
	}
	d.UpdatedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	aged, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal desired: %v", err)
	}
	if err := m.store.PutObject(context.Background(), desiredKey("w1"), aged); err != nil {
		t.Fatalf("store aged desired: %v", err)
	}
	st, err = m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StateUnapplied {
		t.Fatalf("applied=%q, want unapplied (aged, hash unchanged)", st.Applied)
	}

	// The gateway finally applies the written bytes -> applied.
	sum := sha256.Sum256([]byte(exec.fs[testConfigPath]))
	h := hex.EncodeToString(sum[:])
	exec.gwHash = &h
	st, err = m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StateApplied {
		t.Fatalf("applied=%q, want applied (hash moved)", st.Applied)
	}
}

// TestRead_LegacyDesiredWithoutBaselineIsPending: a desired.json written
// by a pre-reconcile controller (no baseline fields) cannot be verified —
// it must stay pending, never applied (backward compatibility).
func TestRead_LegacyDesiredWithoutBaselineIsPending(t *testing.T) {
	m := newTestManager(t, newFakeExec(pluginsDiscordUp))
	legacy := `{"rev":"` + revOf([]byte(testConfigInitial)) + `","updatedAt":"` +
		time.Now().UTC().Format(time.RFC3339) + `","writtenKeys":["agents.defaults.model.primary"],"content":{}}`
	if err := m.store.PutObject(context.Background(), desiredKey("w1"), []byte(legacy)); err != nil {
		t.Fatalf("store legacy desired: %v", err)
	}
	st, err := m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StatePending {
		t.Fatalf("applied=%q, want pending (no baseline in legacy desired state)", st.Applied)
	}
}

// TestRead_CorruptDesiredTimestampIsError: an unparseable updatedAt is
// corrupt state — fail closed with an error rather than guessing
// freshness.
func TestRead_CorruptDesiredTimestampIsError(t *testing.T) {
	m := newTestManager(t, newFakeExec(pluginsDiscordUp))
	legacy := `{"rev":"` + revOf([]byte(testConfigInitial)) +
		`","baselineHash":"applied-baseline","baselineKnown":true,"updatedAt":"not-a-timestamp","writtenKeys":[],"content":{}}`
	if err := m.store.PutObject(context.Background(), desiredKey("w1"), []byte(legacy)); err != nil {
		t.Fatalf("store desired: %v", err)
	}
	if _, err := m.Read(context.Background(), "w1"); err == nil {
		t.Fatalf("Read must fail closed on a corrupt desired-state timestamp")
	}
}

// TestRead_GatewayDownIsError: with a known baseline, an unreachable
// gateway during a GET is an error (502-class at the HTTP layer), not a
// verdict — consistent with the fail-closed plugin check.
func TestRead_GatewayDownIsError(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	m := newTestManager(t, exec)
	if _, err := m.Write(context.Background(), "w1", subagentModelPatch(), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	exec.gwErr = errors.New("gateway not running")
	if _, err := m.Read(context.Background(), "w1"); err == nil {
		t.Fatalf("Read must fail when the gateway is unreachable with a known baseline")
	}
}

// --- declared secret refs stay atomic across the write walkers (#1493) ---

// secretRefValue is the canonical resolvable env ref used by the
// secret-atomicity tests.
func secretRefValue() map[string]any {
	return map[string]any{"source": "env", "provider": "default", "id": "TRACKER_TEST_TOKEN"}
}

// payloadFor wraps a dotted key as a nested partial document
// ("gateway.auth.token" -> {"gateway":{"auth":{"token":v}}}).
func payloadFor(key string, v any) map[string]any {
	segs := strings.Split(key, ".")
	var doc any = v
	for i := len(segs) - 1; i >= 0; i-- {
		doc = map[string]any{segs[i]: doc}
	}
	return doc.(map[string]any)
}

// docAt walks a nested document along a dotted key.
func docAt(doc map[string]any, key string) (any, bool) {
	cur := any(doc)
	for _, seg := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// secretRefEquals reports whether got equals the want ref object field by
// field (both must be maps of the same size and content).
func secretRefEquals(got, want any) bool {
	gm, ok := got.(map[string]any)
	if !ok {
		return false
	}
	wm, ok := want.(map[string]any)
	if !ok || len(gm) != len(wm) {
		return false
	}
	for k, v := range wm {
		if gm[k] != v {
			return false
		}
	}
	return true
}

// secretRefKeys is the set of declared secret fields the #1493 fix must
// keep atomic (surface.json, all type: secret + sensitive).
var secretRefKeys = []string{
	"gateway.auth.token",
	"models.providers.example.apiKey",
	"hooks.token",
	"skills.entries.demo.apiKey",
}

// TestWrite_SecretRefEnvResolves: a declared secret field's legal value is
// a SecretRef object; the write walkers must keep it whole. Before the
// fix, collectLeaves decomposed it into .source/.provider/.id, which
// ResolveKey reported as unknown fields (422) before ValidateSecretRef
// ever ran.
func TestWrite_SecretRefEnvResolves(t *testing.T) {
	ref := secretRefValue()
	for _, key := range secretRefKeys {
		t.Run(key, func(t *testing.T) {
			exec := newFakeExec(pluginsDiscordUp)
			exec.env = map[string]string{"TRACKER_TEST_TOKEN": "s3cret"}
			m := newTestManager(t, exec)

			res, err := m.Write(context.Background(), "w1", payloadFor(key, ref), "")
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if len(res.Unsupported) != 0 {
				t.Fatalf("unsupported=%v, want none", res.Unsupported)
			}
			for _, k := range res.PendingRestart {
				if strings.HasSuffix(k, ".source") || strings.HasSuffix(k, ".provider") || strings.HasSuffix(k, ".id") {
					t.Fatalf("pendingRestart contains decomposed ref fragment %q", k)
				}
			}
			// The ref lands on disk verbatim, at the full key.
			var doc map[string]any
			if err := json.Unmarshal([]byte(exec.fs[testConfigPath]), &doc); err != nil {
				t.Fatalf("parse written file: %v", err)
			}
			got, ok := docAt(doc, key)
			if !ok {
				t.Fatalf("key %q missing from the written file", key)
			}
			if !secretRefEquals(got, ref) {
				t.Fatalf("file value at %q = %v, want the ref verbatim", key, got)
			}
			// writtenKeys (read back from the desired state) keeps the
			// full key, not ref fragments.
			raw, err := m.store.GetObject(context.Background(), desiredKey("w1"))
			if err != nil {
				t.Fatalf("read desired state: %v", err)
			}
			var d DesiredState
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatalf("unmarshal desired state: %v", err)
			}
			if len(d.WrittenKeys) != 1 || d.WrittenKeys[0] != key {
				t.Fatalf("writtenKeys=%v, want [%s]", d.WrittenKeys, key)
			}
		})
	}
}

// TestWrite_SecretRefEnvUnresolvable: an env ref that does not resolve in
// the worker container makes the key unsupported — skipped and reported,
// never written, never downgraded to plaintext. With a regular key in the
// same payload the write still succeeds; with the secret as the only key
// it is a NoWritableKeysError.
func TestWrite_SecretRefEnvUnresolvable(t *testing.T) {
	ref := secretRefValue()
	for _, key := range secretRefKeys {
		t.Run(key, func(t *testing.T) {
			exec := newFakeExec(pluginsDiscordUp) // env left empty: unresolvable
			m := newTestManager(t, exec)

			// Mixed payload: the regular key is written, the secret ref
			// is skipped and reported.
			partial := deepMerge(
				payloadFor("gateway.publicOrigin", "https://gw.test"),
				payloadFor(key, ref),
			)
			res, err := m.Write(context.Background(), "w1", partial, "")
			if err != nil {
				t.Fatalf("Write (mixed): %v", err)
			}
			if len(res.Unsupported) != 1 || res.Unsupported[0].Key != key {
				t.Fatalf("unsupported=%v, want exactly one entry for %s", res.Unsupported, key)
			}
			if !strings.Contains(res.Unsupported[0].Reason, "plaintext fallback is forbidden") {
				t.Fatalf("reason=%q, want the no-plaintext-fallback explanation", res.Unsupported[0].Reason)
			}
			// No trace of the ref (or any fragment of it) on disk, while
			// the regular key landed.
			rawFile := exec.fs[testConfigPath]
			if strings.Contains(rawFile, "TRACKER_TEST_TOKEN") {
				t.Fatalf("written file still carries secret ref material")
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(rawFile), &doc); err != nil {
				t.Fatalf("parse written file: %v", err)
			}
			if _, ok := docAt(doc, key); ok {
				t.Fatalf("key %q written despite the unresolvable ref", key)
			}
			if v, ok := docAt(doc, "gateway.publicOrigin"); !ok || v != "https://gw.test" {
				t.Fatalf("regular key missing from the written file (doc=%v)", doc)
			}

			// Secret-only payload: no writable keys remain.
			exec2 := newFakeExec(pluginsDiscordUp)
			m2 := newTestManager(t, exec2)
			_, err = m2.Write(context.Background(), "w1", payloadFor(key, ref), "")
			var noKeys *NoWritableKeysError
			if !errors.As(err, &noKeys) {
				t.Fatalf("Write (secret-only) error = %v, want NoWritableKeysError", err)
			}
			if len(noKeys.Unsupported) != 1 || noKeys.Unsupported[0].Key != key {
				t.Fatalf("unsupported=%v, want exactly one entry for %s", noKeys.Unsupported, key)
			}
		})
	}
}

// TestWrite_SecretRefPlaintextRejected: a secret field never accepts a
// plaintext scalar — ValidationError, and the file is untouched.
func TestWrite_SecretRefPlaintextRejected(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	m := newTestManager(t, exec)
	_, err := m.Write(context.Background(), "w1", payloadFor("gateway.auth.token", "plaintext-token"), "")
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("Write error = %v, want ValidationError", err)
	}
	if vErr.Key != "gateway.auth.token" {
		t.Fatalf("error key = %q, want gateway.auth.token", vErr.Key)
	}
	if exec.fs[testConfigPath] != testConfigInitial {
		t.Fatalf("file changed despite the rejected plaintext write")
	}
}

// TestCollectLeaves_SecretRefStaysAtomic: the walker keeps a declared
// secret ref as one leaf at its full key and still decomposes ordinary
// nested objects.
func TestCollectLeaves_SecretRefStaysAtomic(t *testing.T) {
	a := New()
	ref := secretRefValue()
	doc := deepMerge(payloadFor("gateway.auth.token", ref), payloadFor("hooks.enabled", true))
	leaves := collectLeaves("", doc, a.isAtomicSecretLeaf)
	want := []string{"gateway.auth.token", "hooks.enabled"}
	if len(leaves) != len(want) {
		t.Fatalf("got %d leaves, want %d", len(leaves), len(want))
	}
	for i, l := range leaves {
		for _, suffix := range []string{".source", ".provider", ".id"} {
			if strings.HasSuffix(l.key, suffix) {
				t.Fatalf("leaf %q is a decomposed ref fragment", l.key)
			}
		}
		if l.key != want[i] {
			t.Fatalf("leaf[%d] = %q, want %q", i, l.key, want[i])
		}
	}
	if !secretRefEquals(leaves[0].value, ref) {
		t.Fatalf("secret leaf value = %v, want the ref verbatim", leaves[0].value)
	}
}

// TestCollectLeafKeys_SecretRefStaysAtomic: the key walker carries the same
// atomicity guarantee as collectLeaves.
func TestCollectLeafKeys_SecretRefStaysAtomic(t *testing.T) {
	a := New()
	ref := secretRefValue()
	doc := deepMerge(payloadFor("gateway.auth.token", ref), payloadFor("hooks.enabled", true))
	got := collectLeafKeys("", doc, a.isAtomicSecretLeaf)
	want := []string{"gateway.auth.token", "hooks.enabled"}
	if len(got) != len(want) {
		t.Fatalf("leaf keys = %v, want %v", got, want)
	}
	for i, k := range got {
		for _, suffix := range []string{".source", ".provider", ".id"} {
			if strings.HasSuffix(k, suffix) {
				t.Fatalf("leaf key %q is a decomposed ref fragment", k)
			}
		}
		if k != want[i] {
			t.Fatalf("leaf keys = %v, want %v", got, want)
		}
	}
}
