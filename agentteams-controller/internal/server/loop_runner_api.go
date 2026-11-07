package server

// This file reserves the 1.2.6 loop-runner API surface: frozen
// interface + empty implementation.
//
// 1.2.6 scope and guarantees:
//   - All state lives in per-server-instance in-memory maps guarded by a
//     mutex: no persistence, no restart survival, no cross-replica
//     sharing, and no new dependencies (stdlib only).
//   - There is NO real runner: the delivery seam (deliver) is never
//     wired, so every kick degrades to 503 runner_not_configured, and
//     the handshake is rejected (no installation record) until one is
//     injected via SetInstalledManifestDigest.
//   - Stable degradation: with the runner absent, loops keep their
//     built-in ticker behavior. The endpoints answer (503 / 404 / empty
//     lists) — they never crash, never break scheduling, and never
//     silently run.
//
// The HTTP shapes below are FROZEN. A later release wires a real runner
// by filling the seams (deliver hook, occurrence writer, installation
// record) without changing the surface.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runnerProtocolWindow is the [min, max] runner protocol versions this
// controller accepts (1.2.6: exactly ["1", "1"]). Comparison is
// lexicographic — adequate for the frozen single-value window.
var runnerProtocolWindow = [2]string{"1", "1"}

const (
	// runnerLeaseTTLSeconds is the liveness lease TTL advertised to the
	// runner at handshake.
	runnerLeaseTTLSeconds = 60
	// leaseLossFactor: a runner is declared lost (loop -> degraded, the
	// in-flight iteration -> abandoned, the occurrence -> missed) when
	// its liveness lease has gone unrenewed for 2xTTL (design §5: lease
	// expiry at 2×TTL).
	leaseLossFactor = 2

	// controllerVersion is reported at handshake. The controller has no
	// build-time version injection (same "dev" convention as
	// GET /api/v1/version).
	controllerVersion = "dev"
)

// Delivery outcomes of one kick (three-state delivery, TH-15).
//
// INVARIANT (frozen): `unknown` is a TERMINAL outcome and is NEVER
// replayed. Once a kick is recorded as unknown, a duplicate request
// under the same idempotency key replays the recorded outcome — it does
// not re-deliver to the runner. Re-delivering an unknown outcome could
// double-execute an iteration whose result was merely unobservable.
const (
	deliveryDelivered = "delivered"
	deliveryUnknown   = "unknown"
	deliveryFailed    = "failed"
)

// outcomeRunnerNotConfigured is the recorded outcome for a kick that
// found no configured runner (answered 503 runner_not_configured).
const outcomeRunnerNotConfigured = "runner_not_configured"

// runnerRequiredCapabilities is the handshake capability floor: the
// runner must offer at least these (capabilities ⊇ this set).
var runnerRequiredCapabilities = []string{"occurrence", "iteration_lease", "kick"}

// KickRequest is the frozen kick body (design §3.2).
type KickRequest struct {
	// IterationID is the kick's idempotency key.
	IterationID string `json:"iteration_id"`
	// Target names what the iteration should wake.
	Target KickTarget `json:"target"`
	// Origin identifies who/what asked for the kick (informational).
	Origin string `json:"origin,omitempty"`
	// Deadline bounds the delivery (RFC3339 in JSON). Nil = no deadline.
	Deadline *time.Time `json:"deadline,omitempty"`
}

// KickTarget is the frozen kick target: kind is task | room | session.
type KickTarget struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// LeaseRequest is the frozen lease body (design §3.3).
type LeaseRequest struct {
	// Holder is the single-writer identity taking the loop lease.
	Holder string `json:"holder"`
	// TTLSeconds is the requested lease lifetime in seconds (> 0).
	TTLSeconds int `json:"ttl_seconds"`
}

