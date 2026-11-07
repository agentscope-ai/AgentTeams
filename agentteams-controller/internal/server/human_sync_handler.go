package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Per-human sync state endpoints (#1414 P3, server half; the client sync
// protocol lands in P4).
//
// The manifest is the client's sync state, stored verbatim by the server
// (no merging, no interpretation of entry payloads). The server owns
// generation: every accepted write bumps it under the per-human path
// lease, so two concurrent writers cannot both read the same base and
// lose an update. The sync scope is fixed to files/** — memory/ and
// digest/ are never part of the manifest.
//
// The manifest object lives at humans/<u>/.sync/manifest.json. The .sync/
// prefix is reserved by the file API (reservedHumanFilePrefixes), so these
// endpoints are its only address; the server talks to object storage
// directly, never through the file API.
//
// All handlers run on the humanFilesHandler so the sync surface reuses
// the file area's authorization shape, per-path lease, snapshot reader,
// coded errors, and audit trail instead of a second mechanism.

// humanSyncManifestRel is both the per-human lease key and, under the
// human area prefix, the manifest object key. One fixed rel per human is
// the per-human serialization unit for sync writes.
const humanSyncManifestRel = ".sync/manifest.json"

// humanManifestMaxBytes caps the manifest body (#1414 P3): 2 MiB.
const humanManifestMaxBytes = 2 << 20

// syncManifestVersion is the only accepted manifest schema version.
const syncManifestVersion = 1

// syncManifest is the stored sync state. Entries map a files/... path to
// the client's per-file sync record (opaque to the server); tombstones
// list the files/... paths the client deleted. Generation, UpdatedAt,
// and UpdatedBy are server-owned: an accepted write replaces them, and
// any client-supplied values are discarded.
type syncManifest struct {
	Version    int   `json:"version"`
	Generation int64 `json:"generation"`

	// Client payloads, opaque to the server.
	Entries    map[string]json.RawMessage `json:"entries"`
	Tombstones []json.RawMessage          `json:"tombstones"`

	// Server-owned: rewritten by every accepted write.
	UpdatedAt string `json:"updatedAt"`
	UpdatedBy string `json:"updatedBy"`
}

// syncStatus is the GET .../sync/status body.
type syncStatus struct {
	Entries    int    `json:"entries"`
	Tombstones int    `json:"tombstones"`
	Generation int64  `json:"generation"`
	UpdatedAt  string `json:"updatedAt"`
	UpdatedBy  string `json:"updatedBy"`
}

func humanSyncManifestKey(username string) string {
	return humanAreaPrefix(username) + humanSyncManifestRel
}

// authorizeHumanSync resolves access to one human's sync surface.
//
//   - RoleAdmin / RoleManager: any human's sync state.
//   - the human themself (RoleHuman whose username equals the CR's
//     EffectiveUsername): requires the file_sync capability or the
//     full_access meta value; a missing grant is 403 — a known identity
//     short of a permission, not the file area's 404 wall.
//   - another human: 404 — sync state is per-person; do not confirm a
//     foreign area exists (the P1 anti-probing discipline).
//   - team identities (team leader, worker) and unknown roles: 403 —
//     the sync surface is not granted to them at all.
//   - missing Human CR, or no caller identity: 404, indistinguishable
//     from every other non-access outcome.
func (h *humanFilesHandler) authorizeHumanSync(ctx context.Context, name string) (*humanFileAccess, int, string) {
	var human v1beta1.Human
	if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &human); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, http.StatusNotFound, "human not found"
		}
		return nil, http.StatusInternalServerError, "human lookup failed"
	}
	caller := authpkg.CallerFromContext(ctx)
	if caller == nil {
		return nil, http.StatusNotFound, "human not found"
	}
	isAdmin := caller.Role == authpkg.RoleAdmin
	isManager := caller.Role == authpkg.RoleManager
	isSelf := caller.Role == authpkg.RoleHuman && caller.Username == human.Spec.EffectiveUsername(human.Name)
	switch {
	case isAdmin, isManager:
		// Full access to any human's sync state.
	case isSelf:
		if !authpkg.HasCapability(caller, authpkg.CapabilityFileSync) {
			return nil, http.StatusForbidden, "the file_sync capability is required for sync access"
		}
	case caller.Role == authpkg.RoleHuman:
		// Another human's sync area: per-person surface, 404 wall.
		return nil, http.StatusNotFound, "human not found"
	default:
		return nil, http.StatusForbidden, "sync access is not granted to this identity"
	}
	return &humanFileAccess{human: &human, caller: caller, isAdmin: isAdmin, isManager: isManager, isSelf: isSelf}, 0, ""
}

