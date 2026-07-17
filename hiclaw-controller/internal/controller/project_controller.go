package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/metrics"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ConditionStorageIdentityReady = "StorageIdentityReady"
	ConditionReposResolved       = "ReposResolved"
	ConditionWorkersRecorded     = "WorkersRecorded"
	ConditionMinIOProjected      = "MinIOProjected"
	ConditionArchiveProjected    = "ArchiveProjected"
	ConditionLeaderNotified      = "LeaderNotified"
	ConditionDeprovisionPending  = "DeprovisionPending"
)

const (
	conditionTrue    = "True"
	conditionFalse   = "False"
	conditionUnknown = "Unknown"
)

const (
	projectLiveRoot    = "shared/projects/"
	projectArchiveRoot = "shared/projects-archived/"
)

type ProjectAdminMessenger interface {
	SendAdminMessage(ctx context.Context, roomID, body string) error
}

type ProjectReconciler struct {
	client.Client
	OSS            oss.StorageClient
	Messenger      ProjectAdminMessenger
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

	storageKey, err := r.ensureStorageIdentity(ctx, &proj)
	if err != nil {
		proj.Status.Phase = "Degraded"
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, err
	}
	if proj.Status.Phase == "Completed" || proj.Status.Phase == "Archived" {
		return r.reconcileCompleted(ctx, &proj, storageKey)
	}
	return r.reconcileNormal(ctx, &proj, storageKey)
}

func projectStorageKey(proj *v1beta1.Project) string {
	if proj.Status.StorageKey != "" {
		return proj.Status.StorageKey
	}
	return proj.Spec.EffectiveProjectName(proj.Name)
}

func (r *ProjectReconciler) ensureStorageIdentity(ctx context.Context, proj *v1beta1.Project) (string, error) {
	candidate := proj.Spec.EffectiveProjectName(proj.Name)
	key := projectStorageKey(proj)
	if proj.Status.StorageKey == "" {
		proj.Status.StorageKey = key
	}
	if errs := kvalidation.IsDNS1123Subdomain(key); len(errs) > 0 {
		msg := strings.Join(errs, "; ")
		proj.Status.SetCondition(ConditionStorageIdentityReady, conditionFalse, "InvalidStorageKey", msg)
		return key, fmt.Errorf("invalid project storage key %q: %s", key, msg)
	}
	if proj.Spec.ProjectName != "" && candidate != key {
		msg := fmt.Sprintf("spec.projectName resolved to %q after storage key %q was assigned", candidate, key)
		proj.Status.SetCondition(ConditionStorageIdentityReady, conditionFalse, "ProjectNameChanged", msg)
		return key, errors.New(msg)
	}
	if err := r.ensureNoStorageCollision(ctx, proj, key); err != nil {
		proj.Status.SetCondition(ConditionStorageIdentityReady, conditionFalse, "StorageKeyCollision", err.Error())
		return key, err
	}
	if r.OSS != nil {
		if err := r.validateManifestOwnership(ctx, proj, projectLiveRoot+key+"/manifest.json"); err != nil {
			proj.Status.SetCondition(ConditionStorageIdentityReady, conditionFalse, "ManifestOwnershipConflict", err.Error())
			return key, err
		}
	}
	proj.Status.SetCondition(ConditionStorageIdentityReady, conditionTrue, "Unique", "storage key "+key+" is uniquely owned")
	return key, nil
}

func (r *ProjectReconciler) ensureNoStorageCollision(ctx context.Context, proj *v1beta1.Project, key string) error {
	var projects v1beta1.ProjectList
	if err := r.List(ctx, &projects); err != nil {
		return fmt.Errorf("list projects for storage identity: %w", err)
	}
	for i := range projects.Items {
		other := &projects.Items[i]
		if other.Namespace == proj.Namespace && other.Name == proj.Name {
			continue
		}
		if projectStorageKey(other) == key {
			return fmt.Errorf("storage key %q is also claimed by Project %s/%s", key, other.Namespace, other.Name)
		}
	}
	return nil
}

