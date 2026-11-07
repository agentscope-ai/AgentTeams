package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newLoopRunnerTestAPI builds a LoopRunnerAPI over the shared fake-client
// helper (newLoopTestHandler), with a deterministic injected clock
// (TH-15 single injected clock, frozen at start).
func newLoopRunnerTestAPI(t *testing.T, start time.Time, objects ...client.Object) *LoopRunnerAPI {
	t.Helper()
	h := newLoopTestHandler(t, objects...)
	a := NewLoopRunnerAPI(h.Client, "default")
	now := start
	a.now = func() time.Time { return now }
	return a
}

// loopRunnerRequest POSTs/GETs one loop-scoped runner endpoint directly
// (same style as loopRequest: handler-level, no mux).
func loopRunnerRequest(t *testing.T, method, id, sub, body string, caller *authpkg.CallerIdentity, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/loops/" + id + "/" + sub
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.SetPathValue("name", id)
	if caller != nil {
		req = withCaller(req, caller)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// internalRunnerRequest POSTs the internal handshake endpoint directly.
func internalRunnerRequest(t *testing.T, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/runner/handshake", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

const kickBody = `{"iteration_id":"it-001","target":{"kind":"task","ref":"p1-i001-01"},"origin":"test","deadline":"2026-11-03T10:05:00Z"}`

func TestKick_RunnerNotConfigured503(t *testing.T) {
	api := newLoopRunnerTestAPI(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC), testLoop("loop-1", "alpha-team", "p1-i001-01"))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	rec := loopRunnerRequest(t, http.MethodPost, "loop-1", "kick", kickBody, admin, api.Kick)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["code"] != "runner_not_configured" {
		t.Fatalf("code=%v want runner_not_configured", resp["code"])
	}
}

func TestKick_IdempotentDuplicate200(t *testing.T) {
	api := newLoopRunnerTestAPI(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC), testLoop("loop-1", "alpha-team", "p1-i001-01"))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	// First attempt: no runner configured -> 503, and the key is recorded.
	rec := loopRunnerRequest(t, http.MethodPost, "loop-1", "kick", kickBody, admin, api.Kick)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first kick status=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
	// Same key + same content -> duplicate, regardless of runner state.
	rec = loopRunnerRequest(t, http.MethodPost, "loop-1", "kick", kickBody, admin, api.Kick)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate kick status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["duplicate"] != true {
		t.Fatalf("duplicate=%v want true", resp["duplicate"])
	}
	// Same content in different raw formatting is still a duplicate:
	// the idempotency digest is content-based, not byte-based.
	reformatted := `{"target":{"ref":"p1-i001-01","kind":"task"},"origin":"test","iteration_id":"it-001","deadline":"2026-11-03T10:05:00Z"}`
	rec = loopRunnerRequest(t, http.MethodPost, "loop-1", "kick", reformatted, admin, api.Kick)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["duplicate"] != true {
		t.Fatalf("reformatted duplicate: status=%d body=%s, want 200 {\"duplicate\":true}", rec.Code, rec.Body.String())
	}
}

func TestKick_IdempotencyConflict409(t *testing.T) {
	api := newLoopRunnerTestAPI(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC), testLoop("loop-1", "alpha-team", "p1-i001-01"))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	rec := loopRunnerRequest(t, http.MethodPost, "loop-1", "kick", kickBody, admin, api.Kick)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first kick status=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
	// Same iteration_id, different content -> conflict.
	conflict := `{"iteration_id":"it-001","target":{"kind":"room","ref":"!other:matrix"},"origin":"test"}`
	rec = loopRunnerRequest(t, http.MethodPost, "loop-1", "kick", conflict, admin, api.Kick)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicting kick status=%d want 409 body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["code"] != "idempotency_conflict" {
		t.Fatalf("code=%v want idempotency_conflict", resp["code"])
	}
}

