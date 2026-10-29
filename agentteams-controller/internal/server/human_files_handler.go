package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultHumanFileMaxBytes is the per-file size limit for human file areas
// (#1414 P1), overridable via the HUMAN_FILE_MAX_BYTES environment variable.
const defaultHumanFileMaxBytes = 64 << 20 // 64 MiB

// humanFilesHandler serves the per-human file area API (#1414 P1): file
// read / list / write / soft-delete under files/<username>/files/, and the
// recycle bin under files/<username>/.trash/.
//
// All routes are authenticated (mw.Authenticate) but NOT authorized by the
// standard middleware, because the permission model needs two distinct
// denials: callers with no access to the area at all (another human, team
// leaders, workers) receive 404 so the API does not confirm a human's
// existence or file layout, while callers with access who attempt an
// admin-only operation (trash purge) receive 403. That split can only be
// made in the handler, after the Human CR is resolved.
type humanFilesHandler struct {
	client       client.Client
	namespace    string
	oss          oss.StorageClient
	auditClient  *audit.Client
	maxFileBytes int64
}

// NewHumanFilesHandler constructs the handler. The per-file size limit is
// read from HUMAN_FILE_MAX_BYTES (bytes); an unset or invalid value falls
// back to the 64 MiB default.
func NewHumanFilesHandler(k8sClient client.Client, namespace string, sc oss.StorageClient, ac *audit.Client) *humanFilesHandler {
	h := &humanFilesHandler{
		client:       k8sClient,
		namespace:    namespace,
		oss:          sc,
		auditClient:  ac,
		maxFileBytes: defaultHumanFileMaxBytes,
	}
	if v := os.Getenv("HUMAN_FILE_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			h.maxFileBytes = n
		} else {
			logEnvWarn("HUMAN_FILE_MAX_BYTES", v)
		}
	}
	return h
}

// logEnvWarn reports a misconfigured operator-facing env var without a
// request context to log against.
func logEnvWarn(name, value string) {
	// Kept deliberately quiet: a bad value simply falls back to the
	// default, which is safe.
	_ = name
	_ = value
}

// --- Storage layout -------------------------------------------------------
//
// The human file area lives under the bucket root, parallel to agents/ and
// teams/:
//
//	humans/<username>/files/...        human's working files (this API)
//	humans/<username>/.trash/<id>/     recycle bin entries (this API)
//	    ├── meta.json                  original path, deleted-at, caller, size
//	    └── content                    the deleted file's bytes
//
// memory/ and digest/ are intentionally not part of P1.

func humanAreaPrefix(username string) string  { return "humans/" + username + "/" }
func humanFilesPrefix(username string) string { return humanAreaPrefix(username) + "files/" }
func humanTrashPrefix(username string) string { return humanAreaPrefix(username) + ".trash/" }

// trashID form: <UTC yyyymmddThhmmssZ>-<8 hex>, e.g.
// 20261024T123045Z-abcd1234.
var trashIDRegex = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{8}$`)

// newTrashID builds a new recycle-bin entry id. The timestamp prefix keeps
// listings sortable by deletion time; the random suffix makes concurrent
// deletes of the same second unique.
func newTrashID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(buf), nil
}

// trashMeta is the per-entry metadata persisted at .trash/<id>/meta.json.
// It carries a controlled summary only — never file contents.
type trashMeta struct {
	Path        string `json:"path"` // original path, "files/<rel>"
	DeletedAt   string `json:"deletedAt"`
	Caller      string `json:"caller"`
	Role        string `json:"role"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType,omitempty"`
}

// humanFileAccess is the resolved authorization result for one request
// against a human's file area.
type humanFileAccess struct {
	human     *v1beta1.Human
	caller    *authpkg.CallerIdentity
	isAdmin   bool
	isManager bool
	isSelf    bool
}