func (r *ProjectReconciler) validateManifestOwnership(ctx context.Context, proj *v1beta1.Project, manifestKey string) error {
	data, err := r.OSS.GetObject(ctx, manifestKey)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing project manifest %s: %w", manifestKey, err)
	}
	var manifest projectManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse existing project manifest %s: %w", manifestKey, err)
	}
	// A legacy ownerless manifest can be claimed after the global collision
	// check proves exactly one Project resolves to this storage key.
	if manifest.Owner.Name == "" && manifest.Owner.Namespace == "" && manifest.Owner.UID == "" {
		return nil
	}
	if manifest.Owner.Namespace != proj.Namespace || manifest.Owner.Name != proj.Name ||
		(manifest.Owner.UID != "" && manifest.Owner.UID != string(proj.UID)) {
		return fmt.Errorf("manifest %s is owned by Project %s/%s (uid %s), not %s/%s (uid %s)",
			manifestKey, manifest.Owner.Namespace, manifest.Owner.Name, manifest.Owner.UID,
			proj.Namespace, proj.Name, proj.UID)
	}
	return nil
}

func (r *ProjectReconciler) reconcileNormal(ctx context.Context, proj *v1beta1.Project, projectID string) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	proj.Status.RepoCount = len(proj.Spec.Repos)

	var team v1beta1.Team
	if err := r.Get(ctx, client.ObjectKey{Name: proj.Spec.Team, Namespace: proj.Namespace}, &team); err != nil {
		proj.Status.Phase = "Degraded"
		proj.Status.SetCondition(ConditionReposResolved, conditionFalse, "TeamNotFound", err.Error())
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("resolve team %q: %w", proj.Spec.Team, err)
	}
	proj.Status.SetCondition(ConditionReposResolved, conditionTrue, "Resolved", fmt.Sprintf("%d repo(s) bound", len(proj.Spec.Repos)))

	workers, err := r.resolveProjectWorkers(ctx, proj, &team)
	if err != nil {
		proj.Status.Phase = "Degraded"
		proj.Status.SetCondition(ConditionWorkersRecorded, conditionFalse, "InvalidAssignment", err.Error())
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, err
	}
	proj.Status.RecordedWorkers = workers
	proj.Status.SetCondition(ConditionWorkersRecorded, conditionTrue, "Recorded", fmt.Sprintf("%d worker(s) recorded", len(workers)))

	if r.OSS != nil {
		manifest := projectManifest{
			ID: projectID,
			Owner: projectManifestOwner{
				Namespace: proj.Namespace,
				Name:      proj.Name,
				UID:       string(proj.UID),
			},
			Team:            proj.Spec.Team,
			Description:     proj.Spec.Description,
			RecordedWorkers: workers,
			UpdatedAt:       metav1.Now().UTC().Format(time.RFC3339),
		}
		for _, repo := range proj.Spec.Repos {
			manifest.Repos = append(manifest.Repos, projectManifestRepo{URL: repo.URL, Access: repo.Access, Name: repo.Name})
		}
		b, err := json.Marshal(manifest)
		if err != nil {
			proj.Status.Phase = "Degraded"
			proj.Status.SetCondition(ConditionMinIOProjected, conditionFalse, "MarshalFailed", err.Error())
			return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("marshal manifest: %w", err)
		}
		key := projectLiveRoot + projectID + "/manifest.json"
		if err := r.OSS.PutObject(ctx, key, b); err != nil {
			proj.Status.Phase = "Degraded"
			proj.Status.SetCondition(ConditionMinIOProjected, conditionFalse, "PutObjectFailed", err.Error())
			return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("project manifest to %s: %w", key, err)
		}
		proj.Status.SetCondition(ConditionMinIOProjected, conditionTrue, "Projected", "manifest written to "+key)
		r.notifyLeaderOnce(ctx, proj, &team, projectID, key)
	} else {
		proj.Status.SetCondition(ConditionMinIOProjected, conditionUnknown, "NoStorageClient", "OSS storage client not configured")
	}

	if c := proj.Status.ConditionByType(ConditionMinIOProjected); c != nil && c.Status == conditionTrue {
		proj.Status.Phase = "Ready"
	} else {
		proj.Status.Phase = "Provisioning"
	}
	r.resolveProjectDependencies(ctx, proj)
	logger.Info("project reconciled", "name", proj.Name, "phase", proj.Status.Phase, "workers", len(workers))
	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

