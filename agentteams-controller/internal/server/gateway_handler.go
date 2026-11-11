package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// GatewayHandler handles /api/v1/gateway/* requests using the unified gateway.Client.
type GatewayHandler struct {
	gw                     gateway.Client
	fallbackConfigWritable bool
}

func NewGatewayHandler(gw gateway.Client) *GatewayHandler {
	return &GatewayHandler{gw: gw, fallbackConfigWritable: os.Getenv("AGENTTEAMS_HIGRESS_FALLBACK_CONFIG_WRITE_ENABLED") == "true"}
}

func (h *GatewayHandler) ListConsumers(w http.ResponseWriter, r *http.Request) {
	lister, ok := h.gw.(interface {
		ListConsumers(context.Context) ([]gateway.ConsumerSummary, error)
	})
	if !ok {
		httputil.WriteError(w, http.StatusNotImplemented, "gateway does not support listing consumers")
		return
	}
	consumers, err := lister.ListConsumers(r.Context())
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "unable to list gateway consumers")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, consumers)
}

func (h *GatewayHandler) CreateConsumer(w http.ResponseWriter, r *http.Request) {
	if h.gw == nil {
		httputil.WriteError(w, http.StatusNotImplemented, "no gateway backend available")
		return
	}

	var req CreateConsumerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}

	if lister, ok := h.gw.(interface {
		ListConsumers(context.Context) ([]gateway.ConsumerSummary, error)
	}); ok {
		consumers, err := lister.ListConsumers(r.Context())
		if err != nil {
			httputil.WriteError(w, http.StatusBadGateway, "unable to check existing gateway consumers")
			return
		}
		for _, consumer := range consumers {
			if consumer.Name == req.Name {
				httputil.WriteError(w, http.StatusConflict, "consumer already exists; choose a different name")
				return
			}
		}
	}

	if strings.TrimSpace(req.CredentialKey) == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "unable to generate consumer credential")
			return
		}
		req.CredentialKey = hex.EncodeToString(key)
	}

	result, err := h.gw.EnsureConsumer(r.Context(), gateway.ConsumerRequest{
		Name:          req.Name,
		CredentialKey: req.CredentialKey,
	})
	if err != nil {
		log.Printf("[ERROR] create consumer %s: %v", req.Name, err)
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if result.Status == "exists" {
		httputil.WriteError(w, http.StatusConflict, "consumer already exists; choose a different name")
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, ConsumerResponse{
		Name:       req.Name,
		ConsumerID: result.ConsumerID,
		APIKey:     result.APIKey,
		Status:     result.Status,
	})
}

func (h *GatewayHandler) BindConsumer(w http.ResponseWriter, r *http.Request) {
	if h.gw == nil {
		httputil.WriteError(w, http.StatusNotImplemented, "no gateway backend available")
		return
	}

	consumerName := r.PathValue("id")
	if consumerName == "" {
		httputil.WriteError(w, http.StatusBadRequest, "consumer name is required")
		return
	}

	if err := h.gw.AuthorizeAIRoutes(r.Context(), consumerName, ""); err != nil {
		log.Printf("[ERROR] bind consumer %s: %v", consumerName, err)
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *GatewayHandler) DeleteConsumer(w http.ResponseWriter, r *http.Request) {
	if h.gw == nil {
		httputil.WriteError(w, http.StatusNotImplemented, "no gateway backend available")
		return
	}

	consumerName := r.PathValue("id")
	if consumerName == "" {
		httputil.WriteError(w, http.StatusBadRequest, "consumer name is required")
		return
	}

	// Higress refuses to delete a consumer still referenced by AI routes.
	if err := h.gw.DeauthorizeAIRoutes(r.Context(), consumerName, ""); err != nil {
		log.Printf("[ERROR] unbind consumer %s before delete: %v", consumerName, err)
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.gw.DeleteConsumer(r.Context(), consumerName); err != nil {
		log.Printf("[ERROR] delete consumer %s: %v", consumerName, err)
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ListAIRoutes serves GET /api/v1/gateway/ai-routes: the read-only AI route
// catalog. Each entry is a gateway route — its name, the providers serving
// it (upstreams) and the consumers authorized on it. A route is the /v1
// entry point with consumer authorization, NOT a model: one route can
// serve several models, and the model IDs valid in chat-completion requests
// (and in Worker/Manager model fields) are defined by the route's upstream
// provider, not by the route name. This endpoint therefore answers "which
// routes exist and who may use them", not "which model names are valid".
// The route is registered with the "gateway" resource kind, which the
// authorizer grants to admin/manager only (L1); team leaders, humans and
// workers get 403.
func (h *GatewayHandler) ListAIRoutes(w http.ResponseWriter, r *http.Request) {
	if h.gw == nil {
		httputil.WriteError(w, http.StatusNotImplemented, "no gateway backend available")
		return
	}

	routes, err := h.gw.ListAIRoutes(r.Context())
	if err != nil {
		if errors.Is(err, gateway.ErrUnsupportedOp) {
			httputil.WriteError(w, http.StatusNotImplemented, err.Error())
			return
		}
		log.Printf("[ERROR] list ai routes: %v", err)
		httputil.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	if routes == nil {
		routes = []gateway.AIRouteInfo{}
	}
	httputil.WriteJSON(w, http.StatusOK, AIRouteListResponse{Routes: routes, Total: len(routes)})
}
