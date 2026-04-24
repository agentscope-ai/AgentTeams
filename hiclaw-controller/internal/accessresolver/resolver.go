package accessresolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/credprovider"
)

// Resolver converts the CR-layer v1beta1.AccessEntry declarations on a
// given caller's Worker/Manager CR into the RESOLVED
// credprovider.AccessEntry form accepted by the sidecar.
type Resolver struct {
	client           client.Client
	namespace        string
	defaultBucket    string
	defaultGatewayID string // may be "" when the cluster has no AI Gateway
	prefix           auth.ResourcePrefix
}

// New builds a Resolver. namespace is the controller-namespace where
// Worker and Manager CRs live. defaultBucket is used to resolve
// `bucketRef: workspace`. defaultGatewayID is used to resolve
// `gatewayRef: default`; pass "" when the cluster has no AI Gateway.
// prefix drives the STS session-name output; empty falls back to
// auth.DefaultResourcePrefix ("hiclaw-").
func New(c client.Client, namespace, defaultBucket, defaultGatewayID string, prefix auth.ResourcePrefix) *Resolver {
	return &Resolver{
		client:           c,
		namespace:        namespace,
		defaultBucket:    defaultBucket,
		defaultGatewayID: defaultGatewayID,
		prefix:           prefix.Or(auth.DefaultResourcePrefix),
	}
}

// ResolveForCaller looks up the caller's backing CR, applies defaults
// when spec.accessEntries is empty, expands template variables and
// logical refs, and returns a list of fully-resolved entries ready
// for credprovider.IssueRequest.
//
// The returned sessionName is a canonical identifier the caller can
// forward to Alibaba Cloud AssumeRole as RoleSessionName.
func (r *Resolver) ResolveForCaller(ctx context.Context, caller *auth.CallerIdentity) (sessionName string, entries []credprovider.AccessEntry, err error) {
	if caller == nil {
		return "", nil, errors.New("accessresolver: caller is nil")
	}

	switch caller.Role {
	case auth.RoleWorker, auth.RoleTeamLeader:
		// Team leader always carries caller.Team (enriched via the
		// spec.leader.name field indexer). A team worker carries it
		// via the spec.workerNames indexer. When caller.Team is set
		// we route to the team path so the resolver can pick up the
		// member's AccessEntries on the Team CR and expand
		// ${self.team}. The empty-team branch also covers standalone
		// workers and any enricher-miss corner case.
		if caller.Team != "" {
			return r.resolveTeamMember(ctx, caller.Username, caller.Team)
		}
		return r.resolveWorker(ctx, caller.Username)
	case auth.RoleManager:
		return r.resolveManager(ctx, caller.Username)
	default:
		return "", nil, fmt.Errorf("accessresolver: role %q is not eligible for STS issuance", caller.Role)
	}
}

func (r *Resolver) resolveWorker(ctx context.Context, name string) (string, []credprovider.AccessEntry, error) {
	if name == "" {
		return "", nil, errors.New("accessresolver: empty worker name")
	}
	var w v1beta1.Worker
	err := r.client.Get(ctx, client.ObjectKey{Name: name, Namespace: r.namespace}, &w)
	if err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return "", nil, fmt.Errorf("get worker %q: %w", name, err)
	}
	crEntries := w.Spec.AccessEntries
	if len(crEntries) == 0 {
		crEntries = DefaultEntriesForWorker()
	}
	tmpl := templateCtx{kind: "Worker", name: name, namespace: r.namespace}
	resolved, err := r.resolveEntries(crEntries, tmpl)
	if err != nil {
		return "", nil, fmt.Errorf("worker %q: %w", name, err)
	}
	return r.prefix.WorkerSessionName(name), resolved, nil
}

