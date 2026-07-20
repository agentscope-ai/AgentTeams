package server

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// PackageHandler handles ZIP package uploads to OSS.
type PackageHandler struct {
	oss oss.StorageClient
}

func NewPackageHandler(ossClient oss.StorageClient) *PackageHandler {
	return &PackageHandler{oss: ossClient}
}

// Upload handles POST /api/v1/packages.
// Accepts multipart/form-data with fields:
//   - file: ZIP binary
//   - name: resource name (used in the storage key)
//
// Returns {"packageUri": "oss://agentteams-config/packages/{name}-{hash}.zip"}
func (h *PackageHandler) Upload(w http.ResponseWriter, r *http.Request) {
	if h.oss == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "OSS client not configured")
		return
	}

	const maxUpload = 64 << 20 // 64 MB
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "parse multipart form: "+err.Error())
		return
	}

	name := r.FormValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name field is required")
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "file field is required: "+err.Error())
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "read uploaded file: "+err.Error())
		return
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(data))[:16]
	ossKey := fmt.Sprintf("agentteams-config/packages/%s-%s.zip", name, hash)

	if err := h.oss.PutObject(r.Context(), ossKey, data); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "upload to OSS: "+err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]string{
		"packageUri": "oss://" + ossKey,
	})
}
