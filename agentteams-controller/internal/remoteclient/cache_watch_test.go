package remoteclient

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// TestRegisterWatch_StoresConfig verifies that RegisterWatch appends each
// invocation to c.watches in order and preserves the supplied object,
// handler, and predicates.
func TestRegisterWatch_StoresConfig(t *testing.T) {
	c := newTestCache(t, &fakeCredClient{})

	if len(c.watches) != 0 {
		t.Fatalf("initial watches len = %d, want 0", len(c.watches))
	}

	worker := &v1beta1.Worker{}
	team := &v1beta1.Team{}
	h := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
		return nil
	})
	predA := predicate.NewPredicateFuncs(func(_ client.Object) bool { return true })
	predB := predicate.NewPredicateFuncs(func(_ client.Object) bool { return false })

	// Pass a nil controller.Controller: RegisterWatch only stores values
	// and never invokes them, so nil is acceptable here.
	c.RegisterWatch(nil, worker, h, predA)
	c.RegisterWatch(nil, team, h, predA, predB)

	if got, want := len(c.watches), 2; got != want {
		t.Fatalf("watches len = %d, want %d", got, want)
	}

	if c.watches[0].object != worker {
		t.Errorf("watches[0].object = %T, want *v1beta1.Worker", c.watches[0].object)
	}
	if c.watches[1].object != team {
		t.Errorf("watches[1].object = %T, want *v1beta1.Team", c.watches[1].object)
	}

	if len(c.watches[0].predicates) != 1 {
		t.Errorf("watches[0].predicates len = %d, want 1", len(c.watches[0].predicates))
	}
	if len(c.watches[1].predicates) != 2 {
		t.Errorf("watches[1].predicates len = %d, want 2", len(c.watches[1].predicates))
	}

	// Handler identity must be preserved across both registrations.
	if !reflect.DeepEqual(c.watches[0].handler, h) {
		t.Error("watches[0].handler not preserved")
	}
	if !reflect.DeepEqual(c.watches[1].handler, h) {
		t.Error("watches[1].handler not preserved")
	}
}

// TestStartRemoteCache_NoWatches_Noop verifies that startRemoteCache
// returns nil immediately when no watches have been registered, without
// attempting any connection against the supplied rest.Config.
func TestStartRemoteCache_NoWatches_Noop(t *testing.T) {
	c := newTestCache(t, &fakeCredClient{})

	// Use a bogus REST config: if startRemoteCache were not a true
	// no-op, building the cache against this config would fail or hang.
	cfg := &rest.Config{Host: "https://invalid.local.test:1"}

	if err := c.startRemoteCache(context.Background(), cfg, "test-cluster"); err != nil {
		t.Fatalf("startRemoteCache with no watches must be a no-op, got err: %v", err)
	}
}

// TestClusterEntry_HasRestConfigField guards the public ClusterEntry
// contract: callers (notably the remote cache wiring in buildEntry and
// future external consumers) rely on the RestConfig field being present
// and typed as *rest.Config.
func TestClusterEntry_HasRestConfigField(t *testing.T) {
	cfg := &rest.Config{Host: "https://example.test"}
	e := &ClusterEntry{RestConfig: cfg}

	if e.RestConfig != cfg {
		t.Fatal("ClusterEntry.RestConfig must round-trip the assigned *rest.Config")
	}

	// Reflect-level check ensures the field is exported and has the
	// expected static type, catching accidental renames or retyping.
	ft, ok := reflect.TypeOf(ClusterEntry{}).FieldByName("RestConfig")
	if !ok {
		t.Fatal("ClusterEntry has no exported RestConfig field")
	}
	if ft.Type != reflect.TypeOf((*rest.Config)(nil)) {
		t.Errorf("ClusterEntry.RestConfig type = %s, want *rest.Config", ft.Type)
	}
}
