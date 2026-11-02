package hermes

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
)

// Adapter implements runtimeconfig.Adapter for Hermes.
//
// Reconciliation note: unlike OpenClaw, Hermes records no
// appliedConfigHash in its config.yaml, so the controller cannot ask the
// running process what it has picked up. A write is reconciled by reading
// the file back and comparing it to what was written, plus by observing
// the next turn's behaviour (next-turn keys only surface at the next
// turn; restart keys only after a restart). The UI must label this weak
// reconciliation: a matching file is not proof the process has applied
// the change.
type Adapter struct {
	surface runtimeconfig.Surface
}

// New builds the adapter from the embedded surface. A load failure is a
// build defect (the embedded surface.json is version-pinned), so it
// panics.
func New() *Adapter {
	s, err := Surface()
	if err != nil {
		panic(err)
	}
	return &Adapter{surface: s}
}

// Surface implements runtimeconfig.Adapter.
func (a *Adapter) Surface() runtimeconfig.Surface { return a.surface }

// FieldForKey resolves a concrete config key against the declared
// fields: exact declared keys win over wildcard patterns (the package's
// FieldFor carries the wildcard convention).
func (a *Adapter) FieldForKey(key string) (runtimeconfig.Field, bool) {
	return FieldFor(a.surface, key)
}

// ConfigRev returns the revision fingerprint of a Hermes config.yaml:
// "<mtime_ns>:<size>". The controller keeps the rev it wrote and compares
// it on read-back; a different rev means the file changed on disk.
func ConfigRev(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("hermes config rev: %s: no such file", path)
		}
		return "", fmt.Errorf("hermes config rev: %s: %w", path, err)
	}
	return strconv.FormatInt(info.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(info.Size(), 10), nil
}
