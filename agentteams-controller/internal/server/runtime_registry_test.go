package server

import (
	"reflect"
	"testing"
)

// TestNewRuntimeRegistry_RegistersEveryCompiledRuntime pins the production
// wiring: every runtime adapter compiled into this binary must be registered
// by newRuntimeRegistry, and nothing else. A runtime whose adapter package
// builds but is never registered 404s as an unknown runtime, which is
// exactly how the P8b wiring gap shipped (the registration line sat
// commented out while the handler-level tests stayed green, because they
// build their own registry).
func TestNewRuntimeRegistry_RegistersEveryCompiledRuntime(t *testing.T) {
	reg := newRuntimeRegistry()

	if _, ok := reg.Lookup("openclaw"); !ok {
		t.Fatalf("Lookup(openclaw) = not found; the openclaw adapter is not registered")
	}
	if _, ok := reg.Lookup("hermes"); !ok {
		t.Fatalf("Lookup(hermes) = not found; the hermes adapter is not registered")
	}
	if _, ok := reg.Lookup("unknown-runtime"); ok {
		t.Fatalf("Lookup(unknown-runtime) = found; the registry must hold only compiled-in runtimes")
	}

	// Runtimes() is the registry's stable (alphabetical) order, so the
	// exact expected list is [hermes openclaw].
	want := []string{"hermes", "openclaw"}
	if got := reg.Runtimes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Runtimes() = %v, want exactly %v", got, want)
	}
}