// validSyncPath checks that p is a concrete file inside the fixed sync
// scope, files/**, with no traversal out of it. The memory/ / digest/ /
// .trash/ / .sync/ prefixes are the human workspace's never-synced areas;
// they cannot satisfy the files/ prefix check above, but the explicit
// rejection keeps that invariant visible if the scope rule is ever
// relaxed.
func validSyncPath(p string) error {
	if !strings.HasPrefix(p, "files/") || len(p) <= len("files/") {
		return fmt.Errorf("path %q must be a file inside files/", p)
	}
	for _, reserved := range []string{"memory/", "digest/", ".trash/", ".sync/"} {
		if strings.HasPrefix(p, reserved) {
			return fmt.Errorf("path %q is outside the sync scope", p)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q contains a forbidden segment", p)
		}
	}
	return nil
}

// parseSyncManifest validates a manifest body: version 1, every entry key
// and tombstone path inside the sync scope, and tombstones that are JSON
// objects with a string path. Entry payloads are opaque (stored
// verbatim, never interpreted).
func parseSyncManifest(data []byte) (syncManifest, error) {
	var m syncManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return syncManifest{}, fmt.Errorf("invalid JSON: %v", err)
	}
	if m.Version != syncManifestVersion {
		return syncManifest{}, fmt.Errorf("version must be %d, got %d", syncManifestVersion, m.Version)
	}
	for key := range m.Entries {
		if err := validSyncPath(key); err != nil {
			return syncManifest{}, err
		}
	}
	for _, raw := range m.Tombstones {
		var ts struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(raw, &ts); err != nil {
			return syncManifest{}, fmt.Errorf("tombstone must be an object with a string path")
		}
		if err := validSyncPath(ts.Path); err != nil {
			return syncManifest{}, err
		}
	}
	return m, nil
}

// storedSyncGeneration extracts the generation from a stored manifest.
// A body that does not parse (only possible for an out-of-band write —
// every in-protocol write is validated before storage) contributes no
// generation, so the successor starts at 1: generation counts
// in-protocol writes, and a corrupt base is exactly none of them.
func storedSyncGeneration(data []byte) int64 {
	var m syncManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return 0
	}
	return m.Generation
}

// getSyncManifest handles GET /api/v1/humans/{name}/sync/manifest. The
// stored manifest is returned verbatim with its ETag (the write base for
// a subsequent If-Match); a missing manifest is 404.
//
// The ETag comes from the stat; the bytes are read only when the
// caller's If-None-Match does not already match — a revalidation hit is
// a 304 with no body, priced at the one stat. (A straddle between the
// stat and the read hands the client a write base one version stale;
// the next If-Match fails closed with 412, so no write can clobber.)
func (h *humanFilesHandler) getSyncManifest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, status, msg := h.authorizeHumanSync(r.Context(), name)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	key := humanSyncManifestKey(a.human.Spec.EffectiveUsername(a.human.Name))

	meta, err := h.oss.StatMeta(ctx, key)
	if err != nil {
		writeStorageError(w, err, "manifest not found", "stat manifest failed")
		return
	}
	etag := httputil.BareETag(meta.ETag)
	hdr := w.Header()
	if etag != "" {
		hdr.Set("ETag", httputil.EtagQuoted(etag))
	}
	hdr.Set("Cache-Control", httputil.ETagCacheControl)
	hdr.Set("Vary", "Authorization")
	if etag != "" && httputil.IfNoneMatchMatches(r.Header.Get("If-None-Match"), etag) {
		// Revalidation hit: the validators of the current
		// representation, no body (RFC 9110 §15.4.5).
		w.WriteHeader(http.StatusNotModified)
		return
	}

	data, err := h.oss.GetObject(ctx, key)
	if err != nil {
		writeStorageError(w, err, "manifest not found", "read manifest failed")
		return
	}
	hdr.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// putSyncManifest handles PUT /api/v1/humans/{name}/sync/manifest.