// authorizeHumanFile resolves access to the file area of the Human CR
// named name. Access matrix (P1):
//
//   - RoleAdmin: full access (read, write, trash restore, purge, quota)
//   - RoleManager: read, write, trash restore (any human's area)
//   - the human themself (RoleHuman whose Username equals the area's
//     EffectiveUsername): read and write their own area
//   - everyone else (other humans, team leaders, workers): 404 — the
//     same response as a non-existent Human, to avoid existence probing
//
// adminOnly additionally requires RoleAdmin (purge), denying with 403
// when the caller has area access but lacks the role.
func (h *humanFilesHandler) authorizeHumanFile(ctx context.Context, name string, adminOnly bool) (*humanFileAccess, int, string) {
	var human v1beta1.Human
	if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &human); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, http.StatusNotFound, "human not found"
		}
		return nil, http.StatusInternalServerError, "human lookup failed"
	}
	caller := authpkg.CallerFromContext(ctx)
	if caller == nil {
		// Unauthenticated (or a test without injected identity): same
		// indistinguishable 404 as any other non-access caller.
		return nil, http.StatusNotFound, "human not found"
	}
	isAdmin := caller.Role == authpkg.RoleAdmin
	isManager := caller.Role == authpkg.RoleManager
	isSelf := caller.Role == authpkg.RoleHuman && caller.Username == human.Spec.EffectiveUsername(human.Name)
	if !isAdmin && !isManager && !isSelf {
		return nil, http.StatusNotFound, "human not found"
	}
	if adminOnly && !isAdmin {
		return nil, http.StatusForbidden, "admin role required"
	}
	return &humanFileAccess{human: &human, caller: caller, isAdmin: isAdmin, isManager: isManager, isSelf: isSelf}, 0, ""
}

// --- Wire-level shapes ----------------------------------------------------

