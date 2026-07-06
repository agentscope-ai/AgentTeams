package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/edgebootstrap"
	"github.com/hiclaw/hiclaw-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EdgeTokenRequest is the body of POST /api/v1/edge/token.
type EdgeTokenRequest struct {
	JWTToken string `json:"jwtToken"`
}

// EdgeTokenResponse is the JSON body returned on a successful token exchange.
type EdgeTokenResponse struct {
	Token              string `json:"token"`
	ExpiresAt          string `json:"expiresAt,omitempty"`
	JWTToken           string `json:"jwtToken,omitempty"`
	JWTExpiresAt       string `json:"jwtExpiresAt,omitempty"`
	WorkerName         string `json:"workerName"`
	WorkerResourceName string `json:"workerResourceName"`
	RuntimeName        string `json:"runtimeName,omitempty"`
	TeamName           string `json:"teamName,omitempty"`
}

// EdgeProvisioner is the narrow surface EdgeHandler needs from the Provisioner.
// Defined locally so the handler can be tested with a fake without dragging in
// the full WorkerProvisioner interface.
type EdgeProvisioner interface {
	EnsureServiceAccount(ctx context.Context, workerName string) error
	DeleteServiceAccount(ctx context.Context, workerName string) error
	RequestSAToken(ctx context.Context, workerName string) (string, time.Time, error)
}

type EdgeBootstrapSigner interface {
	Verify(ctx context.Context, token string, now time.Time) (*edgebootstrap.Claims, error)
	Sign(ctx context.Context, workerUUID string, ttl time.Duration, now time.Time) (*edgebootstrap.SignedToken, error)
}

// AuthCacheInvalidator clears TokenReview authentication cache after
// ServiceAccount deletion invalidates previously issued tokens.
type AuthCacheInvalidator interface {
	InvalidateCache()
}

// EdgeHandler serves the unauthenticated POST /api/v1/edge/token endpoint
// used by Edge runtimes to exchange their bootstrap JWT for a short-lived
// ServiceAccount token.
type EdgeHandler struct {
	client         client.Client
	provisioner    EdgeProvisioner
	signer         EdgeBootstrapSigner
	authCache      AuthCacheInvalidator
	namespace      string
	controllerName string
}

// NewEdgeHandler constructs an EdgeHandler with the given dependencies.
func NewEdgeHandler(c client.Client, p EdgeProvisioner, signer EdgeBootstrapSigner, namespace, controllerName string, authCache AuthCacheInvalidator) *EdgeHandler {
	return &EdgeHandler{
		client:         c,
		provisioner:    p,
		signer:         signer,
		authCache:      authCache,
		namespace:      namespace,
		controllerName: edgebootstrap.NormalizeControllerName(controllerName),
	}
}

