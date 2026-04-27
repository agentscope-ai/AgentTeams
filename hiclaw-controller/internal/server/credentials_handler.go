package server

import (
	"log"
	"net/http"

	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/credentials"
	"github.com/hiclaw/hiclaw-controller/internal/httputil"
)

// CredentialsHandler handles /api/v1/credentials/* requests.
type CredentialsHandler struct {
	stsService *credentials.STSService
}

func NewCredentialsHandler(stsService *credentials.STSService) *CredentialsHandler {
	return &CredentialsHandler{stsService: stsService}
}

// RefreshSTS handles POST /api/v1/credentials/sts
func (h *CredentialsHandler) RefreshSTS(w http.ResponseWriter, r *http.Request) {
	log.Printf("[INFO] STS credential request received from %s", r.RemoteAddr)
	if h.stsService == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "STS service not available (not in cloud mode)")
		return
	}

	caller := auth.CallerFromContext(r.Context())
	if caller == nil || caller.Username == "" {
		log.Printf("[WARN] STS credential request from %s: caller identity not found in request context", r.RemoteAddr)
		httputil.WriteError(w, http.StatusBadRequest, "caller identity not found in request context")
		return
	}

	log.Printf("[INFO] STS credential request from %s/%s", caller.Role, caller.Username)
	token, err := h.stsService.IssueForCaller(r.Context(), caller)
	if err != nil {
		log.Printf("[ERROR] issue STS token for %s/%s: %v", caller.Role, caller.Username, err)
		httputil.WriteError(w, http.StatusInternalServerError, "failed to issue STS token: "+err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusOK, token)
}
