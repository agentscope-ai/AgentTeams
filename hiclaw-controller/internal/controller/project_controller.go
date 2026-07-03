package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/metrics"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Project condition types (plan §10.1 / decisions #16, #18).
const (
	ConditionReposResolved      = "ReposResolved"
	ConditionWorkersRecorded    = "WorkersRecorded"
	ConditionMinIOProjected     = "MinIOProjected"
	ConditionLeaderNotified     = "LeaderNotified"
	ConditionDeprovisionPending = "DeprovisionPending"
)

const (
	conditionTrue    = "True"
	conditionFalse   = "False"
	conditionUnknown = "Unknown"
)

// ProjectAdminMessenger is the narrow slice of service.Provisioner this
// reconciler needs to notify a team's lead (step 8b, decision #16). Defined
// locally (rather than widening service.WorkerProvisioner) because the
// Project reconciler never provisions Matrix users/rooms — it only sends one
// best-effort admin message once MinIOProjected is True (retried on later
// reconciles until it actually lands, e.g. once the team room exists).
type ProjectAdminMessenger interface {
	SendAdminMessage(ctx context.Context, roomID, body string) error
}

// ProjectReconciler reconciles Project resources (plan §10.1). It performs
// NO Gitea calls and NO gateway calls, and holds NO PATs — the per-worker
// Gitea identity, mcp-gitea-<worker> registration, and repo-collaborator role
// are applied out-of-band by the operator helper (scripts/provision-worker-gitea.sh,
// decisions #12/#13) reading the manifest this reconciler projects to MinIO.
type ProjectReconciler struct {
	client.Client

	OSS       oss.StorageClient     // MinIO projection only
	Messenger ProjectAdminMessenger // notify the team lead once (step 8b); may be nil to skip notification

	// ControllerName, when non-empty, is stamped as hiclaw.io/controller on
	// resources this reconciler manages directly (currently unused for
	// Project since it creates no child objects, but kept for symmetry with
	// the other reconcilers and future use).
	ControllerName string
}

