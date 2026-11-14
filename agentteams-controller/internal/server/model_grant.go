package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// --- Model grant enforcement (PR-B1) ---
//
// Explicit caller grants plus current values authorize selections. The worker's
// L1-issued annotation records history only: removing an explicit grant must
// not leave a historical selection usable through that annotation.
// The current value stays available, so revocation never stops a running model.
// modelGrantMax bounds annotation history; validated explicit rows are not capped
// by it. The preview and write paths share the same effective-set builder.

// modelGrantAnnotation is the per-worker implicit grant list. Value: a JSON
// array of {"provider","model"} pairs (provider may be "" = unspecified).
const modelGrantAnnotation = "agentteams.io/model-grants"

// modelGrantMax bounds the implicit grant list (the worker annotation)
// ONLY — never the union of explicit rows and current values. The L1
// append path keeps the list at the cap; an annotation that exceeds it
// (only possible by writing the annotation directly) is rejected by
// checkImplicitGrantCap with the count in the message — never silently
// truncated. Growing past the cap is an explicit-table concern (200 rows
// per surface), not an annotation concern.
const modelGrantMax = 32

// errModelGrantOverflow marks a worker whose implicit grant annotation
// exceeds modelGrantMax. Error code: model_grant_overflow.
var errModelGrantOverflow = errors.New("model_grant_overflow")

// checkImplicitGrantCap is the single size-boundary check for the
// implicit grant list: modelGrantMax (32) applies to the annotation
// only. The explicit table rows (up to 200 per surface, validated at
// write time) and the worker's current value are legitimate data and
// are never truncated. Both the preview (GET /api/v1/model-access) and
// the write checks run this same check through buildModelGrantSet, so
// the two paths can never disagree on the boundary. An over-cap
// annotation fails closed — the count stays in the message, no row is
// silently dropped.
func checkImplicitGrantCap(worker *v1beta1.Worker) error {
	if n := len(readModelGrants(worker)); n > modelGrantMax {
		return fmt.Errorf("%w: worker %q carries %d implicit model grants, the annotation cap is %d; reduce the %s annotation",
			errModelGrantOverflow, worker.Name, n, modelGrantMax, modelGrantAnnotation)
	}
	return nil
}

// grantAllows reports whether any entry admits (provider, model) under the
// single matching rule above.
func grantAllows(entries []v1beta1.ModelAccessEntry, provider, model string) bool {
	for _, e := range entries {
		if e.Model != model {
			continue
		}
		if e.Provider == "" || provider == "" || e.Provider == provider {
			return true
		}
	}
	return false
}

// mergeGrantEntries unions the given entry groups in the given order
// (explicit table rows, implicit grants, current value): the first
// occurrence of a (provider, model) pair wins. The union is never
// truncated — explicit rows (validated, up to 200 per surface) and the
// current value are legitimate CRD data and must stay fully writable.
// modelGrantMax (32) bounds the implicit group only, and that bound is
// enforced upstream in checkImplicitGrantCap, which both the preview and
// the write paths run before merging.
func mergeGrantEntries(groups ...[]v1beta1.ModelAccessEntry) []v1beta1.ModelAccessEntry {
	out := []v1beta1.ModelAccessEntry{}
	seen := make(map[v1beta1.ModelAccessEntry]struct{})
	for _, g := range groups {
		for _, e := range g {
			if _, dup := seen[e]; dup {
				continue
			}
			seen[e] = struct{}{}
			out = append(out, e)
		}
	}
	return out
}

// readModelGrants parses the worker's implicit grant annotation. Malformed
// JSON degrades to an empty list: an unreadable grant list grants nothing
// and never breaks the request.
func readModelGrants(worker *v1beta1.Worker) []v1beta1.ModelAccessEntry {
	raw, ok := worker.Annotations[modelGrantAnnotation]
	if !ok || raw == "" {
		return nil
	}
	var entries []v1beta1.ModelAccessEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	return entries
}

// appendModelGrant adds (provider, model) to the worker's implicit grants,
// deduplicated by (provider, model) and capped at modelGrantMax. The
// mutation happens on the in-memory object before the Create/Update call —
// a marshal failure is logged and swallowed so the annotation can never
// block the underlying write.
// modelGrantDefaultProvider is the provider id the controller materialises for
// the cluster gateway; a worker with an empty ModelProvider is served by it.
// Pinning the current-value entry to it keeps "same model, different provider"
// from matching through the empty-provider wildcard.
const modelGrantDefaultProvider = "agentteams-gateway"

func modelGrantProviderOr(provider string) string {
	if provider == "" {
		return modelGrantDefaultProvider
	}
	return provider
}

func (h *ResourceHandler) appendModelGrant(ctx context.Context, worker *v1beta1.Worker, provider, model string) {
	appendImplicitModelGrant(ctx, worker, provider, model)
}

