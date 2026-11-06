package runtimeconfig

import "sort"

// Adapter is implemented once per runtime; it never translates fields.
type Adapter interface {
	Surface() Surface
}

// Registry holds the adapters of the runtimes compiled into this binary.
type Registry struct {
	byRuntime map[string]Adapter
}

func NewRegistry() *Registry { return &Registry{byRuntime: map[string]Adapter{}} }

// Register adds an adapter. Registering the same runtime twice panics: it can
// only happen through a wiring mistake, and silently keeping one of the two
// would make the console show a surface that is not the one being written.
func (r *Registry) Register(a Adapter) {
	id := a.Surface().Runtime
	if id == "" {
		panic("runtimeconfig: adapter with empty runtime id")
	}
	if _, dup := r.byRuntime[id]; dup {
		panic("runtimeconfig: duplicate adapter for runtime " + id)
	}
	r.byRuntime[id] = a
}

// Lookup returns the adapter for a runtime and whether one exists. Callers must
// treat a missing adapter as "unknown runtime" and never fall back to another
// runtime: writing a config to the wrong runtime is worse than refusing.
func (r *Registry) Lookup(runtime string) (Adapter, bool) {
	a, ok := r.byRuntime[runtime]
	return a, ok
}

// Runtimes lists the registered runtime ids in stable order.
func (r *Registry) Runtimes() []string {
	out := make([]string, 0, len(r.byRuntime))
	for id := range r.byRuntime {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