func (r *ProjectReconciler) resolveProjectWorkers(ctx context.Context, proj *v1beta1.Project, team *v1beta1.Team) ([]string, error) {
	aliases := make(map[string]string)
	ordered := make([]string, 0)
	add := func(name, runtimeName string) error {
		name = strings.TrimSpace(name)
		runtimeName = strings.TrimSpace(runtimeName)
		if name == "" || runtimeName == "" {
			return fmt.Errorf("team %s has an empty worker identity", team.Name)
		}
		if existing, ok := aliases[runtimeName]; ok && existing != runtimeName {
			return fmt.Errorf("team %s has duplicate runtime name %q", team.Name, runtimeName)
		}
		if _, ok := aliases[runtimeName]; !ok {
			ordered = append(ordered, runtimeName)
		}
		aliases[name] = runtimeName
		aliases[runtimeName] = runtimeName
		return nil
	}

		if len(team.Spec.WorkerMembers) > 0 {
			for _, ref := range team.Spec.WorkerMembers {
				var worker v1beta1.Worker
				if err := r.Get(ctx, client.ObjectKey{Namespace: team.Namespace, Name: ref.Name}, &worker); err != nil {
					return nil, fmt.Errorf("resolve Team %s member Worker %q: %w", team.Name, ref.Name, err)
				}
				if err := add(ref.Name, worker.Spec.EffectiveWorkerName(worker.Name)); err != nil {
					return nil, err
				}
			}
		} else {
			// Compatibility for legacy Teams retained by the upstream schema.
			// When Status.Members is populated (post-reconcile observed state),
			// it is authoritative — its RuntimeName supersedes the spec-derived
			// default (which falls back to Name when WorkerName is unset). Recording
			// from both would double-count the leader, so prefer Status when present.
			statusByMember := make(map[string]v1beta1.TeamMemberStatus, len(team.Status.Members))
			for _, m := range team.Status.Members {
				statusByMember[m.Name] = m
			}
			if team.Spec.Leader.Name != "" {
				runtimeName := team.Spec.Leader.EffectiveWorkerName()
				if sm, ok := statusByMember[team.Spec.Leader.Name]; ok && sm.RuntimeName != "" {
					runtimeName = sm.RuntimeName
				}
				if err := add(team.Spec.Leader.Name, runtimeName); err != nil {
					return nil, err
				}
			}
			for _, worker := range team.Spec.Workers {
				runtimeName := worker.EffectiveWorkerName()
				if sm, ok := statusByMember[worker.Name]; ok && sm.RuntimeName != "" {
					runtimeName = sm.RuntimeName
				}
				if err := add(worker.Name, runtimeName); err != nil {
					return nil, err
				}
			}
			// Record any Status.Members not represented in the spec (e.g. a member
			// whose CR exists but wasn't listed in legacy Spec.Workers) so their
			// runtime names are addressable by proj.Spec.Workers lookups.
			for _, member := range team.Status.Members {
				if _, already := aliases[member.Name]; already {
					continue
				}
				runtimeName := member.RuntimeName
				if runtimeName == "" {
					runtimeName = member.Name
				}
				if err := add(member.Name, runtimeName); err != nil {
					return nil, err
				}
			}
		}

	if len(proj.Spec.Workers) == 0 {
		return ordered, nil
	}
	resolved := make([]string, 0, len(proj.Spec.Workers))
	seen := make(map[string]struct{}, len(proj.Spec.Workers))
	for _, requested := range proj.Spec.Workers {
		runtimeName, ok := aliases[strings.TrimSpace(requested)]
		if !ok {
			return nil, fmt.Errorf("worker %q is not a member of Team %s", requested, team.Name)
		}
		if _, duplicate := seen[runtimeName]; duplicate {
			return nil, fmt.Errorf("worker %q duplicates runtime worker %q", requested, runtimeName)
		}
		seen[runtimeName] = struct{}{}
		resolved = append(resolved, runtimeName)
	}
	return resolved, nil
}