// appendImplicitModelGrant is the handler-independent form used by the
// internal provision route, which has no public claims context but
// must preserve the create-grant behaviour: an admin/manager create carries
// the chosen model into the worker's implicit grant list before the object
// exists in the controller.
func appendImplicitModelGrant(ctx context.Context, worker *v1beta1.Worker, provider, model string) {
	if model == "" {
		return
	}
	entries := readModelGrants(worker)
	for _, e := range entries {
		if e.Provider == provider && e.Model == model {
			return
		}
	}
	if len(entries) >= modelGrantMax {
		log.FromContext(ctx).Info("model grant list at cap; skipping implicit append",
			"worker", worker.Name, "cap", modelGrantMax)
		return
	}
	entries = append(entries, v1beta1.ModelAccessEntry{Provider: provider, Model: model})
	data, err := json.Marshal(entries)
	if err != nil {
		log.FromContext(ctx).Error(err, "encode model grants annotation; skipping", "worker", worker.Name)
		return
	}
	if worker.Annotations == nil {
		worker.Annotations = map[string]string{}
	}
	worker.Annotations[modelGrantAnnotation] = string(data)
}

// modelGrantCLISet is one coding CLI's effective grant set.
type modelGrantCLISet struct {
	entries []v1beta1.ModelAccessEntry // explicit (CLI unchanged-value checks are separate)
	inherit bool                       // the CLI grant's inherit flag
}

// modelGrantSet is one caller's effective grant set for one worker:
// explicit table rows ∪ current values, merged per
// surface (§2.2). scope/tableSpec are carried for authpkg.Resolve so the
// caller-side capabilities (canEdit/inherit/reason) stay evaluator-driven.
type modelGrantSet struct {
	scope        *v1beta1.ModelAccessScope
	tableSpec    *v1beta1.ModelAccessTableSpec
	main         []v1beta1.ModelAccessEntry // explicit ∪ implicit ∪ current model
	spawn        []v1beta1.ModelAccessEntry // explicit ∪ implicit ∪ current subagent
	spawnInherit bool
	cli          map[string]modelGrantCLISet
}

func (s *modelGrantSet) cliSet(id string) modelGrantCLISet {
	return s.cli[id]
}

// buildModelGrantSet merges the explicit grant (the selected scope plus the
// resolved table) with the worker's current model
// values into the per-surface effective sets. A dangling table reference
// (scope.Table set, tableSpec nil) degrades to no explicit rows. It
// returns an error only when the implicit annotation exceeds
// modelGrantMax (checkImplicitGrantCap, error code
// model_grant_overflow): the caller — the preview endpoint or a write
// check — must reject the request instead of serving a truncated set.
func buildModelGrantSet(scope *v1beta1.ModelAccessScope, tableSpec *v1beta1.ModelAccessTableSpec, worker *v1beta1.Worker) (*modelGrantSet, error) {
	if err := checkImplicitGrantCap(worker); err != nil {
		return nil, err
	}
	var (
		mainSurf  *v1beta1.ModelAccessSurface
		spawnSurf *v1beta1.ModelAccessSurface
		clis      []v1beta1.ModelAccessCli
	)
	if scope != nil {
		if scope.Table != "" && tableSpec != nil {
			mainSurf = tableSpec.Main
			spawnSurf = tableSpec.Spawn
			clis = tableSpec.CodingCli
		} else if scope.Table == "" {
			mainSurf = scope.Main
			spawnSurf = scope.Spawn
			clis = scope.CodingCli
		}
	}

	// The current value only joins the set when it exists: an empty model
	// (or subagent model) is the absence of a value, not a ("","") pair.
	var currentMain []v1beta1.ModelAccessEntry
	if worker.Spec.Model != "" {
		currentMain = []v1beta1.ModelAccessEntry{{Provider: modelGrantProviderOr(worker.Spec.ModelProvider), Model: worker.Spec.Model}}
	}
	var currentSpawn []v1beta1.ModelAccessEntry
	if worker.Spec.SubagentModel != "" {
		currentSpawn = []v1beta1.ModelAccessEntry{{Provider: modelGrantProviderOr(worker.Spec.ModelProvider), Model: worker.Spec.SubagentModel}}
	}

	s := &modelGrantSet{
		scope:     scope,
		tableSpec: tableSpec,
		main:      mergeGrantEntries(mainSurfEntries(mainSurf), currentMain),
		spawn:     mergeGrantEntries(spawnSurfEntries(spawnSurf), currentSpawn),
		cli:       map[string]modelGrantCLISet{},
	}
	s.spawnInherit = spawnSurf != nil && spawnSurf.Inherit != nil && *spawnSurf.Inherit
	for _, c := range clis {
		if _, dup := s.cli[c.CLI]; dup {
			continue
		}
		s.cli[c.CLI] = modelGrantCLISet{
			entries: mergeGrantEntries(c.Entries),
			inherit: c.Inherit != nil && *c.Inherit,
		}
	}
	return s, nil
}

func mainSurfEntries(s *v1beta1.ModelAccessSurface) []v1beta1.ModelAccessEntry {
	if s == nil {
		return nil
	}
	return s.Entries
}