//
// The write is always conditional: If-Match <etag> replaces the version
// the client last saw (412 etag_mismatch on a mismatch, including a
// manifest that does not exist yet); If-None-Match: * creates the
// manifest on a first write (409 manifest_exists when one already
// exists). The body must be a valid manifest (2 MiB cap, version 1,
// files/** scope). The server rewrites generation, updatedAt, and
// updatedBy: generation is the stored value plus one, read under the
// per-human lease so concurrent writers cannot both read the same base;
// the client's own generation field is ignored.
func (h *humanFilesHandler) putSyncManifest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, status, msg := h.authorizeHumanSync(r.Context(), name)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	key := humanSyncManifestKey(username)

	ifMatch := r.Header.Get("If-Match")
	ifNoneMatch := r.Header.Get("If-None-Match")
	switch {
	case ifMatch == "" && ifNoneMatch == "":
		writeHumanFileCodedError(w, http.StatusBadRequest, "conditional_write_required",
			"a conditional write is required: If-Match <etag>, or If-None-Match: * for a first write")
		return
	case ifMatch != "" && ifNoneMatch != "":
		writeHumanFileCodedError(w, http.StatusBadRequest, "conditional_write_required",
			"exactly one of If-Match and If-None-Match is allowed")
		return
	case ifNoneMatch != "" && ifNoneMatch != "*":
		writeHumanFileCodedError(w, http.StatusBadRequest, "conditional_write_required",
			"If-None-Match only supports *")
		return
	}

	// Bounded read: limit+1 so an over-limit body is detected without
	// buffering more than one byte past the cap.
	data, err := io.ReadAll(io.LimitReader(r.Body, humanManifestMaxBytes+1))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "read request body failed")
		return
	}
	if int64(len(data)) > humanManifestMaxBytes {
		writeHumanFileCodedError(w, http.StatusRequestEntityTooLarge, "manifest_too_large",
			fmt.Sprintf("manifest exceeds the %d byte limit", humanManifestMaxBytes))
		return
	}
	manifest, err := parseSyncManifest(data)
	if err != nil {
		writeHumanFileCodedError(w, http.StatusBadRequest, "invalid_manifest", err.Error())
		return
	}

	// The per-human lease serializes this write against every other sync
	// write for this human (the file area's per-path lease, keyed on the
	// fixed manifest rel), making the generation read-modify-write below
	// atomic with respect to in-protocol writers.
	lock, err := h.acquireHumanPathLock(ctx, username, humanSyncManifestRel, h.pathLockTTL)
	if err != nil {
		if errors.Is(err, errHumanPathBusy) {
			writeHumanFileCodedError(w, http.StatusConflict, "file_busy",
				"another operation is in flight on this manifest; retry shortly")
			return
		}
		if errors.Is(err, oss.ErrConditionalUnsupported) {
			writeHumanFileCodedError(w, http.StatusServiceUnavailable, "conditional_write_unsupported",
				"storage provider does not support conditional writes; the path lease cannot be taken (fail closed)")
			return
		}
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "acquire path lease failed")
		return
	}
	defer lock.Release(ctx)

	// Precondition check and the generation base, both under the lease.
	var baseGen int64
	if ifMatch != "" {
		meta, merr := h.oss.StatMeta(ctx, key)
		switch {
		case errors.Is(merr, os.ErrNotExist):
			writeHumanFileCodedError(w, http.StatusPreconditionFailed, "etag_mismatch",
				"If-Match failed: the manifest does not exist")
			return
		case merr != nil:
			if errors.Is(merr, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "stat manifest failed")
			return
		case httputil.BareETag(meta.ETag) != "" && !strings.EqualFold(httputil.BareETag(meta.ETag), httputil.BareETag(ifMatch)):
			writeHumanFileCodedError(w, http.StatusPreconditionFailed, "etag_mismatch",
				"If-Match failed: the manifest changed since it was read")
			return
		}
		if old, oerr := h.oss.GetObject(ctx, key); oerr == nil {
			baseGen = storedSyncGeneration(old)
		} else if !errors.Is(oerr, os.ErrNotExist) {
			if errors.Is(oerr, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "read manifest failed")
			return
		}
	} else { // ifNoneMatch == "*"
		if _, merr := h.oss.StatMeta(ctx, key); merr == nil {
			writeHumanFileCodedError(w, http.StatusConflict, "manifest_exists",
				"a manifest already exists; read it and retry with If-Match")
			return
		} else if !errors.Is(merr, os.ErrNotExist) {
			if errors.Is(merr, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "stat manifest failed")
			return
		}
	}

	manifest.Generation = baseGen + 1
	manifest.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	manifest.UpdatedBy = a.caller.Username
	out, err := json.Marshal(&manifest)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "encode manifest failed")
		return
	}
	// Fence: an expired holder whose lease was taken over must not keep
	// mutating the manifest.
	if verr := lock.Verify(ctx); verr != nil {
		if errors.Is(verr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		writeHumanFileCodedError(w, http.StatusConflict, "lease_lost",
			"the path lease was taken over by another operation; nothing was written")
		return
	}
	var werr error
	if ifMatch != "" {
		// Storage takes the bare validator: the client may echo the
		// quoted ETag straight back from a GET (normalized once).
		werr = h.oss.PutObjectIfMatch(ctx, key, out, httputil.BareETag(ifMatch))
	} else {
		werr = h.oss.PutObjectIfAbsent(ctx, key, out)
	}
	if werr != nil {
		if errors.Is(werr, oss.ErrPreconditionFailed) {
			if ifMatch != "" {
				writeHumanFileCodedError(w, http.StatusPreconditionFailed, "etag_mismatch",
					"If-Match failed: the manifest changed since it was read")
			} else {
				writeHumanFileCodedError(w, http.StatusConflict, "manifest_exists",
					"a manifest already exists; read it and retry with If-Match")
			}
			return
		}
		if errors.Is(werr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "write manifest failed")
		return
	}

	etag := ""
	if meta, merr := h.oss.StatMeta(ctx, key); merr == nil {
		etag = meta.ETag
	}
	if etag == "" {
		etag = md5Hex(out) // single-part content hash: the ETag of exactly these bytes
	}
	h.recordAudit(ctx, a, "human_manifest_write",
		"entries", len(manifest.Entries), "tombstones", len(manifest.Tombstones), "generation", manifest.Generation, "etag", etag)
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"etag":       etag,
		"generation": manifest.Generation,
		"updatedAt":  manifest.UpdatedAt,
	})
}

// getSyncStatus handles GET /api/v1/humans/{name}/sync/status: the stored
// manifest's summary (entry/tombstone counts, generation, last update).
// A missing manifest is a zero summary with 200, not an error.
func (h *humanFilesHandler) getSyncStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, status, msg := h.authorizeHumanSync(r.Context(), name)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	key := humanSyncManifestKey(a.human.Spec.EffectiveUsername(a.human.Name))
	data, err := h.oss.GetObject(ctx, key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httputil.WriteJSON(w, http.StatusOK, syncStatus{})
			return
		}
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "read manifest failed")
		return
	}
	var m syncManifest
	if err := json.Unmarshal(data, &m); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "stored manifest is corrupt")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, syncStatus{
		Entries:    len(m.Entries),
		Tombstones: len(m.Tombstones),
		Generation: m.Generation,
		UpdatedAt:  m.UpdatedAt,
		UpdatedBy:  m.UpdatedBy,
	})
}