// humanFileEntry is one entry in a directory listing.
type humanFileEntry struct {
	Name      string `json:"name"`
	Type      string `json:"type"` // "file" | "directory"
	Size      int64  `json:"size"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// humanFileListResponse is the directory-listing body.
type humanFileListResponse struct {
	Path    string           `json:"path"`
	Entries []humanFileEntry `json:"entries"`
}

// trashEntry is one entry in the trash listing.
type trashEntry struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	DeletedAt string `json:"deletedAt"`
	Caller    string `json:"caller"`
	Role      string `json:"role"`
}

// humanFileCodedError is the stable machine-readable body for
// precondition/limit rejections (412 etag_mismatch, 413 size_exceeded /
// quota_exceeded).
type humanFileCodedError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeHumanFileCodedError(w http.ResponseWriter, status int, code, message string) {
	httputil.WriteJSON(w, status, humanFileCodedError{Code: code, Message: message})
}

// --- Helpers ----------------------------------------------------------------

// humanFileDetail renders the audit Detail field: a controlled JSON
// summary of non-secret request attributes.
func humanFileDetail(kv ...any) string {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func (h *humanFilesHandler) recordAudit(ctx context.Context, a *humanFileAccess, action string, detail string) {
	if h.auditClient == nil {
		return
	}
	h.auditClient.Record(ctx, audit.Event{
		Who:    a.caller.Username,
		Role:   a.caller.Role,
		When:   time.Now().UTC(),
		Target: a.human.Name,
		Action: action,
		Detail: detail,
	})
}

func contentTypeForName(name string) string {
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// --- GET files (read one file or list a directory) --------------------------

// getFile handles GET /api/v1/humans/{name}/files/{path...}. When the path
// is empty (the files root), ends with "/", or ?list=1 is present the
// directory at that path is listed; otherwise the single file is streamed
// back with its ETag.
func (h *humanFilesHandler) getFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rawPath := r.PathValue("path")

	a, status, msg := h.authorizeHumanFile(r.Context(), name, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	username := a.human.Spec.EffectiveUsername(a.human.Name)

	if rawPath == "" || strings.HasSuffix(rawPath, "/") || r.URL.Query().Has("list") {
		h.listDirectory(w, r, a, username, rawPath)
		return
	}
	h.readSingleFile(w, r, a, username, rawPath)
}

func (h *humanFilesHandler) readSingleFile(w http.ResponseWriter, r *http.Request, a *humanFileAccess, username, rawPath string) {
	rel, err := validateHumanFilePath(rawPath)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rel == "" {
		httputil.WriteError(w, http.StatusBadRequest, "path is required to read a file; use a trailing / or ?list=1 to list a directory")
		return
	}
	ctx := r.Context()
	key := humanFilesPrefix(username) + rel
	data, err := h.oss.GetObject(ctx, key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httputil.WriteError(w, http.StatusNotFound, "file not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "read file failed")
		return
	}
	if meta, merr := h.oss.StatMeta(ctx, key); merr == nil && meta.ETag != "" {
		w.Header().Set("ETag", meta.ETag)
	}
	w.Header().Set("Content-Type", contentTypeForName(rel))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// listDirectory lists the first level of the directory at rawPath
// (""). Directory entries are synthesized from nested keys; marker objects
// (.keep) are hidden.
func (h *humanFilesHandler) listDirectory(w http.ResponseWriter, r *http.Request, a *humanFileAccess, username, rawPath string) {
	dir, err := validateHumanFilePath(rawPath)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	prefix := humanFilesPrefix(username)
	if dir != "" {
		prefix += dir + "/"
	}
	infos, err := h.oss.ListObjectsDetailed(ctx, prefix)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "list directory failed")
		return
	}
	entries := make(map[string]*humanFileEntry)
	for _, info := range infos {
		rel := info.Name
		if rel == "" {
			continue
		}
		base, isDir := rel, false
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			base, isDir = rel[:i], true
		}
		if base == ".keep" {
			continue // directory marker object
		}
		e, ok := entries[base]
		if !ok {
			e = &humanFileEntry{Name: base}
			entries[base] = e
		}
		if isDir {
			if e.Type == "" {
				e.Type = "directory"
			}
			if info.UpdatedAt > e.UpdatedAt {
				e.UpdatedAt = info.UpdatedAt
			}
			continue
		}
		e.Type = "file"
		e.UpdatedAt = info.UpdatedAt
		// ListObjectsDetailed does not carry sizes; resolve each file's
		// exact byte count (the same source the quota accounting uses).
		if meta, merr := h.oss.StatMeta(ctx, prefix+rel); merr == nil {
			e.Size = meta.Size
		}
	}
	out := make([]humanFileEntry, 0, len(entries))
	for _, e := range entries {
		if e.Type == "" {
			e.Type = "file"
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	respPath := "files/"
	if dir != "" {
		respPath += dir + "/"
	}
	httputil.WriteJSON(w, http.StatusOK, humanFileListResponse{Path: respPath, Entries: out})
}

// --- PUT files ---------------------------------------------------------------

// putFile handles PUT /api/v1/humans/{name}/files/{path...}. The raw
// request body is the file content. An If-Match header enables optimistic
// concurrency (412 etag_mismatch on conflict).
func (h *humanFilesHandler) putFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rawPath := r.PathValue("path")
	if strings.HasSuffix(rawPath, "/") {
		httputil.WriteError(w, http.StatusBadRequest, "cannot write a directory; name the file explicitly")
		return
	}
	a, status, msg := h.authorizeHumanFile(r.Context(), name, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	rel, err := validateHumanFilePath(rawPath)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rel == "" {
		httputil.WriteError(w, http.StatusBadRequest, "path is required")
		return
	}

	// Bounded read: maxFileBytes+1 so an over-limit upload is detected
	// without buffering more than one byte past the limit.
	data, err := io.ReadAll(io.LimitReader(r.Body, h.maxFileBytes+1))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "read request body failed")
		return
	}
	if int64(len(data)) > h.maxFileBytes {
		writeHumanFileCodedError(w, http.StatusRequestEntityTooLarge, "size_exceeded",
			fmt.Sprintf("file size exceeds the %d byte per-file limit", h.maxFileBytes))
		return
	}
	key := humanFilesPrefix(username) + rel
	// A replacement only charges its delta: used already counts the
	// object being replaced, so only newSize-oldSize may grow the area.
	// oldSize is 0 when the target does not exist yet.
	var oldSize int64
	if meta, merr := h.oss.StatMeta(ctx, key); merr == nil {
		oldSize = meta.Size
	} else if !errors.Is(merr, os.ErrNotExist) {
		httputil.WriteError(w, http.StatusInternalServerError, "stat file failed")
		return
	}
	reservedDelta, status, msg := h.reserveQuota(ctx, a, username, oldSize, int64(len(data)))
	if status != 0 {
		if status == http.StatusRequestEntityTooLarge {
			writeHumanFileCodedError(w, status, "quota_exceeded", msg)
		} else {
			httputil.WriteError(w, status, msg)
		}
		return
	}
	// The ledger reserves before the object write. If any later step fails,
	// release the reservation; after a successful write, keep it because the
	// new object's bytes are now part of usedBytes.
	committed := false
	if reservedDelta != 0 {
		defer func() {
			if !committed {
				h.releaseQuota(ctx, username, reservedDelta)
			}
		}()
	}

	if match := r.Header.Get("If-Match"); match != "" {
		// Resolve the current state first so both backends reject
		// identically when the target does not exist yet (mc If-Match
		// semantics are only specified for existing objects).
		meta, merr := h.oss.StatMeta(ctx, key)
		switch {
		case errors.Is(merr, os.ErrNotExist):
			writeHumanFileCodedError(w, http.StatusPreconditionFailed, "etag_mismatch",
				"If-Match failed: the file does not exist")
			return
		case merr != nil:
			httputil.WriteError(w, http.StatusInternalServerError, "stat file failed")
			return
		case meta.ETag != "":
			if meta.ETag != match {
				writeHumanFileCodedError(w, http.StatusPreconditionFailed, "etag_mismatch",
					"If-Match failed: the file changed since it was read")
				return
			}
		}
		if err := h.oss.PutObjectIfMatch(ctx, key, data, match); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				writeHumanFileCodedError(w, http.StatusPreconditionFailed, "etag_mismatch",
					"If-Match failed: the file changed since it was read")
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "write file failed")
			return
		}
		committed = true
	} else {
		if err := h.oss.PutObject(ctx, key, data); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "write file failed")
			return
		}
		committed = true
	}

	var etag string
	if meta, merr := h.oss.StatMeta(ctx, key); merr == nil {
		etag = meta.ETag
	}
	h.recordAudit(ctx, a, "human_file_write",
		humanFileDetail("path", "files/"+rel, "size", len(data), "etag", etag))
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"path": "files/" + rel,
		"size": len(data),
		"etag": etag,
	})
}

// --- DELETE files (soft delete into the recycle bin) -------------------------

// deleteFile handles DELETE /api/v1/humans/{name}/files/{path...}. The file
// is moved to .trash/<id>/ (meta.json + content) and the original removed;
// the response carries the trash id for a later restore.
func (h *humanFilesHandler) deleteFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rawPath := r.PathValue("path")
	if strings.HasSuffix(rawPath, "/") {
		httputil.WriteError(w, http.StatusBadRequest, "directory deletion is not supported; delete files individually")
		return
	}
	a, status, msg := h.authorizeHumanFile(r.Context(), name, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	rel, err := validateHumanFilePath(rawPath)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rel == "" {
		httputil.WriteError(w, http.StatusBadRequest, "path is required")
		return
	}
	key := humanFilesPrefix(username) + rel
	data, err := h.oss.GetObject(ctx, key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httputil.WriteError(w, http.StatusNotFound, "file not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "read file failed")
		return
	}
	trashID, err := newTrashID()
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "generate trash id failed")
		return
	}
	m := trashMeta{
		Path:        "files/" + rel,
		DeletedAt:   time.Now().UTC().Format(time.RFC3339),
		Caller:      a.caller.Username,
		Role:        a.caller.Role,
		Size:        int64(len(data)),
		ContentType: contentTypeForName(rel),
	}
	metaJSON, err := json.Marshal(m)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "encode trash metadata failed")
		return
	}
	trashPrefix := humanTrashPrefix(username) + trashID + "/"
	// Copy into trash BEFORE removing the original: a failure anywhere in
	// between leaves the file readable (at worst with a recoverable trash
	// copy) — never both gone.
	if err := h.oss.PutObject(ctx, trashPrefix+"meta.json", metaJSON); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "write trash metadata failed")
		return
	}
	if err := h.oss.PutObject(ctx, trashPrefix+"content", data); err != nil {
		_ = h.oss.DeleteObject(ctx, trashPrefix+"meta.json") // roll back the partial entry
		httputil.WriteError(w, http.StatusInternalServerError, "write trash content failed")
		return
	}
	if err := h.oss.DeleteObject(ctx, key); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError,
			"remove original failed; the file is preserved in the trash and may still be readable")
		return
	}
	h.recordAudit(ctx, a, "human_file_delete",
		humanFileDetail("path", "files/"+rel, "size", len(data), "trashId", trashID))
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"trashId": trashID})
}

// --- Trash --------------------------------------------------------------------

// listTrash handles GET /api/v1/humans/{name}/trash. Entries are newest
// first (the trash id is time-prefixed).
func (h *humanFilesHandler) listTrash(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, status, msg := h.authorizeHumanFile(r.Context(), name, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	prefix := humanTrashPrefix(username)
	infos, err := h.oss.ListObjectsDetailed(ctx, prefix)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "list trash failed")
		return
	}
	ids := make(map[string]struct{})
	for _, info := range infos {
		rel := strings.TrimSuffix(info.Name, "/")
		if rel == "" {
			continue
		}
		id := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			id = rel[:i]
		}
		if !trashIDRegex.MatchString(id) {
			continue
		}
		ids[id] = struct{}{}
	}
	out := make([]trashEntry, 0, len(ids))
	for id := range ids {
		e := trashEntry{ID: id}
		if data, gerr := h.oss.GetObject(ctx, prefix+id+"/meta.json"); gerr == nil {
			var m trashMeta
			if json.Unmarshal(data, &m) == nil {
				e.Path, e.Size, e.DeletedAt, e.Caller, e.Role = m.Path, m.Size, m.DeletedAt, m.Caller, m.Role
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// restoreTrash handles POST /api/v1/humans/{name}/trash/{id}/restore. The
// entry's bytes are written back to the original path and the trash entry
// removed. A pre-existing target yields 409 unless ?overwrite=1 is given
// by an admin.
func (h *humanFilesHandler) restoreTrash(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id := r.PathValue("id")
	if !trashIDRegex.MatchString(id) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid trash id")
		return
	}
	a, status, msg := h.authorizeHumanFile(r.Context(), name, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	trashPrefix := humanTrashPrefix(username) + id + "/"

	metaData, err := h.oss.GetObject(ctx, trashPrefix+"meta.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httputil.WriteError(w, http.StatusNotFound, "trash entry not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "read trash metadata failed")
		return
	}
	var m trashMeta
	if err := json.Unmarshal(metaData, &m); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "trash metadata is corrupt")
		return
	}
	content, err := h.oss.GetObject(ctx, trashPrefix+"content")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httputil.WriteError(w, http.StatusNotFound, "trash content not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "read trash content failed")
		return
	}
	// Defense against a tampered meta.json: the recorded path must still
	// land inside this human's files/ area.
	if !strings.HasPrefix(m.Path, "files/") {
		httputil.WriteError(w, http.StatusInternalServerError, "trash metadata path is invalid")
		return
	}
	rel, err := validateHumanFilePath(strings.TrimPrefix(m.Path, "files/"))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "trash metadata path is invalid: "+err.Error())
		return
	}

	override := r.URL.Query().Get("overwrite") == "1"
	if override && !a.isAdmin {
		httputil.WriteError(w, http.StatusForbidden, "only an admin may restore with overwrite")
		return
	}
	target := humanFilesPrefix(username) + rel
	var existingSize int64
	exists := false
	switch meta, serr := h.oss.StatMeta(ctx, target); {
	case serr == nil:
		exists = true
		existingSize = meta.Size
	case errors.Is(serr, os.ErrNotExist):
	default:
		httputil.WriteError(w, http.StatusInternalServerError, "stat restore target failed")
		return
	}
	if exists && !override {
		writeHumanFileCodedError(w, http.StatusConflict, "path_conflict",
			"a file already exists at the restore target; an admin may pass ?overwrite=1")
		return
	}
	// A restore is net-neutral for the quota: the trash entry's bytes are
	// already counted in usedBytes, and moving them back to files/ does not
	// add usage. An overwrite replaces an existing target and reduces usage
	// further. Only writes (putFile) can grow the area, so only they are
	// quota-gated; a restore must not be rejected at exactly the quota.
	if !exists {
		// Use If-None-Match:* so a file created between StatMeta and this
		// write cannot be silently overwritten. The check and create are
		// one storage operation, not a handler-local lock.
		if err := h.oss.PutObjectIfAbsent(ctx, target, content); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				writeHumanFileCodedError(w, http.StatusConflict, "path_conflict",
					"a file already exists at the restore target")
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "write restored file failed")
			return
		}
	} else if err := h.oss.PutObject(ctx, target, content); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "write restored file failed")
		return
	}
	// Remove the trash entry; a failure here only leaks a recoverable copy.
	_ = h.oss.DeleteObject(ctx, trashPrefix+"content")
	_ = h.oss.DeleteObject(ctx, trashPrefix+"meta.json")
	// An overwrite restore removes the pre-existing target; that target's
	// bytes were part of the ledger's usedBytes, so release them. A
	// non-overwrite restore is net-neutral and has existingSize == 0.
	h.adjustQuotaLedger(ctx, username, -existingSize)
	h.recordAudit(ctx, a, "human_trash_restore",
		humanFileDetail("path", "files/"+rel, "size", len(content), "trashId", id, "overwrite", override))
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"path": "files/" + rel,
		"size": len(content),
	})
}

// purgeTrash handles DELETE /api/v1/humans/{name}/trash/{id}: permanent
// removal. Admin-only (403 for other authorized callers, 404 for
// non-access callers).
func (h *humanFilesHandler) purgeTrash(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id := r.PathValue("id")
	if !trashIDRegex.MatchString(id) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid trash id")
		return
	}
	a, status, msg := h.authorizeHumanFile(r.Context(), name, true)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	trashPrefix := humanTrashPrefix(username) + id + "/"

	var m trashMeta
	if data, err := h.oss.GetObject(ctx, trashPrefix+"meta.json"); err == nil {
		_ = json.Unmarshal(data, &m)
	} else if !errors.Is(err, os.ErrNotExist) {
		httputil.WriteError(w, http.StatusInternalServerError, "read trash metadata failed")
		return
	}
	if serr := h.oss.Stat(ctx, trashPrefix+"content"); serr != nil && !errors.Is(serr, os.ErrNotExist) {
		httputil.WriteError(w, http.StatusInternalServerError, "stat trash entry failed")
		return
	}
	if m.Path == "" && errors.Is(h.oss.Stat(ctx, trashPrefix+"content"), os.ErrNotExist) {
		httputil.WriteError(w, http.StatusNotFound, "trash entry not found")
		return
	}
	if err := h.oss.DeletePrefix(ctx, trashPrefix); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "purge trash entry failed")
		return
	}
	// Purged bytes are gone from both files/ and .trash/; release them from
	// the ledger when one exists. A missing ledger is fine: the next quota
	// write seeds it from live usage (already reduced).
	h.adjustQuotaLedger(ctx, username, -m.Size)
	h.recordAudit(ctx, a, "human_trash_purge",
		humanFileDetail("path", m.Path, "size", m.Size, "trashId", id))
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"purged": id})
}

// --- Quota -------------------------------------------------------------------

// humanQuotaResponse is the GET quota body. quotaBytes is null when the
// human has no workspace quota (unlimited).
type humanQuotaResponse struct {
	UsedBytes  int64  `json:"usedBytes"`
	QuotaBytes *int64 `json:"quotaBytes"`
}

// usedBytes computes the current usage of a human's workspace live: the
// total size of every object under files/ plus .trash/ (trash entries
// count toward the quota). GET /quota uses this live value; the separate
// .control/quota.json ledger is only the atomic reservation state for
// writes and is excluded from these prefixes.
func (h *humanFilesHandler) usedBytes(ctx context.Context, username string) (int64, error) {
	total := int64(0)
	for _, prefix := range []string{humanFilesPrefix(username), humanTrashPrefix(username)} {
		infos, err := h.oss.ListObjectsDetailed(ctx, prefix)
		if err != nil {
			return 0, err
		}
		for _, info := range infos {
			if strings.HasSuffix(info.Name, "/") {
				continue
			}
			meta, err := h.oss.StatMeta(ctx, prefix+info.Name)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue // removed between the list and the stat
				}
				return 0, err
			}
			total += meta.Size
		}
	}
	return total, nil
}

// getQuota handles GET /api/v1/humans/{name}/quota: the area's live usage
// and its configured quota (null = unlimited).
func (h *humanFilesHandler) getQuota(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, status, msg := h.authorizeHumanFile(r.Context(), name, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	used, err := h.usedBytes(r.Context(), username)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "compute usage failed")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, humanQuotaResponse{
		UsedBytes:  used,
		QuotaBytes: a.human.Spec.WorkspaceQuotaBytes,
	})
}

// putQuota handles PUT /api/v1/humans/{name}/quota (admin only). Body:
// {"quotaBytes": N} to set the quota or {"quotaBytes": null} to clear it
// (unlimited).
func (h *humanFilesHandler) putQuota(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, status, msg := h.authorizeHumanFile(r.Context(), name, true)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	var body struct {
		QuotaBytes *int64 `json:"quotaBytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.QuotaBytes != nil && *body.QuotaBytes < 0 {
		httputil.WriteError(w, http.StatusBadRequest, "quotaBytes must not be negative")
		return
	}
	for attempt := 0; attempt < k8sUpdateMaxRetries; attempt++ {
		var human v1beta1.Human
		if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &human); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "human lookup failed")
			return
		}
		human.Spec.WorkspaceQuotaBytes = body.QuotaBytes
		if err := h.client.Update(ctx, &human); err != nil {
			if apierrors.IsConflict(err) && attempt+1 < k8sUpdateMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				continue
			}
			writeK8sError(w, "update human quota", err)
			return
		}
		h.recordAudit(ctx, a, "human_quota_set", humanFileDetail("quotaBytes", body.QuotaBytes))
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"quotaBytes": body.QuotaBytes})
		return
	}
}