func (r *ProjectReconciler) notifyLeaderOnce(ctx context.Context, proj *v1beta1.Project, team *v1beta1.Team, projectID, manifestKey string) {
	logger := log.FromContext(ctx)
	if existing := proj.Status.ConditionByType(ConditionLeaderNotified); existing != nil && existing.Status == conditionTrue {
		return
	}
	if r.Messenger == nil || team.Status.TeamRoomID == "" {
		return
	}
	body := fmt.Sprintf("New Project manifest available: id=%s, path=%s. Run projectflow(create_project) to start planning.", projectID, manifestKey)
	if err := r.Messenger.SendAdminMessage(ctx, team.Status.TeamRoomID, body); err != nil {
		logger.Error(err, "failed to notify team lead of new project (non-fatal)", "team", team.Name, "project", proj.Name)
		return
	}
	proj.Status.SetCondition(ConditionLeaderNotified, conditionTrue, "Notified", "posted to team room "+team.Status.TeamRoomID)
}

func (r *ProjectReconciler) reconcileCompleted(ctx context.Context, proj *v1beta1.Project, projectID string) (reconcile.Result, error) {
	r.resolveProjectDependencies(ctx, proj)
	if existing := proj.Status.ConditionByType(ConditionDeprovisionPending); existing == nil || existing.Status != conditionTrue {
		proj.Status.SetCondition(ConditionDeprovisionPending, conditionTrue, proj.Status.Phase,
			"project marked "+proj.Status.Phase+"; run provision-worker-gitea.sh --deprovision "+projectID)
	}
	if proj.Status.Phase != "Archived" || r.OSS == nil {
		return reconcile.Result{RequeueAfter: reconcileInterval}, nil
	}
	livePrefix := projectLiveRoot + projectID + "/"
	archivePrefix := projectArchiveRoot + projectID + "/"
	for _, key := range []string{livePrefix + "manifest.json", archivePrefix + "manifest.json"} {
		if err := r.validateManifestOwnership(ctx, proj, key); err != nil {
			proj.Status.SetCondition(ConditionArchiveProjected, conditionFalse, "ManifestOwnershipConflict", err.Error())
			return reconcile.Result{RequeueAfter: reconcileRetryDelay}, err
		}
	}
	if err := r.OSS.MovePrefix(ctx, livePrefix, archivePrefix); err != nil {
		proj.Status.SetCondition(ConditionArchiveProjected, conditionFalse, "MovePrefixFailed", err.Error())
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("archive project prefix: %w", err)
	}
	proj.Status.SetCondition(ConditionArchiveProjected, conditionTrue, "Archived", "all project objects moved to "+archivePrefix)
	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

func (r *ProjectReconciler) reconcileDelete(ctx context.Context, proj *v1beta1.Project) (reconcile.Result, error) {
	projectID := projectStorageKey(proj)
	if errs := kvalidation.IsDNS1123Subdomain(projectID); len(errs) > 0 {
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("refusing project cleanup for invalid storage key %q: %s", projectID, strings.Join(errs, "; "))
	}
	if err := r.ensureNoStorageCollision(ctx, proj, projectID); err != nil {
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, err
	}
	if r.OSS != nil {
		prefixes := []string{projectLiveRoot + projectID + "/", projectArchiveRoot + projectID + "/"}
		for _, prefix := range prefixes {
			if err := r.validateManifestOwnership(ctx, proj, prefix+"manifest.json"); err != nil {
				return reconcile.Result{RequeueAfter: reconcileRetryDelay}, err
			}
		}
		for _, prefix := range prefixes {
			if err := r.OSS.DeletePrefix(ctx, prefix); err != nil {
				// Best-effort cleanup: a transient OSS outage during project
				// deletion must NOT strand the Project CR with a finalizer
				// forever. Log and proceed to remove the finalizer; the
				// prefix is now orphaned (an operator can gc it later) but
				// deletion completes.
				log := ctrl.LoggerFrom(ctx)
				log.Error(err, "non-fatal: failed to delete project storage prefix during finalizer removal", "prefix", prefix)
			}
		}
	}
	base := proj.DeepCopy()
	controllerutil.RemoveFinalizer(proj, finalizerName)
	if err := r.Patch(ctx, proj, client.MergeFrom(base)); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func projectDependencySatisfied(phase string) bool {
	switch phase {
	case "Completed", "Ready", "Archived":
		return true
	default:
		return false
	}
}

func (r *ProjectReconciler) resolveProjectDependencies(ctx context.Context, proj *v1beta1.Project) {
	if len(proj.Spec.DependsOn) == 0 {
		proj.Status.Dependencies = nil
		return
	}
	deps := make([]v1beta1.ProjectDependency, 0, len(proj.Spec.DependsOn))
	for _, depName := range proj.Spec.DependsOn {
		depName = strings.TrimSpace(depName)
		if depName == "" {
			continue
		}
		entry := v1beta1.ProjectDependency{Project: depName}
		if depName == proj.Name {
			entry.Phase = "Invalid"
			deps = append(deps, entry)
			continue
		}
		var dep v1beta1.Project
		if err := r.Get(ctx, client.ObjectKey{Namespace: proj.Namespace, Name: depName}, &dep); err != nil {
			if client.IgnoreNotFound(err) == nil {
				entry.Phase = "Missing"
			}
			deps = append(deps, entry)
			continue
		}
		phase := dep.Status.Phase
		if phase == "" {
			phase = "Pending"
		}
		entry.Phase = phase
		entry.Satisfied = projectDependencySatisfied(phase)
		deps = append(deps, entry)
	}
	proj.Status.Dependencies = deps
}

func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Project{}).
		Watches(&v1beta1.Team{}, handler.EnqueueRequestsFromMapFunc(r.projectsForTeam)).
		Watches(&v1beta1.Worker{}, handler.EnqueueRequestsFromMapFunc(r.projectsForWorker)).
		Watches(&v1beta1.Project{}, handler.EnqueueRequestsFromMapFunc(r.projectsForDependency)).
		Complete(r)
}

