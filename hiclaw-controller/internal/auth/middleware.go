package auth

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/hiclaw/hiclaw-controller/internal/httputil"
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

// resolveResourceTeam looks up the target worker resource's team membership
// via the Team CR cache (single source of truth). Returns "" for non-worker
// kinds or when the worker is standalone.
func (m *Middleware) resolveResourceTeam(ctx context.Context, kind, name string) string {
	if name == "" || m.k8s == nil {
		return ""
	}
	if kind != "worker" {
		return ""
	}
	return LookupWorkerTeam(ctx, m.k8s, m.namespace, name)
}

func (m *Middleware) authenticateAndEnrich(r *http.Request) (*CallerIdentity, bool) {
	token := extractBearerToken(r)
	if token == "" {
		log.Printf("[AUTH] no bearer token in request (path=%s %s)", r.Method, r.URL.Path)
		return nil, false
	}

	clusterID := r.Header.Get("X-AgentTeams-Cluster-ID")

	identity, err := m.authenticator.Authenticate(r.Context(), token, clusterID)
	if err != nil {
		// Log key diagnostic context (clusterID empty == local mode, non-empty == remote mode);
		// only log token length, never the token itself, for security reasons.
		log.Printf("[AUTH] authentication failed: %v (clusterID=%q, path=%s %s, tokenLen=%d)", err, clusterID, r.Method, r.URL.Path, len(token))
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
