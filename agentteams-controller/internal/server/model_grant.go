package server

import (
	"context"
	"encoding/json"
	"net/http"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// --- Model grant enforcement (PR-B1) ---
//
// Two mechanisms, one matching rule:
//   - implicit grants: the worker annotation agentteams.io/model-grants,
//     maintained by L1 model writes only ("create = authorize", §2.1);
//   - effective set per surface: explicit table rows ∪ implicit grants ∪
//     the worker's current value (§2.2). The current value is what makes
//     "lock ≠ unavailable" true: an identical value is always writable.
//
// grantAllows is the single matching predicate used by every write check
// and by no other component: an entry matches when the model is equal and
// at least one side's provider is empty (provider-agnostic row);
// otherwise both provider and model must be equal.

// modelGrantAnnotation is the per-worker implicit grant list. Value: a JSON
// array of {"provider","model"} pairs (provider may be "" = unspecified).
const modelGrantAnnotation = "agentteams.io/model-grants"

// modelGrantMax bounds the implicit grant list. Overflow appends are
// skipped (the list never exceeds the cap); growing beyond it is an
// explicit-table concern, not an annotation concern.
const modelGrantMax = 32

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
// occurrence of a (provider, model) pair wins, and the result never
// exceeds modelGrantMax.
func mergeGrantEntries(groups ...[]v1beta1.ModelAccessEntry) []v1beta1.ModelAccessEntry {
	out := make([]v1beta1.ModelAccessEntry, 0, modelGrantMax)
	seen := make(map[v1beta1.ModelAccessEntry]struct{}, modelGrantMax)
	for _, g := range groups {
		for _, e := range g {
			if len(out) >= modelGrantMax {
				return out
			}
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
func (h *ResourceHandler) appendModelGrant(ctx context.Context, worker *v1beta1.Worker, provider, model string) {
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
	entries []v1beta1.ModelAccessEntry // explicit ∪ implicit (no current value)
	inherit bool                       // the CLI grant's inherit flag
}

// modelGrantSet is one caller's effective grant set for one worker:
// explicit table rows ∪ implicit grants ∪ current values, merged per
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
// resolved table) with the worker's implicit grants and current model
// values into the per-surface effective sets. A dangling table reference
// (scope.Table set, tableSpec nil) degrades to no explicit rows.
func buildModelGrantSet(scope *v1beta1.ModelAccessScope, tableSpec *v1beta1.ModelAccessTableSpec, worker *v1beta1.Worker) *modelGrantSet {
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
	implicit := readModelGrants(worker)

	// The current value only joins the set when it exists: an empty model
	// (or subagent model) is the absence of a value, not a ("","") pair.
	var currentMain []v1beta1.ModelAccessEntry
	if worker.Spec.Model != "" {
		currentMain = []v1beta1.ModelAccessEntry{{Provider: worker.Spec.ModelProvider, Model: worker.Spec.Model}}
	}
	var currentSpawn []v1beta1.ModelAccessEntry
	if worker.Spec.SubagentModel != "" {
		currentSpawn = []v1beta1.ModelAccessEntry{{Provider: worker.Spec.ModelProvider, Model: worker.Spec.SubagentModel}}
	}

	s := &modelGrantSet{
		scope:     scope,
		tableSpec: tableSpec,
		main:      mergeGrantEntries(mainSurfEntries(mainSurf), implicit, currentMain),
		spawn:     mergeGrantEntries(spawnSurfEntries(spawnSurf), implicit, currentSpawn),
		cli:       map[string]modelGrantCLISet{},
	}
	s.spawnInherit = spawnSurf != nil && spawnSurf.Inherit != nil && *spawnSurf.Inherit
	for _, c := range clis {
		if _, dup := s.cli[c.CLI]; dup {
			continue
		}
		s.cli[c.CLI] = modelGrantCLISet{
			entries: mergeGrantEntries(c.Entries, implicit),
			inherit: c.Inherit != nil && *c.Inherit,
		}
	}
	return s
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
// grant set — never a 500; any other Human or table read error is returned
// for the handler to map to 500. Both write paths (worker updates and
// coding-CLI settings) resolve through the same function.
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
	return buildModelGrantSet(scope, tableSpec, worker), nil
}

// checkHumanModelUpdate enforces the grant-based model policy on an L2
// human's worker update (§2.3). Only value-changing writes are checked;
// an identical value is always allowed ("lock ≠ unavailable"). Returns
// (0, "") when the update is allowed.
func (h *ResourceHandler) checkHumanModelUpdate(ctx context.Context, caller *authpkg.CallerIdentity, team *v1beta1.Team, name string, req *UpdateWorkerRequest) (int, string) {
	if req.Model == "" && req.ModelProvider == "" && req.SubagentModel == nil {
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
	if req.ModelProvider != "" && req.Model == "" {
		return http.StatusForbidden, "model_pair_required: provider must be sent with model"
	}
	set, err := h.resolveCallerGrant(ctx, caller, &worker, team.Name, teamMemberRole(team, name))
	if err != nil {
		return http.StatusInternalServerError, "resolve model grants: " + err.Error()
	}
	if req.Model != "" && req.Model != worker.Spec.Model {
		provider := worker.Spec.ModelProvider
		if req.ModelProvider != "" {
			provider = req.ModelProvider
		}
		if !grantAllows(set.main, provider, req.Model) {
			return http.StatusForbidden, "model_not_allowed: " + provider + "/" + req.Model
		}
	}
	if req.SubagentModel != nil && *req.SubagentModel != worker.Spec.SubagentModel {
		if *req.SubagentModel == "" {
			if !set.spawnInherit {
				return http.StatusForbidden, "inherit_not_granted: spawn"
			}
			return 0, ""
		}
		if !grantAllows(set.spawn, worker.Spec.ModelProvider, *req.SubagentModel) {
			return http.StatusForbidden, "model_not_allowed: " + *req.SubagentModel
		}
	}
	return 0, ""
}