func (r *ProjectReconciler) Reconcile(ctx context.Context, req reconcile.Request) (retres reconcile.Result, reterr error) {
	start := time.Now()
	defer func() { metrics.Observe("project", start, reterr) }()

	logger := log.FromContext(ctx)

	var proj v1beta1.Project
	if err := r.Get(ctx, req.NamespacedName, &proj); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	patchBase := client.MergeFrom(proj.DeepCopy())

	defer func() {
		if !proj.DeletionTimestamp.IsZero() {
			return
		}
		if reterr == nil {
			proj.Status.ObservedGeneration = proj.Generation
			if proj.Status.Phase != "Completed" && proj.Status.Phase != "Archived" {
				proj.Status.Message = ""
			}
		} else {
			proj.Status.Message = reterr.Error()
		}
		if err := r.Status().Patch(ctx, &proj, patchBase); err != nil {
			logger.Error(err, "failed to patch project status")
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	if !proj.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&proj, finalizerName) {
			return r.reconcileDelete(ctx, &proj)
		}
		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&proj, finalizerName) {
		base := proj.DeepCopy()
		controllerutil.AddFinalizer(&proj, finalizerName)
		if err := r.Patch(ctx, &proj, client.MergeFrom(base)); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Operator-set terminal phases are never overwritten by reconcile
	// (decision #18) — once Completed/Archived, only status.message/condition
	// bookkeeping (DeprovisionPending) continues.
	if proj.Status.Phase == "Completed" || proj.Status.Phase == "Archived" {
		return r.reconcileCompleted(ctx, &proj)
	}

	return r.reconcileNormal(ctx, &proj)
}

// reconcileNormal runs the idempotent convergence steps: resolve team,
// resolve/record assigned workers, project to MinIO, notify the lead once.
func (r *ProjectReconciler) reconcileNormal(ctx context.Context, proj *v1beta1.Project) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	projectID := proj.Spec.EffectiveProjectName(proj.Name)

	proj.Status.RepoCount = len(proj.Spec.Repos)

	// --- Resolve team (decision #2: team-scoped) ---
	var team v1beta1.Team
	if err := r.Get(ctx, client.ObjectKey{Name: proj.Spec.Team, Namespace: proj.Namespace}, &team); err != nil {
		proj.Status.Phase = "Degraded"
		proj.Status.SetCondition(ConditionReposResolved, conditionFalse, "TeamNotFound", err.Error())
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("resolve team %q: %w", proj.Spec.Team, err)
	}
	proj.Status.SetCondition(ConditionReposResolved, conditionTrue, "Resolved", fmt.Sprintf("%d repo(s) bound", len(proj.Spec.Repos)))

	// --- Resolve assigned workers ---
	workers := proj.Spec.Workers
	if len(workers) == 0 {
		workers = make([]string, 0, len(team.Status.Members))
		for _, m := range team.Status.Members {
			runtimeName := m.RuntimeName
			if runtimeName == "" {
				runtimeName = m.Name
			}
			workers = append(workers, runtimeName)
		}
	}
	proj.Status.RecordedWorkers = workers
	proj.Status.SetCondition(ConditionWorkersRecorded, conditionTrue, "Recorded", fmt.Sprintf("%d worker(s) recorded", len(workers)))

	// --- MinIO projection ---
	if r.OSS != nil {
		manifest := projectManifest{
			ID:              projectID,
			Team:            proj.Spec.Team,
			Description:     proj.Spec.Description,
			RecordedWorkers: workers,
			UpdatedAt:       metav1.Now().UTC().Format(time.RFC3339),
		}
		for _, repo := range proj.Spec.Repos {
			manifest.Repos = append(manifest.Repos, projectManifestRepo{
				URL:    repo.URL,
				Access: repo.Access,
				Name:   repo.Name,
			})
		}
		b, err := json.Marshal(manifest)
		if err != nil {
			proj.Status.Phase = "Degraded"
			proj.Status.SetCondition(ConditionMinIOProjected, conditionFalse, "MarshalFailed", err.Error())
			return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("marshal manifest: %w", err)
		}
		key := "shared/projects/" + projectID + "/manifest.json"
		if err := r.OSS.PutObject(ctx, key, b); err != nil {
			proj.Status.Phase = "Degraded"
			proj.Status.SetCondition(ConditionMinIOProjected, conditionFalse, "PutObjectFailed", err.Error())
			return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("project manifest to %s: %w", key, err)
		}
		proj.Status.SetCondition(ConditionMinIOProjected, conditionTrue, "Projected", "manifest written to "+key)

		// --- Notify the lead once (step 8b, decision #16) ---
		// Called on every successful projection pass, not just the
		// MinIOProjected False->True edge: notifyLeaderOnce itself
		// short-circuits once LeaderNotified is already True, so this
		// stays exactly-once in the common case while still letting a
		// later pass deliver the notification if the team room wasn't
		// provisioned yet (Team.Status.TeamRoomID == "") on the first
		// successful projection.
		r.notifyLeaderOnce(ctx, proj, &team, projectID, key)
	} else {
		proj.Status.SetCondition(ConditionMinIOProjected, conditionUnknown, "NoStorageClient", "OSS storage client not configured")
	}

	if proj.Status.ConditionByType(ConditionMinIOProjected) != nil &&
		proj.Status.ConditionByType(ConditionMinIOProjected).Status == conditionTrue {
		proj.Status.Phase = "Ready"
	} else {
		proj.Status.Phase = "Provisioning"
	}

	logger.Info("project reconciled", "name", proj.Name, "phase", proj.Status.Phase, "workers", len(workers))
	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

// notifyLeaderOnce posts a message into the team's room informing the lead a
// new project manifest is available. Best-effort: failures are logged, never
// fatal, and LeaderNotified stays False so the next successful MinIO
// projection pass can retry (fire-at-least-once, not fire-exactly-once, on
// persistent Matrix outage).
func (r *ProjectReconciler) notifyLeaderOnce(ctx context.Context, proj *v1beta1.Project, team *v1beta1.Team, projectID, manifestKey string) {
	logger := log.FromContext(ctx)
	if existing := proj.Status.ConditionByType(ConditionLeaderNotified); existing != nil && existing.Status == conditionTrue {
		return // already notified
	}
	if r.Messenger == nil {
		return
	}
	roomID := team.Status.TeamRoomID
	if roomID == "" {
		logger.Info("skipping leader notification: team room not provisioned yet", "team", team.Name)
		return
	}
	body := fmt.Sprintf(
		"New Project manifest available: id=%s, path=%s. Run projectflow(create_project) to start planning.",
		projectID, manifestKey,
	)
	if err := r.Messenger.SendAdminMessage(ctx, roomID, body); err != nil {
		logger.Error(err, "failed to notify team lead of new project (non-fatal)", "team", team.Name, "project", proj.Name)
		return
	}
	proj.Status.SetCondition(ConditionLeaderNotified, conditionTrue, "Notified", "posted to team room "+roomID)
}

// reconcileCompleted handles operator-set terminal phases (decision #18).
// Completed raises DeprovisionPending=True (surfaced by the dashboard as "run
// provision-worker-gitea.sh --deprovision <id>"); Archived additionally moves
// the MinIO projection to a cold prefix. The controller still makes no Gitea
// calls in either case.
func (r *ProjectReconciler) reconcileCompleted(ctx context.Context, proj *v1beta1.Project) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	projectID := proj.Spec.EffectiveProjectName(proj.Name)

	if existing := proj.Status.ConditionByType(ConditionDeprovisionPending); existing == nil || existing.Status != conditionTrue {
		proj.Status.SetCondition(ConditionDeprovisionPending, conditionTrue, proj.Status.Phase,
			"project marked "+proj.Status.Phase+"; run provision-worker-gitea.sh --deprovision "+projectID)
	}

	if proj.Status.Phase == "Archived" && r.OSS != nil {
		liveKey := "shared/projects/" + projectID + "/manifest.json"
		coldKey := "shared/projects-archived/" + projectID + "/manifest.json"
		if data, err := r.OSS.GetObject(ctx, liveKey); err == nil {
			if err := r.OSS.PutObject(ctx, coldKey, data); err != nil {
				logger.Error(err, "failed to move archived project manifest to cold prefix (non-fatal)", "project", proj.Name)
			} else if err := r.OSS.DeletePrefix(ctx, "shared/projects/"+projectID+"/"); err != nil {
				logger.Error(err, "failed to clear live prefix after archiving (non-fatal)", "project", proj.Name)
			}
		}
	}

	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

// reconcileDelete removes the MinIO projection (non-fatal) and the finalizer.
// The controller makes NO Gitea calls here — the operator helper de-provisions
// Gitea users / mcp-gitea-<worker> registrations / collaborator membership
// out-of-band.
func (r *ProjectReconciler) reconcileDelete(ctx context.Context, proj *v1beta1.Project) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	projectID := proj.Spec.EffectiveProjectName(proj.Name)
	logger.Info("deleting project", "name", proj.Name)

	if r.OSS != nil {
		if err := r.OSS.DeletePrefix(ctx, "shared/projects/"+projectID+"/"); err != nil {
			logger.Error(err, "failed to delete project MinIO projection (non-fatal)", "name", proj.Name)
		}
	}

	base := proj.DeepCopy()
	controllerutil.RemoveFinalizer(proj, finalizerName)
	if err := r.Patch(ctx, proj, client.MergeFrom(base)); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("project deleted", "name", proj.Name)
	return reconcile.Result{}, nil
}