// resolveTeamMember handles both team leaders and team workers. It
// fetches the Team CR, locates the matching member spec, and applies
// DefaultEntriesForTeamMember when the member omits accessEntries.
//
// When the Team CR cannot be fetched (not found, CRD not installed)
// but caller.Team is populated we still proceed with an empty
// crEntries slice so defaults kick in. This matches the behaviour of
// resolveWorker / resolveManager: a deleted CR shouldn't take down
// STS issuance for a caller whose identity has already been accepted
// by the enricher.
func (r *Resolver) resolveTeamMember(ctx context.Context, name, teamName string) (string, []credprovider.AccessEntry, error) {
	if name == "" {
		return "", nil, errors.New("accessresolver: empty team member name")
	}
	if teamName == "" {
		return "", nil, errors.New("accessresolver: empty team name")
	}

	var team v1beta1.Team
	if err := r.client.Get(ctx, client.ObjectKey{Name: teamName, Namespace: r.namespace}, &team); err != nil {
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return "", nil, fmt.Errorf("get team %q: %w", teamName, err)
		}
	}

	var crEntries []v1beta1.AccessEntry
	kind := "TeamWorker"
	switch {
	case team.Spec.Leader.Name == name && team.Spec.Leader.Name != "":
		crEntries = team.Spec.Leader.AccessEntries
		kind = "TeamLeader"
	default:
		for _, w := range team.Spec.Workers {
			if w.Name == name {
				crEntries = w.AccessEntries
				break
			}
		}
	}
	if len(crEntries) == 0 {
		crEntries = DefaultEntriesForTeamMember()
	}

	tmpl := templateCtx{kind: kind, name: name, namespace: r.namespace, team: teamName}
	resolved, err := r.resolveEntries(crEntries, tmpl)
	if err != nil {
		return "", nil, fmt.Errorf("team member %q (team %q): %w", name, teamName, err)
	}
	// Team members — both leaders and workers — share the Worker
	// session-name shape because their ServiceAccount name on the
	// pod is still hiclaw-worker-<name> (see auth.ResourcePrefix.SAName).
	return r.prefix.WorkerSessionName(name), resolved, nil
}

func (r *Resolver) resolveManager(ctx context.Context, name string) (string, []credprovider.AccessEntry, error) {
	if name == "" {
		name = "manager"
	}
	var m v1beta1.Manager
	err := r.client.Get(ctx, client.ObjectKey{Name: name, Namespace: r.namespace}, &m)
	if err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return "", nil, fmt.Errorf("get manager %q: %w", name, err)
	}
	crEntries := m.Spec.AccessEntries
	if len(crEntries) == 0 {
		crEntries = DefaultEntriesForManager()
	}
	tmpl := templateCtx{kind: "Manager", name: name, namespace: r.namespace}
	resolved, err := r.resolveEntries(crEntries, tmpl)
	if err != nil {
		return "", nil, fmt.Errorf("manager %q: %w", name, err)
	}
	return r.prefix.ManagerSessionName(name), resolved, nil
}

type templateCtx struct {
	kind      string
	name      string
	namespace string
	// team is non-empty only for team leaders and team workers; empty
	// for standalone workers and managers. When expand encounters
	// ${self.team} in a non-team context it will be replaced with the
	// empty string.
	team string
}

func (t templateCtx) expand(s string) string {
	s = strings.ReplaceAll(s, "${self.name}", t.name)
	s = strings.ReplaceAll(s, "${self.kind}", t.kind)
	s = strings.ReplaceAll(s, "${self.namespace}", t.namespace)
	s = strings.ReplaceAll(s, "${self.team}", t.team)
	return s
}

func (r *Resolver) resolveEntries(in []v1beta1.AccessEntry, tmpl templateCtx) ([]credprovider.AccessEntry, error) {
	out := make([]credprovider.AccessEntry, 0, len(in))
	for i, e := range in {
		switch e.Service {
		case credprovider.ServiceObjectStorage:
			entry, err := r.resolveObjectStorage(e, tmpl)
			if err != nil {
				return nil, fmt.Errorf("entry[%d]: %w", i, err)
			}
			out = append(out, entry)
		case credprovider.ServiceAIGateway:
			entry, err := r.resolveAIGateway(e, tmpl)
			if err != nil {
				return nil, fmt.Errorf("entry[%d]: %w", i, err)
			}
			out = append(out, entry)
		case credprovider.ServiceAIRegistry:
			entry, err := r.resolveAIRegistry(e, tmpl)
			if err != nil {
				return nil, fmt.Errorf("entry[%d]: %w", i, err)
			}
			out = append(out, entry)
		case "":
			return nil, fmt.Errorf("entry[%d]: missing service", i)
		default:
			return nil, fmt.Errorf("entry[%d]: unsupported service %q", i, e.Service)
		}
	}
	return out, nil
}

// objectStorageScope is the typed view of a CR-layer scope blob for
// service=object-storage. Keep field names in sync with the CRD schema
// description.
type objectStorageScope struct {
	BucketRef string   `json:"bucketRef,omitempty"`
	Bucket    string   `json:"bucket,omitempty"`
	Prefixes  []string `json:"prefixes,omitempty"`
}

