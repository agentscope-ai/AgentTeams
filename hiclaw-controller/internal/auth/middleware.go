package auth

import (
	"context"
	"log"
	"net/http"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/httputil"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type contextKey string

const callerKey contextKey = "caller"

// CallerFromContext extracts the CallerIdentity from the request context.
func CallerFromContext(ctx context.Context) *CallerIdentity {
	if v := ctx.Value(callerKey); v != nil {
		return v.(*CallerIdentity)
	}
	return nil
}

// CallerKeyForTest returns the context key for injecting CallerIdentity in tests.
func CallerKeyForTest() contextKey {
	return callerKey
}

// Middleware provides HTTP authentication and authorization middleware.
type Middleware struct {
	authenticator Authenticator
	enricher      IdentityEnricher
	authorizer    *Authorizer
	k8s           client.Client
	namespace     string
}

// NewMiddleware creates an auth Middleware with the full auth chain.
func NewMiddleware(auth Authenticator, enricher IdentityEnricher, authz *Authorizer, k8s client.Client, namespace string) *Middleware {
	return &Middleware{
		authenticator: auth,
		enricher:      enricher,
		authorizer:    authz,
		k8s:           k8s,
		namespace:     namespace,
	}
}

// Authenticate returns middleware that authenticates the caller and places
// the CallerIdentity in the request context. No authorization is performed.
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.authenticator == nil {
			next.ServeHTTP(w, r)
			return
		}

		identity, ok := m.authenticateAndEnrich(r)
		if !ok {
			httputil.WriteError(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}

		ctx := context.WithValue(r.Context(), callerKey, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ResourceNameFunc extracts the target resource name from an HTTP request.
type ResourceNameFunc func(r *http.Request) string

// NameFromPath returns a ResourceNameFunc that reads the "name" path parameter.
func NameFromPath(r *http.Request) string {
	return r.PathValue("name")
}

// RequireAuthz returns middleware that authenticates, enriches, resolves the
// target resource's team, and checks authorization against the permission matrix.
func (m *Middleware) RequireAuthz(action Action, kind string, nameFn ResourceNameFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m.authenticator == nil {
				next.ServeHTTP(w, r)
				return
			}

			identity, ok := m.authenticateAndEnrich(r)
			if !ok {
				httputil.WriteError(w, http.StatusUnauthorized, "invalid or missing bearer token")
				return
			}

			resourceName := ""
			if nameFn != nil {
				resourceName = nameFn(r)
			}

			resourceTeam := m.resolveResourceTeam(r.Context(), kind, resourceName)

			authzReq := AuthzRequest{
				Action:       action,
				ResourceKind: kind,
				ResourceName: resourceName,
				ResourceTeam: resourceTeam,
			}

			if err := m.authorizer.Authorize(identity, authzReq); err != nil {
				httputil.WriteError(w, http.StatusForbidden, err.Error())
				return
			}

			ctx := context.WithValue(r.Context(), callerKey, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveResourceTeam resolves the target worker's team from the
// authoritative source. Post team-refactor, team members (leader and
// worker) have no Worker CR at all — they exist only as entries in a
// Team CR's spec — so a Team CR reverse lookup (by leader/worker name,
// via the same field indexers CREnricher uses) is authoritative and is
// tried first. A standalone Worker CR's "hiclaw.io/team" annotation is
// consulted only as a defensive fallback for legacy/pre-refactor
// resources (see isTeamMemberWorker in resource_handler.go); standalone
// Workers created via the current /workers API never carry that
// annotation (CreateWorker rejects team/role fields), so this fallback
// normally yields "".
//
// Returns "" when the resource's team is truly unknown/absent — callers
// (requireSameTeam) MUST treat "" as "deny for team-leaders", not
// "allow", since an unresolved team is not proof the resource is
// team-less.
func (m *Middleware) resolveResourceTeam(ctx context.Context, kind, name string) string {
	if name == "" || m.k8s == nil {
		return ""
	}
	if kind != "worker" {
		return ""
	}

	// 1. Authoritative: reverse lookup against Team CRs (leader or worker
	// member name). This is the only linkage for post-refactor team members.
	if team, ok, err := m.lookupTeamByField(ctx, teamLeaderNameField, name); err == nil && ok {
		return team.Name
	}
	if team, ok, err := m.lookupTeamByField(ctx, teamWorkerNameField, name); err == nil && ok {
		return team.Name
	}

	// 2. Defensive fallback: legacy annotation on a standalone Worker CR.
	var worker v1beta1.Worker
	key := client.ObjectKey{Name: name, Namespace: m.namespace}
	if err := m.k8s.Get(ctx, key, &worker); err != nil {
		return ""
	}
	return worker.Annotations["hiclaw.io/team"]
}

// lookupTeamByField mirrors CREnricher.lookupTeamByField (duplicated rather
// than shared to keep resolveResourceTeam self-contained within middleware.go;
// both use the same teamLeaderNameField/teamWorkerNameField indexers).
func (m *Middleware) lookupTeamByField(ctx context.Context, field, value string) (*v1beta1.Team, bool, error) {
	var list v1beta1.TeamList
	if err := m.k8s.List(ctx, &list,
		client.InNamespace(m.namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(field, value)},
	); err != nil {
		return nil, false, err
	}
	if len(list.Items) == 0 {
		return nil, false, nil
	}
	return &list.Items[0], true, nil
}

func (m *Middleware) authenticateAndEnrich(r *http.Request) (*CallerIdentity, bool) {
	token := extractBearerToken(r)
	if token == "" {
		return nil, false
	}

	identity, err := m.authenticator.Authenticate(r.Context(), token)
	if err != nil {
		log.Printf("[AUTH] authentication failed: %v", err)
		return nil, false
	}

	if m.enricher != nil {
		if err := m.enricher.EnrichIdentity(r.Context(), identity); err != nil {
			log.Printf("[AUTH] identity enrichment failed for %s: %v", identity.Username, err)
		}
	}

	return identity, true
}

func extractBearerToken(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return ""
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == authHeader {
		return ""
	}
	return token
}