func (r *ProjectReconciler) projectsForDependency(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*v1beta1.Project)
	if !ok {
		return nil
	}
	return r.projectRequests(ctx, changed.Namespace, func(p *v1beta1.Project) bool {
		if p.Name == changed.Name {
			return false
		}
		for _, dep := range p.Spec.DependsOn {
			if strings.TrimSpace(dep) == changed.Name {
				return true
			}
		}
		return false
	})
}

func (r *ProjectReconciler) projectsForTeam(ctx context.Context, obj client.Object) []reconcile.Request {
	team, ok := obj.(*v1beta1.Team)
	if !ok {
		return nil
	}
	return r.projectRequests(ctx, team.Namespace, func(p *v1beta1.Project) bool { return p.Spec.Team == team.Name })
}

func (r *ProjectReconciler) projectsForWorker(ctx context.Context, obj client.Object) []reconcile.Request {
	worker, ok := obj.(*v1beta1.Worker)
	if !ok {
		return nil
	}
	var teams v1beta1.TeamList
	if err := r.List(ctx, &teams, client.InNamespace(worker.Namespace)); err != nil {
		return nil
	}
	teamNames := make(map[string]struct{})
	for i := range teams.Items {
		for _, ref := range teams.Items[i].Spec.WorkerMembers {
			if ref.Name == worker.Name {
				teamNames[teams.Items[i].Name] = struct{}{}
			}
		}
	}
	return r.projectRequests(ctx, worker.Namespace, func(p *v1beta1.Project) bool {
		_, ok := teamNames[p.Spec.Team]
		return ok
	})
}

func (r *ProjectReconciler) projectRequests(ctx context.Context, namespace string, include func(*v1beta1.Project) bool) []reconcile.Request {
	var projects v1beta1.ProjectList
	if err := r.List(ctx, &projects, client.InNamespace(namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(projects.Items))
	for i := range projects.Items {
		if include(&projects.Items[i]) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: projects.Items[i].Name}})
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Name < requests[j].Name })
	return requests
}

type projectManifest struct {
	ID              string                `json:"id"`
	Owner           projectManifestOwner  `json:"owner"`
	Team            string                `json:"team"`
	Description     string                `json:"description,omitempty"`
	Repos           []projectManifestRepo `json:"repos"`
	RecordedWorkers []string              `json:"recordedWorkers,omitempty"`
	UpdatedAt       string                `json:"updatedAt"`
}

type projectManifestOwner struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type projectManifestRepo struct {
	URL    string `json:"url"`
	Access string `json:"access"`
	Name   string `json:"name,omitempty"`
}