func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Project{}).
		Watches(
			&v1beta1.Team{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				team, ok := obj.(*v1beta1.Team)
				if !ok {
					return nil
				}
				var projects v1beta1.ProjectList
				if err := r.List(ctx, &projects, client.InNamespace(team.Namespace)); err != nil {
					return nil
				}
				requests := make([]reconcile.Request, 0, len(projects.Items))
				for i := range projects.Items {
					if projects.Items[i].Spec.Team != team.Name {
						continue
					}
					requests = append(requests, reconcile.Request{
						NamespacedName: client.ObjectKey{
							Name:      projects.Items[i].Name,
							Namespace: projects.Items[i].Namespace,
						},
					})
				}
				return requests
			}),
		).
		Complete(r)
}

// --- MinIO manifest shape (plan §10.1 step 5) ---

type projectManifest struct {
	ID              string                `json:"id"`
	Team            string                `json:"team"`
	Description     string                `json:"description,omitempty"`
	Repos           []projectManifestRepo `json:"repos"`
	RecordedWorkers []string              `json:"recordedWorkers,omitempty"`
	UpdatedAt       string                `json:"updatedAt"`
}

type projectManifestRepo struct {
	URL    string `json:"url"`
	Access string `json:"access"`
	Name   string `json:"name,omitempty"`
}