// leaseResponse is the frozen lease response (design §3.3).
type leaseResponse struct {
	LeaseID   string    `json:"lease_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// HandshakeRequest is the frozen runner handshake body (design §3.4).
type HandshakeRequest struct {
	Instance        string   `json:"instance"`
	Version         string   `json:"version"`
	ProtocolVersion string   `json:"protocol_version"`
	ManifestDigest  string   `json:"manifest_digest"`
	Capabilities    []string `json:"capabilities"`
}

// loopOccurrence is one recorded loop occurrence (element of the
// §3.1 list response).
type loopOccurrence struct {
	IterationID string `json:"iteration_id"`
	// Status is the three-state delivery outcome (delivered / unknown /
	// failed) of the occurrence's kick.
	Status string `json:"status"`
}

// kickRecord is one idempotency entry: key -> content digest + result.
type kickRecord struct {
	digest  string
	outcome string // recorded result: a delivery tri-state or outcomeRunnerNotConfigured
}

// loopLease is the single-writer lease of one loop: at most one holder
// per loop at any moment.
type loopLease struct {
	holder    string
	leaseID   string
	expiresAt time.Time
}

// runnerState is the single-runner lifecycle observed by this server
// instance. One controller serves at most one runner (single writer).
type runnerState struct {
	handshaking      bool
	handshaken       bool
	observedProtocol string
	leaseGrantedAt   time.Time // zero = no active lease
}

// KickDeliverer is the runner delivery seam. 1.2.6 ships no runner, so
// it is always nil and every kick degrades to 503
// runner_not_configured. A future release wires a real implementation
// that delivers to the runner and returns the tri-state outcome.
type KickDeliverer func(ctx context.Context, loopName string, req KickRequest) (string, error)

// LoopRunnerAPI serves the reserved loop-runner API surface:
//
//	GET  /api/v1/loops/{name}/occurrences
//	POST /api/v1/loops/{name}/kick
//	POST /api/v1/loops/{name}/lease
//	POST /internal/runner/handshake
type LoopRunnerAPI struct {
	Client    client.Client
	Namespace string

	// now is the single injected clock (TH-15): every time decision
	// (lease expiry, liveness loss) goes through it, so behavior is
	// deterministic under test.
	now func() time.Time

	// deliver is the runner delivery seam (nil in 1.2.6).
	deliver KickDeliverer

	mu sync.Mutex
	// occurrences is the in-memory occurrence registry (loop name ->
	// occurrences, oldest first). 1.2.6 has no occurrence storage, so it
	// stays empty and the list endpoint always answers an empty page; the
	// registry exists as the wiring point for the future occurrence
	// writer (occurrence uniqueness, TH-15).
	occurrences map[string][]loopOccurrence
	// kicks is the in-memory idempotency registry (loop name +
	// iteration_id -> content digest + recorded result).
	kicks map[string]kickRecord
	// leases is the single-writer lease registry (loop name -> lease).
	leases map[string]loopLease
	// runner holds the single-runner lifecycle state.
	runner runnerState
	// installedManifestDigest is the digest of the installed runner
	// build a handshake must match. Empty in 1.2.6 (no installation
	// record -> every handshake rejected); inject it via
	// SetInstalledManifestDigest (the future install flow, tests).
	installedManifestDigest string
}

// NewLoopRunnerAPI builds the handler with its in-memory registries.
func NewLoopRunnerAPI(c client.Client, namespace string) *LoopRunnerAPI {
	return &LoopRunnerAPI{
		Client:      c,
		Namespace:   namespace,
		now:         time.Now,
		occurrences: map[string][]loopOccurrence{},
		kicks:       map[string]kickRecord{},
		leases:      map[string]loopLease{},
	}
}

// SetInstalledManifestDigest records the installed runner build's
// manifest digest — the value a handshake's manifest_digest must match.
// While it is unset (1.2.6: always) there is no installation record and
// every handshake is rejected.
func (a *LoopRunnerAPI) SetInstalledManifestDigest(digest string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.installedManifestDigest = digest
}

// ---- GET /api/v1/loops/{name}/occurrences --------------------------------

// ListOccurrences handles GET /api/v1/loops/{name}/occurrences.
//
// Response: {"occurrences": [...], "next": "<cursor>" | null}. 1.2.6 has
// no occurrence storage, so the page is always empty and next is always
// null; the in-memory registry is the wiring point for the future
// writer. A missing loop is a uniform 404.
func (a *LoopRunnerAPI) ListOccurrences(w http.ResponseWriter, r *http.Request) {
	loop, ok := a.requireLoop(w, r)
	if !ok {
		return
	}
	if err := a.checkLoopAccess(r.Context(), loop); err != nil {
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}

	a.mu.Lock()
	page := a.occurrences[loop.Name]
	next := any(nil)
	if len(page) > limit {
		next = strconv.Itoa(limit) // opaque cursor: offset of the next page
		page = page[:limit]
	}
	out := make([]loopOccurrence, len(page))
	copy(out, page)
	a.mu.Unlock()

	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"occurrences": out,
		"next":        next,
	})
}

// ---- POST /api/v1/loops/{name}/kick ---------------------------------------

// Kick handles POST /api/v1/loops/{name}/kick.
//
// The idempotency key is iteration_id (scoped per loop); the registry
// maps key -> content digest + recorded result. Same key + same content
// replays as 200 {"duplicate": true}; same key + different content is a
// 409 idempotency_conflict.
//
// FROZEN ordering: the idempotency check runs BEFORE the runner
// availability check — a duplicate is answered from the registry
// regardless of whether a runner is up.
//
// With no configured runner (1.2.6: always) the answer is 503
// runner_not_configured — NOT 404: the endpoint exists, only the
// executor is missing. The loop keeps its built-in ticker.
func (a *LoopRunnerAPI) Kick(w http.ResponseWriter, r *http.Request) {
	loop, ok := a.requireLoop(w, r)
	if !ok {
		return
	}
	if err := a.checkLoopAccess(r.Context(), loop); err != nil {
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	var req KickRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest,
			`invalid request body: expected a JSON object with "iteration_id", "target", "origin", "deadline"`)
		return
	}
	req.IterationID = strings.TrimSpace(req.IterationID)
	if req.IterationID == "" {
		httputil.WriteError(w, http.StatusBadRequest, `"iteration_id" is required`)
		return
	}
	req.Target.Kind = strings.ToLower(strings.TrimSpace(req.Target.Kind))
	req.Target.Ref = strings.TrimSpace(req.Target.Ref)
	if req.Target.Kind != "task" && req.Target.Kind != "room" && req.Target.Kind != "session" {
		httputil.WriteError(w, http.StatusBadRequest,
			fmt.Sprintf(`invalid target kind %q: must be "task", "room" or "session"`, req.Target.Kind))
		return
	}
	if req.Target.Ref == "" {
		httputil.WriteError(w, http.StatusBadRequest, `"target.ref" is required`)
		return
	}

	digest, err := canonicalKickDigest(req)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "cannot canonicalize the kick content")
		return
	}

	a.mu.Lock()
	key := loop.Name + "\x00" + req.IterationID
	if rec, seen := a.kicks[key]; seen {
		a.mu.Unlock()
		if rec.digest == digest {
			// Duplicate of an identical kick: replay the recorded
			// result as a no-op. INVARIANT (TH-15): this replay NEVER
			// re-delivers — a recorded `unknown` outcome is terminal
			// and is answered, not retried.
			httputil.WriteJSON(w, http.StatusOK, map[string]any{"duplicate": true})
			return
		}
		writeCodedError(w, http.StatusConflict, "idempotency_conflict",
			"a kick with the same iteration_id but different content was already recorded")
		return
	}
	// Runner availability (AFTER the idempotency check, per the frozen
	// ordering). 1.2.6: the delivery seam is never wired, so this is
	// the only branch — a stable 503 degradation, never 404.
	if a.deliver == nil {
		a.kicks[key] = kickRecord{digest: digest, outcome: outcomeRunnerNotConfigured}
		a.mu.Unlock()
		writeCodedError(w, http.StatusServiceUnavailable, "runner_not_configured",
			"no loop runner is configured; the loop keeps its built-in ticker")
		return
	}
	a.mu.Unlock()

	// Wire-in point for the future runner (unreachable in 1.2.6):
	// deliver to the runner and record the tri-state outcome.
	// INVARIANT (TH-15): an `unknown` outcome is terminal and is never
	// replayed.
	outcome, err := a.deliver(r.Context(), loop.Name, req)
	if err != nil {
		outcome = deliveryFailed
	}
	a.mu.Lock()
	a.kicks[key] = kickRecord{digest: digest, outcome: outcome}
	a.mu.Unlock()
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"outcome": outcome})
}

// ---- POST /api/v1/loops/{name}/lease --------------------------------------

// AcquireLease handles POST /api/v1/loops/{name}/lease.
//
// Single writer: at most one holder per loop at any moment. A request
// from a holder other than the current one while a live lease exists is
// a 409 lease_held. The current holder may renew (the lease is extended
// under its lease_id); an expired lease is free for takeover.
func (a *LoopRunnerAPI) AcquireLease(w http.ResponseWriter, r *http.Request) {
	loop, ok := a.requireLoop(w, r)
	if !ok {
		return
	}
	if err := a.checkLoopAccess(r.Context(), loop); err != nil {
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	var req LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest,
			`invalid request body: expected a JSON object with "holder" and "ttl_seconds"`)
		return
	}
	req.Holder = strings.TrimSpace(req.Holder)
	if req.Holder == "" {
		httputil.WriteError(w, http.StatusBadRequest, `"holder" is required`)
		return
	}
	if req.TTLSeconds <= 0 {
		httputil.WriteError(w, http.StatusBadRequest, `"ttl_seconds" must be a positive integer`)
		return
	}

	now := a.now()
	a.mu.Lock()
	if cur, held := a.leases[loop.Name]; held && cur.expiresAt.After(now) {
		if cur.holder != req.Holder {
			a.mu.Unlock()
			writeCodedError(w, http.StatusConflict, "lease_held",
				fmt.Sprintf("loop %q is leased to holder %q until %s",
					loop.Name, cur.holder, cur.expiresAt.UTC().Format(time.RFC3339)))
			return
		}
		// Renewal by the current holder: extend, keep the lease id.
		cur.expiresAt = now.Add(time.Duration(req.TTLSeconds) * time.Second)
		a.leases[loop.Name] = cur
		a.mu.Unlock()
		httputil.WriteJSON(w, http.StatusOK, leaseResponse{LeaseID: cur.leaseID, ExpiresAt: cur.expiresAt})
		return
	}
	leaseID, err := newLeaseID()
	if err != nil {
		a.mu.Unlock()
		httputil.WriteError(w, http.StatusInternalServerError, "cannot allocate a lease id")
		return
	}
	lease := loopLease{
		holder:    req.Holder,
		leaseID:   leaseID,
		expiresAt: now.Add(time.Duration(req.TTLSeconds) * time.Second),
	}
	a.leases[loop.Name] = lease
	a.mu.Unlock()
	httputil.WriteJSON(w, http.StatusOK, leaseResponse{LeaseID: lease.leaseID, ExpiresAt: lease.expiresAt})
}

// ---- POST /internal/runner/handshake --------------------------------------

// Handshake handles POST /internal/runner/handshake.
//
// The endpoint is internal machine-to-machine (no user-auth middleware,
// same pattern as the appservice transaction routes): the triple check
// below IS the authentication boundary.
//
// Triple check — any failure is a 409 handshake_rejected:
//  1. protocol_version is inside the supported window ["1", "1"]
//  2. manifest_digest matches the installation record (1.2.6: no record
//     -> always rejected until one is injected)
//  3. capabilities ⊇ {occurrence, iteration_lease, kick}
func (a *LoopRunnerAPI) Handshake(w http.ResponseWriter, r *http.Request) {
	var req HandshakeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest,
			`invalid request body: expected a JSON object with "instance", "version", "protocol_version", "manifest_digest", "capabilities"`)
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if req.ProtocolVersion < runnerProtocolWindow[0] || req.ProtocolVersion > runnerProtocolWindow[1] {
		writeCodedError(w, http.StatusConflict, "handshake_rejected",
			fmt.Sprintf("protocol_version %q is outside the supported window [%q, %q]",
				req.ProtocolVersion, runnerProtocolWindow[0], runnerProtocolWindow[1]))
		return
	}
	if a.installedManifestDigest == "" || req.ManifestDigest != a.installedManifestDigest {
		// No installation record (1.2.6) or digest mismatch: reject.
		// (A future release records the runner.handshake_failed event
		// here; 1.2.6 has no event store.)
		writeCodedError(w, http.StatusConflict, "handshake_rejected",
			"manifest_digest does not match the installed runner record")
		return
	}
	have := make(map[string]bool, len(req.Capabilities))
	for _, c := range req.Capabilities {
		have[strings.ToLower(strings.TrimSpace(c))] = true
	}
	var missing []string
	for _, c := range runnerRequiredCapabilities {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		writeCodedError(w, http.StatusConflict, "handshake_rejected",
			fmt.Sprintf("capabilities missing required entries: %s", strings.Join(missing, ", ")))
		return
	}

	// Accepted: the runner is active from now on under a liveness lease
	// of runnerLeaseTTLSeconds (declared lost after 2xTTL, design §5).
	// The dual-replica case (design §5: the later runner exits with
	// runner_already_active) is runner-side single-writer behavior; the
	// frozen controller surface enforces single-writer through the loop
	// lease endpoint above.
	now := a.now()
	a.runner = runnerState{
		handshaken:       true,
		observedProtocol: req.ProtocolVersion,
		leaseGrantedAt:   now,
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"accepted":           true,
		"controller_version": controllerVersion,
		"protocol_window":    runnerProtocolWindow,
		"lease_ttl_seconds":  runnerLeaseTTLSeconds,
	})
}

// ---- runner status projection (LoopStatus.runner) -------------------------

// RunnerObservation is one snapshot of the runner lifecycle as the
// controller sees it. ProjectRunnerStatus is a pure function of this
// snapshot, so the mapping is unit-testable without HTTP or registries.
type RunnerObservation struct {
	Configured       bool // an installation record exists
	Handshaking      bool // a handshake is in flight
	Handshaken       bool // a handshake was accepted
	LeaseLapsed      bool // the liveness lease has gone unrenewed past 2xTTL
	ObservedProtocol string
}

// ProjectRunnerStatus maps a lifecycle snapshot to the LoopStatus.runner
// projection (design §7):
//
//	unconfigured, or no handshake (including a failed one) -> absent
//	handshake in flight                                  -> handshaking
//	liveness lease lapsed                                -> degraded
//	handshake accepted and the lease is valid            -> active
func ProjectRunnerStatus(obs RunnerObservation) *v1beta1.RunnerStatus {
	switch {
	case !obs.Configured || (!obs.Handshaken && !obs.Handshaking):
		// No installation record, never handshaken, or the handshake
		// failed: the loop runs on its built-in ticker.
		return &v1beta1.RunnerStatus{State: v1beta1.RunnerStateAbsent}
	case obs.Handshaking:
		return &v1beta1.RunnerStatus{State: v1beta1.RunnerStateHandshaking, ObservedProtocol: obs.ObservedProtocol}
	case obs.LeaseLapsed:
		return &v1beta1.RunnerStatus{State: v1beta1.RunnerStateDegraded, ObservedProtocol: obs.ObservedProtocol}
	default: // Handshaken, lease valid.
		return &v1beta1.RunnerStatus{State: v1beta1.RunnerStateActive, ObservedProtocol: obs.ObservedProtocol}
	}
}

// observeRunner snapshots the lifecycle for the status projection — the
// single point a future reconciler reads to mirror LoopStatus.runner.
func (a *LoopRunnerAPI) observeRunner() RunnerObservation {
	a.mu.Lock()
	defer a.mu.Unlock()
	lapsed := false
	if a.runner.handshaken && !a.runner.leaseGrantedAt.IsZero() {
		// Design §5: loss is declared when the liveness lease has gone
		// unrenewed for 2xTTL. 1.2.6 has no renew endpoint, so a
		// handshaken runner lapses at grant+2xTTL (the single injected
		// clock drives it; a lapsed runner's iteration is abandoned and
		// its occurrence missed — an `unknown` kick is never replayed).
		lapsed = a.now().After(a.runner.leaseGrantedAt.Add(time.Duration(leaseLossFactor*runnerLeaseTTLSeconds) * time.Second))
	}
	return RunnerObservation{
		Configured:       a.installedManifestDigest != "",
		Handshaking:      a.runner.handshaking,
		Handshaken:       a.runner.handshaken,
		LeaseLapsed:      lapsed,
		ObservedProtocol: a.runner.observedProtocol,
	}
}

// ---- helpers ----------------------------------------------------------------

// requireLoop loads the Loop CR by path name and answers a uniform 404
// when it does not exist (same not-found behavior as pause/resume).
func (a *LoopRunnerAPI) requireLoop(w http.ResponseWriter, r *http.Request) (*v1beta1.Loop, bool) {
	name := r.PathValue("name")
	loop := &v1beta1.Loop{}
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: a.Namespace, Name: name}, loop); err != nil {
		httputil.WriteError(w, http.StatusNotFound, fmt.Sprintf("loop %q not found", name))
		return nil, false
	}
	return loop, true
}

// checkLoopAccess mirrors LoopHandler.checkLoopAccess (non-resume
// matrix). The middleware cannot resolve loop -> team, so the handler is
// the real team-scope boundary (defense in depth, same pattern as
// pause/resume).
func (a *LoopRunnerAPI) checkLoopAccess(ctx context.Context, loop *v1beta1.Loop) error {
	caller := auth.CallerFromContext(ctx)
	if caller == nil {
		// No identity in context (e.g. direct handler invocation
		// without authentication): the middleware layer is responsible;
		// nothing to check here.
		return nil
	}
	switch caller.Role {
	case auth.RoleAdmin, auth.RoleManager:
		return nil
	case auth.RoleTeamLeader, auth.RoleHuman:
		return loopTeamAccess(caller, loop.Spec.TeamName)
	case auth.RoleWorker:
		return fmt.Errorf("authorization denied: worker may not control loops")
	default:
		return fmt.Errorf("authorization denied: unknown role %q", caller.Role)
	}
}

// writeCodedError writes the runner API's error shape: the frozen,
// machine-readable `code` plus a human-readable message.
func writeCodedError(w http.ResponseWriter, status int, code, message string) {
	httputil.WriteJSON(w, status, map[string]any{"code": code, "message": message})
}

// canonicalKickDigest hashes the canonical JSON of the kick content: two
// requests are "same content" iff their digests match, regardless of
// raw-body formatting or key order.
func canonicalKickDigest(req KickRequest) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// newLeaseID mints a lease id (crypto/rand; stdlib only — 1.2.6 adds no
// dependencies).
func newLeaseID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "lease-" + hex.EncodeToString(b[:]), nil
}