// ExchangeToken handles POST /api/v1/edge/token.
//
// The endpoint is unauthenticated: callers prove identity with a signed JWT
// issued from the controller namespace signing Secret. The handler verifies
// the JWT, resolves its workerUuid claim to a single Edge Worker, issues a
// short-lived SA token, and returns a refreshed JWT.
func (h *EdgeHandler) ExchangeToken(w http.ResponseWriter, r *http.Request) {
	log.Printf("[INFO] edge token exchange request from %s", r.RemoteAddr)

	var req EdgeTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.JWTToken == "" {
		httputil.WriteError(w, http.StatusBadRequest, "jwtToken is required")
		return
	}

	ctx := r.Context()
	now := time.Now().UTC()
	if h.signer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "edge bootstrap signing secret unavailable")
		return
	}
	claims, err := h.signer.Verify(ctx, req.JWTToken, now)
	if err != nil {
		if errors.Is(err, edgebootstrap.ErrUnavailable) {
			log.Printf("[ERROR] edge token exchange: signing secret unavailable: %v", err)
			httputil.WriteError(w, http.StatusServiceUnavailable, "edge bootstrap signing secret unavailable")
			return
		}
		httputil.WriteError(w, http.StatusUnauthorized, "invalid jwt token")
		return
	}

	// Find the Worker carrying this UUID, scoped to the controller instance
	// to avoid cross-tenant collisions in shared namespaces.
	var workers v1beta1.WorkerList
	listOpts := []client.ListOption{
		client.InNamespace(h.namespace),
		client.MatchingLabels{
			v1beta1.LabelWorkerEdgeUUID: claims.WorkerUUID,
			v1beta1.LabelController:     h.controllerName,
		},
	}
	if err := h.client.List(ctx, &workers, listOpts...); err != nil {
		log.Printf("[ERROR] edge token exchange: list workers by uuid: %v", err)
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list workers: "+err.Error())
		return
	}

	switch len(workers.Items) {
	case 0:
		httputil.WriteError(w, http.StatusNotFound, "no worker found for the given uuid")
		return
	case 1:
		// expected
	default:
		log.Printf("[WARN] edge token exchange: %d workers match uuid", len(workers.Items))
		httputil.WriteError(w, http.StatusConflict, "multiple workers found for the given uuid")
		return
	}

	worker := &workers.Items[0]
	workerName := worker.Spec.EffectiveWorkerName(worker.Name)

	// Only Edge-mode workers may exchange UUIDs for SA tokens.
	deployMode := ""
	if worker.Spec.DeployMode != nil {
		deployMode = *worker.Spec.DeployMode
	}
	if deployMode != v1beta1.DeployModeEdge {
		httputil.WriteError(w, http.StatusForbidden, "worker deploy mode is not Edge")
		return
	}

	appliedUUID := ""
	if worker.Annotations != nil {
		appliedUUID = worker.Annotations[v1beta1.AnnotationEdgeAppliedUUID]
	}
	if appliedUUID != "" && appliedUUID != claims.WorkerUUID {
		if err := h.provisioner.DeleteServiceAccount(ctx, worker.Name); err != nil {
			log.Printf("[ERROR] edge token exchange: delete rotated SA for %s: %v", worker.Name, err)
			httputil.WriteError(w, http.StatusInternalServerError, "failed to rotate token: "+err.Error())
			return
		}
		if h.authCache != nil {
			h.authCache.InvalidateCache()
		}
	}

	// Ensure the ServiceAccount exists, then mint a long-lived token.
	if err := h.provisioner.EnsureServiceAccount(ctx, worker.Name); err != nil {
		log.Printf("[ERROR] edge token exchange: ensure SA for %s: %v", worker.Name, err)
		httputil.WriteError(w, http.StatusInternalServerError, "failed to issue token: "+err.Error())
		return
	}
	token, expiresAt, err := h.provisioner.RequestSAToken(ctx, worker.Name)
	if err != nil {
		log.Printf("[ERROR] edge token exchange: request SA token for %s: %v", worker.Name, err)
		httputil.WriteError(w, http.StatusInternalServerError, "failed to issue token: "+err.Error())
		return
	}
	refreshedJWT, err := h.signer.Sign(ctx, claims.WorkerUUID, edgebootstrap.DefaultJWTTTL, now)
	if err != nil {
		log.Printf("[ERROR] edge token exchange: refresh jwt for %s: %v", worker.Name, err)
		httputil.WriteError(w, http.StatusInternalServerError, "failed to issue token")
		return
	}

	// Record the UUID we just consumed so rotation detection compares
	// against the value the Edge host is actually using.
	if worker.Annotations == nil {
		worker.Annotations = map[string]string{}
	}
	if worker.Annotations[v1beta1.AnnotationEdgeAppliedUUID] != claims.WorkerUUID {
		worker.Annotations[v1beta1.AnnotationEdgeAppliedUUID] = claims.WorkerUUID
		if err := h.client.Update(ctx, worker); err != nil {
			log.Printf("[ERROR] edge token exchange: update applied-uuid annotation on %s: %v", worker.Name, err)
			httputil.WriteError(w, http.StatusInternalServerError, "failed to issue token: "+err.Error())
			return
		}
	}

	httputil.WriteJSON(w, http.StatusOK, EdgeTokenResponse{
		Token:              token,
		ExpiresAt:          expiresAt.UTC().Format(time.RFC3339),
		JWTToken:           refreshedJWT.Token,
		JWTExpiresAt:       refreshedJWT.ExpiresAt.UTC().Format(time.RFC3339),
		WorkerName:         workerName,
		WorkerResourceName: worker.Name,
		RuntimeName:        workerName,
		TeamName:           worker.Labels[v1beta1.LabelTeam],
	})
}
