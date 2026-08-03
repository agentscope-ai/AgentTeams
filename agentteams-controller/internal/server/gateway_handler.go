package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// GatewayHandler handles /api/v1/gateway/* requests using the unified gateway.Client.
type GatewayHandler struct {
	gw gateway.Client
}

func NewGatewayHandler(gw gateway.Client) *GatewayHandler {
	return &GatewayHandler{gw: gw}
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

	result, err := h.gw.EnsureConsumer(r.Context(), gateway.ConsumerRequest{
		Name:          req.Name,
		CredentialKey: req.CredentialKey,
	})
	if err != nil {
		log.Printf("[ERROR] create consumer %s: %v", req.Name, err)
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
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

	if err := h.gw.DeleteConsumer(r.Context(), consumerName); err != nil {
		log.Printf("[ERROR] delete consumer %s: %v", consumerName, err)
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