func spawnSurfEntries(s *v1beta1.ModelAccessSurface) []v1beta1.ModelAccessEntry {
	if s == nil {
		return nil
	}
	return s.Entries
}

// resolveCallerGrant resolves the caller's effective grant set for the
// worker (§4.1). A missing Human CR (NotFound) fails closed to an empty
// grant set — never a 500; an over-cap implicit annotation surfaces as
// errModelGrantOverflow (model_grant_overflow) for the handler to map to
// 409; any other Human or table read error is returned for the handler to
// map to 500. Both write paths (worker updates and coding-CLI settings)
// resolve through the same function.
func (h *ResourceHandler) resolveCallerGrant(ctx context.Context, caller *authpkg.CallerIdentity, worker *v1beta1.Worker, team, role string) (*modelGrantSet, error) {
	return resolveCallerGrant(ctx, h.client, h.namespace, caller, worker, team, role)
}

func resolveCallerGrant(ctx context.Context, c client.Client, namespace string, caller *authpkg.CallerIdentity, worker *v1beta1.Worker, team, role string) (*modelGrantSet, error) {
	var human v1beta1.Human
	err := c.Get(ctx, client.ObjectKey{Name: caller.Username, Namespace: namespace}, &human)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		human = v1beta1.Human{}
	}
	scope := authpkg.EffectiveScope(human.Spec.ModelAccess, role)
	var tableSpec *v1beta1.ModelAccessTableSpec
	if scope != nil && scope.Table != "" {
		var table v1beta1.ModelAccessTable
		if terr := c.Get(ctx, client.ObjectKey{Name: scope.Table, Namespace: namespace}, &table); terr != nil {
			if !apierrors.IsNotFound(terr) {
				return nil, terr
			}
			// Dangling table reference: fail closed (no grant rows).
		} else {
			tableSpec = &table.Spec
		}
	}
	return buildModelGrantSet(scope, tableSpec, worker)
}

// checkHumanModelUpdate enforces the grant-based model policy on an L2
// human's worker update (§2.3). Only value-changing writes are checked;
// an identical value is always allowed ("lock ≠ unavailable"). Returns
// (0, "") when the update is allowed.
func (h *ResourceHandler) checkHumanModelUpdate(ctx context.Context, caller *authpkg.CallerIdentity, team *v1beta1.Team, name string, req *UpdateWorkerRequest) (int, string) {
	if req.Model == "" && req.ModelProvider == nil && req.SubagentModel == nil {
		return 0, ""
	}
	var worker v1beta1.Worker
	if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			return http.StatusNotFound, "worker: not found"
		}
		return http.StatusInternalServerError, "get worker: " + err.Error()
	}
	// Pair rule: a provider without a model is never a valid update.
	if req.ModelProvider != nil && req.Model == "" {
		return http.StatusForbidden, "model_pair_required: provider must be sent with model"
	}
	set, err := h.resolveCallerGrant(ctx, caller, &worker, team.Name, teamMemberRole(team, name))
	if err != nil {
		// An over-cap implicit annotation is worker state to fix (409),
		// not a server bug: the count stays in the message.
		if errors.Is(err, errModelGrantOverflow) {
			return http.StatusConflict, err.Error()
		}
		return http.StatusInternalServerError, "resolve model grants: " + err.Error()
	}
	// A change of EITHER half of the pair is a pair write: a same-name model
	// with a different provider must not slip past the grant.
	modelChanged := req.Model != "" && req.Model != worker.Spec.Model
	providerChanged := req.ModelProvider != nil && *req.ModelProvider != worker.Spec.ModelProvider
	if modelChanged || providerChanged {
		provider := worker.Spec.ModelProvider
		if req.ModelProvider != nil {
			provider = *req.ModelProvider
		}
		model := worker.Spec.Model
		if req.Model != "" {
			model = req.Model
		}
		if !grantAllows(set.main, modelGrantProviderOr(provider), model) {
			return http.StatusForbidden, "model_not_allowed: " + provider + "/" + model
		}
	}
	spawn := worker.Spec.SubagentModel
	if req.SubagentModel != nil {
		spawn = *req.SubagentModel
	}
	if spawn != worker.Spec.SubagentModel || (providerChanged && spawn != "") {
		if spawn == "" {
			if !set.spawnInherit {
				return http.StatusForbidden, "inherit_not_granted: spawn"
			}
			return 0, ""
		}
		provider := worker.Spec.ModelProvider
		if req.ModelProvider != nil {
			provider = *req.ModelProvider
		}
		if !grantAllows(set.spawn, modelGrantProviderOr(provider), spawn) {
			return http.StatusForbidden, "model_not_allowed: " + spawn
		}
	}
	return 0, ""
}

// writeGrantResolutionError renders a grant-resolution error on the
// preview path: an over-cap implicit annotation is worker state to fix
// (409, count in the message) — never a 500; any other error keeps the
// usual K8s mapping.
func writeGrantResolutionError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, errModelGrantOverflow) {
		httputil.WriteError(w, http.StatusConflict, err.Error())
		return
	}
	writeK8sError(w, op, err)
}
