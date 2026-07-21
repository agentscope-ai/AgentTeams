package controller

import (
	"context"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// welcomeRequeueInterval is how long to wait before re-checking when the
// Manager Matrix user has not yet joined the Admin DM room. Kept short
// because the gap between container start and OpenClaw's first /sync
// auto-join is typically a few seconds; longer than this makes the
// admin's Element Web window sit empty for an uncomfortable time on
// fresh installs. The cost of the 5s loop is one ListRoomMembers HTTP
// call against the local Tuwunel — negligible — and the loop terminates
// the moment the agent's auto-join lands.
const welcomeRequeueInterval = 5 * time.Second

// reconcileManagerWelcome delivers the first-boot onboarding prompt that
// asks the Manager Agent to greet the admin and ask the four identity
// questions (name / language / style / behavior). It is the
// new-architecture replacement for the legacy in-container welcome flow
// that lived in `start-manager-agent.sh` and only ran when
// AGENTTEAMS_RUNTIME != "k8s". The legacy path remains untouched for
// docker single-container deploys; in k8s / embedded mode the controller
// owns this responsibility because:
//
//   - it has admin Matrix credentials cached in TuwunelClient already;
//   - it knows when the DM Room was just created (via Status.WelcomeSent);
//   - it does not need to give every Manager container the admin password
//     just to send one prompt at boot.
//
// Concurrency model — claim-before-send:
//
// Two reconciles can race on the same Manager (e.g. the post-Create
// requeue from Reconciler N+1 starts before Reconciler N's deferred
// Status().Patch has landed AND been observed by the informer cache).
// Both would otherwise read WelcomeSent=false and both would call
// SendManagerWelcomeMessage, double-delivering the prompt to the admin
// DM. The deferred end-of-Reconcile status patch protects ObservedGeneration
// (resource-version conflict on the loser) but does NOT protect
// non-idempotent side effects.
//
// To make "exactly once" deterministic regardless of cache lag, we:
//
//  1. Skip if WelcomeSent already true (idempotency).
//
//  2. Skip if no RoomID — provisioning hasn't reached Step 4 yet.
//
//  3. Skip if container not Running/Ready (no point if OpenClaw isn't
//     up to receive the message anyway).
//
//  4. Two side-effect-free readiness gates, polled on every requeue
//     WITHOUT touching status (no claim churn while we wait):
//
//     a. IsManagerJoinedDM — the manager Matrix user must have actually
//     joined the Admin DM room before we send. Otherwise the welcome
//     lands in the room's historical timeline, which OpenClaw / hermes /
//     copaw drop during their first-boot catch-up sync.
//
//     b. IsManagerLLMAuthReady — Higress's WASM key-auth filter must
//     have finished syncing the manager's consumer credential. This
//     activation is asynchronous and takes ~40-45s on first install
//     (the legacy `start-manager-agent.sh` papered over it with a
//     `sleep 45` after Higress setup). Auto-join (~10s) lands long
//     before auth propagation (~45s), so gating on join alone would
//     deliver the welcome while the manager's first
//     /v1/chat/completions call is still 401ing — the prompt arrives,
//     the agent tries to reply, the LLM call fails, and the onboarding
//     turn is silently lost. The probe POSTs a sentinel body that
//     never reaches the upstream LLM (see Provisioner.IsManagerLLMAuthReady).
//
//     Either signal returning false → RequeueAfter and exit. We poll
//     both on every requeue rather than caching the first one because
//     the cost is two cheap HTTP calls against the local homeserver and
//     gateway, and remembering the join-then-wait-for-auth state across
//     reconciles would require yet another Status field.
//
//  5. CLAIM the slot: set WelcomeSent=true and immediately Status().Patch
//     with optimistic concurrency. If another reconcile already claimed
//     and committed, our patch returns Conflict and we abort with no
//     send. If our patch wins, every later reconcile that re-reads from
//     the API server (or the cache once it catches up) will see
//     WelcomeSent=true and skip via step 1.
//
//  6. Refresh the scope's patchBase so the deferred end-of-Reconcile
//     patch starts from the post-claim snapshot (otherwise it would
//     re-apply the same WelcomeSent=true diff and could lose other
//     status fields modified later in this same reconcile).
//
//  7. SEND. If the send call returns an error AFTER the claim is
//     committed, log loudly and return nil. We do NOT roll back the
//     claim — silent re-attempt would risk the race re-opening, and the
//     admin can prompt the agent manually if a transient Matrix hiccup
//     ate the first message. Missing welcome is recoverable; permanent
//     double welcome is not.
func (r *ManagerReconciler) reconcileManagerWelcome(ctx context.Context, s *managerScope) (reconcile.Result, error) {
	m := s.manager
	if m.Status.WelcomeSent {
		return reconcile.Result{}, nil
	}
	if m.Status.RoomID == "" {
		return reconcile.Result{}, nil
	}

	logger := log.FromContext(ctx)

	wb := r.managerBackend(ctx)
	if wb != nil {
		st, err := wb.Status(ctx, r.managerContainerName(m.Name))
		if err == nil {
			switch st.Status {
			case backend.StatusRunning, backend.StatusReady:
				// container is live, proceed to membership check
			default:
				// container not yet usable; rely on the next reconcile
				// (triggered by the Pod-watch mapper for k8s, or the
				// standard reconcileInterval for docker)
				return reconcile.Result{}, nil
			}
		}
	}

	// Readiness gate (a): manager must be in the DM room so the welcome
	// lands as a live event rather than historical timeline.
	joined, err := r.Provisioner.IsManagerJoinedDM(ctx, m.Status.RoomID)
	if err != nil {
		// Best-effort: log and try again on the next reconcile. Don't
		// flip the manager phase to Failed — the agent still works; only
		// the welcome message is delayed.
		logger.Error(err, "manager welcome membership check failed (non-fatal, will retry)",
			"manager", m.Name, "roomID", m.Status.RoomID)
		return reconcile.Result{RequeueAfter: welcomeRequeueInterval}, nil
	}
	if !joined {
		logger.V(1).Info("manager not yet joined DM room, requeue for welcome",
			"manager", m.Name, "roomID", m.Status.RoomID)
		return reconcile.Result{RequeueAfter: welcomeRequeueInterval}, nil
	}

	// Readiness gate (b): Higress WASM key-auth must have propagated the
	// manager's consumer key onto the AI route. Without this the welcome
	// lands but the manager's reply attempt 401s against the gateway and
	// the onboarding turn is lost. s.provResult.GatewayKey is the freshly
	// minted key for both the first-boot path and the credential-refresh
	// path — reconcileManagerInfrastructure populates it in both.
	gatewayKey := ""
	if s.provResult != nil {
		gatewayKey = s.provResult.GatewayKey
	}
	authReady, err := r.Provisioner.IsManagerLLMAuthReady(ctx, gatewayKey)
	if err != nil {
		logger.Error(err, "manager welcome llm-auth probe failed (non-fatal, will retry)",
			"manager", m.Name)
		return reconcile.Result{RequeueAfter: welcomeRequeueInterval}, nil
	}
	if !authReady {
		logger.V(1).Info("manager llm-auth not yet propagated by gateway, requeue for welcome",
			"manager", m.Name)
		return reconcile.Result{RequeueAfter: welcomeRequeueInterval}, nil
	}

	// CLAIM. Use a SEPARATE deep copy for the claim patch so the working
	// `m` is not mutated by the API server response — controller-runtime
	// repopulates the object passed to Status().Patch with whatever the
	// server returns, which would clobber any in-memory status fields
	// (e.g. RoomID) that earlier reconcile steps set but that have not
	// yet been persisted by the deferred end-of-Reconcile patch.
	//
	// The patch we send only contains the WelcomeSent diff; the deferred
	// end-of-Reconcile patch (which uses the scope's original patchBase)
	// will atomically persist RoomID + MatrixUserID + WelcomeSent + the
	// usual ObservedGeneration / Phase machinery in one go a few lines
	// later.
	claimCopy := m.DeepCopy()
	claimCopy.Status.WelcomeSent = true
	if err := r.Status().Patch(ctx, claimCopy, client.MergeFrom(m.DeepCopy())); err != nil {
		if apierrors.IsConflict(err) {
			// Another reconcile won the claim. The next reconcile
			// triggered by their successful patch will read
			// WelcomeSent=true from the API server and skip via step 1.
			// We do not requeue from here — the winner's reconcile (or
			// the standard reconcileInterval) will be the source of
			// truth for any further work.
			logger.V(1).Info("manager welcome claim lost to a concurrent reconcile, skipping",
				"manager", m.Name)
			return reconcile.Result{}, nil
		}
		// Real API error — surface it so it shows up in metrics, but
		// don't fail the manager: the next reconcile will retry the
		// claim.
		logger.Error(err, "manager welcome claim patch failed (non-fatal, will retry)",
			"manager", m.Name)
		return reconcile.Result{RequeueAfter: welcomeRequeueInterval}, nil
	}
	// Mirror the now-persisted flag onto the working `m` so the deferred
	// end-of-Reconcile patch (which diffs against the scope's original
	// patchBase) sees a consistent post-claim state. Other status fields
	// are intentionally left as the working reconcile set them.
	m.Status.WelcomeSent = true

	// SEND. Failures here are intentionally NOT rolled back — see the
	// big concurrency-model comment above.
	if err := r.Provisioner.SendManagerWelcomeMessage(ctx, service.ManagerWelcomeRequest{
		RoomID:   m.Status.RoomID,
		Language: r.UserLanguage,
		Timezone: r.UserTimezone,
	}); err != nil {
		logger.Error(err, "manager welcome send failed AFTER claim — admin may need to prompt the agent manually",
			"manager", m.Name, "roomID", m.Status.RoomID)
		return reconcile.Result{}, nil
	}

	logger.Info("manager onboarding welcome sent", "manager", m.Name, "roomID", m.Status.RoomID)
	return reconcile.Result{}, nil
}