func (r *Resolver) resolveObjectStorage(e v1beta1.AccessEntry, tmpl templateCtx) (credprovider.AccessEntry, error) {
	var s objectStorageScope
	if err := unmarshalScope(e.Scope, &s); err != nil {
		return credprovider.AccessEntry{}, fmt.Errorf("object-storage: %w", err)
	}

	bucket := strings.TrimSpace(s.Bucket)
	if bucket == "" {
		switch s.BucketRef {
		case "", "workspace":
			if r.defaultBucket == "" {
				return credprovider.AccessEntry{}, errors.New("object-storage: bucketRef=workspace but controller has no OSS bucket configured")
			}
			bucket = r.defaultBucket
		default:
			return credprovider.AccessEntry{}, fmt.Errorf("object-storage: unknown bucketRef %q", s.BucketRef)
		}
	}

	prefixes := make([]string, 0, len(s.Prefixes))
	for _, p := range s.Prefixes {
		prefixes = append(prefixes, tmpl.expand(p))
	}
	if len(prefixes) == 0 {
		return credprovider.AccessEntry{}, errors.New("object-storage: prefixes is empty")
	}

	return credprovider.AccessEntry{
		Service:     credprovider.ServiceObjectStorage,
		Permissions: copyPermissions(e.Permissions),
		Scope: credprovider.AccessScope{
			Bucket:   bucket,
			Prefixes: prefixes,
		},
	}, nil
}

type aiGatewayScope struct {
	GatewayRef string   `json:"gatewayRef,omitempty"`
	GatewayID  string   `json:"gatewayId,omitempty"`
	Resources  []string `json:"resources,omitempty"`
}

func (r *Resolver) resolveAIGateway(e v1beta1.AccessEntry, tmpl templateCtx) (credprovider.AccessEntry, error) {
	var s aiGatewayScope
	if err := unmarshalScope(e.Scope, &s); err != nil {
		return credprovider.AccessEntry{}, fmt.Errorf("ai-gateway: %w", err)
	}

	gatewayID := strings.TrimSpace(s.GatewayID)
	if gatewayID == "" {
		switch s.GatewayRef {
		case "", "default":
			if r.defaultGatewayID == "" {
				return credprovider.AccessEntry{}, errors.New("ai-gateway: gatewayRef=default but controller has no AI Gateway configured")
			}
			gatewayID = r.defaultGatewayID
		default:
			return credprovider.AccessEntry{}, fmt.Errorf("ai-gateway: unknown gatewayRef %q", s.GatewayRef)
		}
	}

	resources := make([]string, 0, len(s.Resources))
	for _, res := range s.Resources {
		resources = append(resources, tmpl.expand(res))
	}
	if len(resources) == 0 {
		resources = []string{"*"}
	}

	return credprovider.AccessEntry{
		Service:     credprovider.ServiceAIGateway,
		Permissions: copyPermissions(e.Permissions),
		Scope: credprovider.AccessScope{
			GatewayID: gatewayID,
			Resources: resources,
		},
	}, nil
}

type aiRegistryScope struct {
	NamespaceID string   `json:"namespaceId,omitempty"`
	Resources   []string `json:"resources,omitempty"`
}

func (r *Resolver) resolveAIRegistry(e v1beta1.AccessEntry, tmpl templateCtx) (credprovider.AccessEntry, error) {
	var s aiRegistryScope
	if err := unmarshalScope(e.Scope, &s); err != nil {
		return credprovider.AccessEntry{}, fmt.Errorf("ai-registry: %w", err)
	}

	namespaceID := strings.TrimSpace(tmpl.expand(s.NamespaceID))
	if namespaceID == "" {
		return credprovider.AccessEntry{}, errors.New("ai-registry: namespaceId is required")
	}

	resources := make([]string, 0, len(s.Resources))
	for _, res := range s.Resources {
		resources = append(resources, tmpl.expand(res))
	}
	if len(resources) == 0 {
		resources = []string{"agentSpec/*", "skill/*"}
	}

	return credprovider.AccessEntry{
		Service:     credprovider.ServiceAIRegistry,
		Permissions: copyPermissions(e.Permissions),
		Scope: credprovider.AccessScope{
			NamespaceID: namespaceID,
			Resources:   resources,
		},
	}, nil
}

func unmarshalScope(raw *apiextensionsv1.JSON, dst any) error {
	if raw == nil || len(raw.Raw) == 0 {
		return errors.New("scope is empty")
	}
	if err := json.Unmarshal(raw.Raw, dst); err != nil {
		return fmt.Errorf("decode scope: %w", err)
	}
	return nil
}

// copyPermissions defensively copies the CR-layer permissions list so
// the resolver never hands the sidecar a slice backed by user-owned
// CR memory.
func copyPermissions(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// marshalJSONDeterministic marshals m with sorted keys so that default
// entries produce stable output (useful in tests).
func marshalJSONDeterministic(m map[string]any) ([]byte, error) {
	// json.Marshal on a map sorts keys alphabetically, but nested
	// maps retain insertion order. For our fixed default shapes this
	// is sufficient; we add explicit sorting for any string slice
	// fields that might otherwise differ across Go runtimes.
	for _, v := range m {
		if ss, ok := v.([]string); ok {
			sort.Strings(ss)
		}
	}
	return json.Marshal(m)
}