func TestLease_SingleWriter409(t *testing.T) {
	api := newLoopRunnerTestAPI(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC),
		testLoop("loop-1", "alpha-team", "p1-i001-01"), testLoop("loop-2", "alpha-team", "p1-i001-01"))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	// Holder A takes the lease on loop-1.
	rec := loopRunnerRequest(t, http.MethodPost, "loop-1", "lease", `{"holder":"runner-a","ttl_seconds":30}`, admin, api.AcquireLease)
	if rec.Code != http.StatusOK {
		t.Fatalf("first lease status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	first := decodeBody(t, rec)
	if first["lease_id"] == "" || first["expires_at"] == "" {
		t.Fatalf("lease response must carry lease_id and expires_at: %v", first)
	}
	if got, _ := first["expires_at"].(string); !strings.Contains(got, "2026-11-03T10:00:30Z") {
		t.Fatalf("expires_at=%v want 10:00:30Z (now+30s on the injected clock)", first["expires_at"])
	}

	// A different holder on the same loop: 409 lease_held.
	rec = loopRunnerRequest(t, http.MethodPost, "loop-1", "lease", `{"holder":"runner-b","ttl_seconds":30}`, admin, api.AcquireLease)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second holder status=%d want 409 body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeBody(t, rec)["code"]; code != "lease_held" {
		t.Fatalf("code=%v want lease_held", code)
	}

	// The current holder renews: 200 under the SAME lease id.
	rec = loopRunnerRequest(t, http.MethodPost, "loop-1", "lease", `{"holder":"runner-a","ttl_seconds":60}`, admin, api.AcquireLease)
	if rec.Code != http.StatusOK {
		t.Fatalf("renewal status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	renewed := decodeBody(t, rec)
	if renewed["lease_id"] != first["lease_id"] {
		t.Fatalf("renewal changed the lease id: %v vs %v", renewed["lease_id"], first["lease_id"])
	}

	// Another loop is independent: holder B may lease loop-2.
	rec = loopRunnerRequest(t, http.MethodPost, "loop-2", "lease", `{"holder":"runner-b","ttl_seconds":30}`, admin, api.AcquireLease)
	if rec.Code != http.StatusOK {
		t.Fatalf("lease on another loop status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandshake_RejectedTripleCheck(t *testing.T) {
	const digest = "sha256:0123456789abcdef"
	cases := []struct {
		name string
		body string
	}{
		{
			name: "protocol out of window",
			body: `{"instance":"r1","version":"0.1","protocol_version":"2","manifest_digest":"` + digest + `","capabilities":["occurrence","iteration_lease","kick"]}`,
		},
		{
			name: "no installation record (1.2.6 default)",
			body: `{"instance":"r1","version":"0.1","protocol_version":"1","manifest_digest":"` + digest + `","capabilities":["occurrence","iteration_lease","kick"]}`,
		},
		{
			name: "capability floor not met",
			body: `{"instance":"r1","version":"0.1","protocol_version":"1","manifest_digest":"` + digest + `","capabilities":["occurrence"]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newLoopRunnerTestAPI(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
			rec := internalRunnerRequest(t, tc.body, api.Handshake)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d want 409 body=%s", rec.Code, rec.Body.String())
			}
			if code := decodeBody(t, rec)["code"]; code != "handshake_rejected" {
				t.Fatalf("code=%v want handshake_rejected", code)
			}
		})
	}

	// Digest MISMATCH (record exists, digest differs) is rejected too.
	api := newLoopRunnerTestAPI(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	api.SetInstalledManifestDigest(digest)
	body := `{"instance":"r1","version":"0.1","protocol_version":"1","manifest_digest":"sha256:ffffffffffffffff","capabilities":["occurrence","iteration_lease","kick"]}`
	rec := internalRunnerRequest(t, body, api.Handshake)
	if rec.Code != http.StatusConflict || decodeBody(t, rec)["code"] != "handshake_rejected" {
		t.Fatalf("digest mismatch: status=%d body=%s, want 409 handshake_rejected", rec.Code, rec.Body.String())
	}
}

func TestHandshake_Accepted(t *testing.T) {
	api := newLoopRunnerTestAPI(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	api.SetInstalledManifestDigest("sha256:0123456789abcdef")

	body := `{"instance":"r1","version":"0.1","protocol_version":"1","manifest_digest":"sha256:0123456789abcdef","capabilities":["occurrence","iteration_lease","kick","extra"]}`
	rec := internalRunnerRequest(t, body, api.Handshake)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["accepted"] != true {
		t.Fatalf("accepted=%v want true", resp["accepted"])
	}
	if resp["controller_version"] != controllerVersion {
		t.Fatalf("controller_version=%v want %q", resp["controller_version"], controllerVersion)
	}
	win, ok := resp["protocol_window"].([]any)
	if !ok || len(win) != 2 || win[0] != "1" || win[1] != "1" {
		t.Fatalf("protocol_window=%v want [1 1]", resp["protocol_window"])
	}
	if resp["lease_ttl_seconds"] != float64(60) {
		t.Fatalf("lease_ttl_seconds=%v want 60", resp["lease_ttl_seconds"])
	}
}

// TestProjectRunnerStatus pins the LoopStatus.runner projection, one row
// per state: the default (zero) observation is absent.
func TestProjectRunnerStatus(t *testing.T) {
	cases := []struct {
		name string
		obs  RunnerObservation
		want string
	}{
		{"default projection is absent", RunnerObservation{}, v1beta1.RunnerStateAbsent},
		{"configured but never handshaken", RunnerObservation{Configured: true}, v1beta1.RunnerStateAbsent},
		{"handshake in flight", RunnerObservation{Configured: true, Handshaking: true, ObservedProtocol: "1"}, v1beta1.RunnerStateHandshaking},
		{"handshaken, lease valid", RunnerObservation{Configured: true, Handshaken: true, ObservedProtocol: "1"}, v1beta1.RunnerStateActive},
		{"lease lapsed", RunnerObservation{Configured: true, Handshaken: true, LeaseLapsed: true, ObservedProtocol: "1"}, v1beta1.RunnerStateDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProjectRunnerStatus(tc.obs)
			if got.State != tc.want {
				t.Fatalf("state=%q want %q", got.State, tc.want)
			}
			if tc.obs.ObservedProtocol != "" && got.ObservedProtocol != tc.obs.ObservedProtocol {
				t.Fatalf("observedProtocol=%q want %q", got.ObservedProtocol, tc.obs.ObservedProtocol)
			}
		})
	}
}

func TestObserveRunner_LifecycleClock(t *testing.T) {
	// Direct construction: this test advances the injected clock.
	h := newLoopTestHandler(t)
	api := NewLoopRunnerAPI(h.Client, "default")
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	api.now = func() time.Time { return now }

	// Unconfigured (1.2.6 default): absent.
	if s := ProjectRunnerStatus(api.observeRunner()); s.State != v1beta1.RunnerStateAbsent {
		t.Fatalf("default state=%q want absent", s.State)
	}

	// Handshake with an injected installation record: active.
	api.SetInstalledManifestDigest("sha256:0123456789abcdef")
	body := `{"instance":"r1","version":"0.1","protocol_version":"1","manifest_digest":"sha256:0123456789abcdef","capabilities":["occurrence","iteration_lease","kick"]}`
	if rec := internalRunnerRequest(t, body, api.Handshake); rec.Code != http.StatusOK {
		t.Fatalf("handshake status=%d body=%s", rec.Code, rec.Body.String())
	}
	s := ProjectRunnerStatus(api.observeRunner())
	if s.State != v1beta1.RunnerStateActive || s.ObservedProtocol != "1" {
		t.Fatalf("post-handshake state=%q protocol=%q, want active/1", s.State, s.ObservedProtocol)
	}

	// Just before the 2xTTL loss threshold: still active.
	now = now.Add(2*time.Minute - time.Second)
	if s := ProjectRunnerStatus(api.observeRunner()); s.State != v1beta1.RunnerStateActive {
		t.Fatalf("just before lapse state=%q want active", s.State)
	}

	// Past the 2xTTL loss threshold (injected clock): degraded.
	now = now.Add(2 * time.Second)
	if s := ProjectRunnerStatus(api.observeRunner()); s.State != v1beta1.RunnerStateDegraded {
		t.Fatalf("post-lapse state=%q want degraded", s.State)
	}
}

func TestListOccurrences_EmptyAndMissingLoop404(t *testing.T) {
	api := newLoopRunnerTestAPI(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC), testLoop("loop-1", "alpha-team", "p1-i001-01"))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	rec := loopRunnerRequest(t, http.MethodGet, "loop-1", "occurrences?limit=50", "", admin, api.ListOccurrences)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	occ, ok := resp["occurrences"].([]any)
	if !ok || len(occ) != 0 {
		t.Fatalf("occurrences=%v want [] (1.2.6 has no occurrence storage)", resp["occurrences"])
	}
	if resp["next"] != nil {
		t.Fatalf("next=%v want null", resp["next"])
	}

	// Missing loop: uniform 404.
	rec = loopRunnerRequest(t, http.MethodGet, "ghost", "occurrences", "", admin, api.ListOccurrences)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing loop status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

func TestKick_MissingLoop404(t *testing.T) {
	api := newLoopRunnerTestAPI(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	rec := loopRunnerRequest(t, http.MethodPost, "ghost", "kick", kickBody, admin, api.Kick)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing loop status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}
