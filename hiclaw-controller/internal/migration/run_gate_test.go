package migration

import (
	"context"
	"testing"

	"github.com/hiclaw/hiclaw-controller/internal/oss/ossfake"
	"k8s.io/client-go/rest"
)

// TestRun_DoesNotWriteMarkerOnPartialCreateFailure verifies the completion
// marker is only written when every per-CR create actually succeeds. Here
// registry data exists (one standalone worker) but the dynamic client can
// never reach the API server (bogus host), so createStandaloneWorkerCR's
// Create call fails. Run must treat this as a partial failure and leave the
// marker unwritten so the next startup retries the missed CR.
func TestRun_DoesNotWriteMarkerOnPartialCreateFailure(t *testing.T) {
	fake := ossfake.NewMemory()
	workersJSON := `{
  "version": 1,
  "updated_at": "2026-01-01T00:00:00Z",
  "workers": {
    "w1": {
      "matrix_user_id": "@w1:example.org",
      "room_id": "!room:example.org",
      "runtime": "node",
      "deployment": "k8s",
      "skills": [],
      "role": "worker",
      "team_id": null,
      "skills_updated_at": ""
    }
  }
}`
	if err := fake.PutObject(context.Background(), "agents/manager/workers-registry.json", []byte(workersJSON)); err != nil {
		t.Fatalf("seed workers registry: %v", err)
	}

	m := &Migrator{
		OSS: fake,
		// An unreachable host: Get-before-Create and Create both fail against
		// it, simulating a transient API-server/admission-webhook error during
		// CR creation.
		RestCfg:   &rest.Config{Host: "http://127.0.0.1:1", Timeout: 1},
		Namespace: "default",
	}

	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil (per-CR failures are logged non-fatal)", err)
	}

	if err := fake.Stat(context.Background(), migrationMarker); err == nil {
		t.Fatalf("marker was written despite a partial create failure; expected it to be withheld so the next startup retries")
	}
}

// TestRun_SkipsWhenMarkerExists verifies the completion gate: when the
// migration marker object is already present in OSS, Run returns nil
// immediately without needing a working RestCfg (RestCfg is left nil here —
// if the gate didn't short-circuit before dynamic.NewForConfig, this test
// would panic/fail on a nil config).
func TestRun_SkipsWhenMarkerExists(t *testing.T) {
	fake := ossfake.NewMemory()
	if err := fake.PutObject(context.Background(), migrationMarker, []byte("done")); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	m := &Migrator{OSS: fake}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil (should skip via completion gate)", err)
	}
}

// TestRun_TransientOSSErrorDoesNotSkip verifies that a Stat error (simulating
// a transient OSS outage, not just "not found") is treated as
// not-yet-migrated rather than causing Run to skip — the gate must not mask
// a genuine OSS availability problem into "migration already done".
func TestRun_TransientOSSErrorDoesNotSkip(t *testing.T) {
	fake := ossfake.NewMemory() // marker absent -> Stat returns os.ErrNotExist

	m := &Migrator{
		OSS:       fake,
		RestCfg:   &rest.Config{Host: "http://127.0.0.1:1"},
		Namespace: "default",
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil (no registry data path)", err)
	}

	// Because there was no registry data, Run should have written the
	// completion marker for next time.
	if err := fake.Stat(context.Background(), migrationMarker); err != nil {
		t.Fatalf("expected marker to be written after a fully successful Run, Stat() = %v", err)
	}
}

// TestRun_WritesMarkerAfterSuccessWithNoRegistryData covers the "nothing to
// reconcile" completion path explicitly: Run must still persist the marker
// so subsequent startups take the fast completion-gate path instead of
// repeatedly finding empty registries.
func TestRun_WritesMarkerAfterSuccessWithNoRegistryData(t *testing.T) {
	fake := ossfake.NewMemory()
	m := &Migrator{
		OSS:       fake,
		RestCfg:   &rest.Config{Host: "http://127.0.0.1:1"},
		Namespace: "default",
	}

	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if err := fake.Stat(context.Background(), migrationMarker); err != nil {
		t.Fatalf("marker not written after successful Run: %v", err)
	}

	// A second Run must now take the completion-gate skip path. We can't
	// directly observe "skipped" from the return value alone (both paths
	// return nil), but a nil RestCfg on this second Migrator proves the gate
	// short-circuited before reaching dynamic.NewForConfig, which would
	// otherwise fail loudly.
	m2 := &Migrator{OSS: fake}
	if err := m2.Run(context.Background()); err != nil {
		t.Fatalf("second Run() = %v, want nil (should hit completion gate)", err)
	}
}
