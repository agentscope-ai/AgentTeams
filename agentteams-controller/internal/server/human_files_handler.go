package server

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
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
	"sync"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// defaultHumanFileMaxBytes is the per-file size limit for human file areas
// (#1414 P1), overridable via the HUMAN_FILE_MAX_BYTES environment variable.
const defaultHumanFileMaxBytes = 64 << 20 // 64 MiB

// defaultHumanFilePresignMaxBytes is the per-file size limit for direct
// (presigned) uploads in 1.2.6. Commit still buffers the staged content in
// the controller, so HUMAN_FILE_PRESIGN_MAX_BYTES may lower this limit but
// cannot raise it until bounded streaming commits are implemented.
const defaultHumanFilePresignMaxBytes = defaultHumanFileMaxBytes // 64 MiB

// defaultHumanPresignTTL is the lifetime of an issued presigned URL when
// the caller does not pass ?ttl=.
const defaultHumanPresignTTL = 5 * time.Minute

// maxHumanPresignTTL caps the lifetime a caller may request: a longer-lived
// URL is a wider tampering window, not a convenience.
const maxHumanPresignTTL = 60 * time.Minute

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
	// presignMaxBytes caps direct (presigned) uploads; the gate is checked
	// at commit against the bytes that actually landed.
	presignMaxBytes int64
	// pathLockTTL is the per-path lease lifetime; tests shorten it to exercise
	// expiry deterministically.
	pathLockTTL time.Duration
	// presignTTL is the presigned-URL lifetime for ?presign=1 (#1475 PR-4),
	// from AGENTTEAMS_PRESIGN_TTL (Go duration) clamped to
	// maxHumanPresignTTL.
	presignTTL time.Duration
	// changesPruneInterval gates the per-human change-log retention
	// sweep; tests set it to 0 to prune on every append.
	changesPruneInterval time.Duration
	// changesPruneMu guards changesPruneAt, the last retention sweep per
	// human (username -> wall-clock time).
	changesPruneMu sync.Mutex
	changesPruneAt map[string]time.Time
}

// NewHumanFilesHandler constructs the handler. The per-file size limit is
// read from HUMAN_FILE_MAX_BYTES (bytes); an unset or invalid value falls
// back to the 64 MiB default.
func NewHumanFilesHandler(k8sClient client.Client, namespace string, sc oss.StorageClient, ac *audit.Client) *humanFilesHandler {
	h := &humanFilesHandler{
		client:               k8sClient,
		namespace:            namespace,
		oss:                  sc,
		auditClient:          ac,
		maxFileBytes:         defaultHumanFileMaxBytes,
		presignMaxBytes:      defaultHumanFilePresignMaxBytes,
		pathLockTTL:          humanPathLockTTL,
		presignTTL:           defaultHumanPresignTTL,
		changesPruneInterval: humanChangePruneInterval,
		changesPruneAt:       make(map[string]time.Time),
	}
	if v := os.Getenv("HUMAN_FILE_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			h.maxFileBytes = n
		} else {
			logEnvWarn("HUMAN_FILE_MAX_BYTES", v)
		}
	}
	if v := os.Getenv("HUMAN_FILE_PRESIGN_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n <= defaultHumanFilePresignMaxBytes {
			h.presignMaxBytes = n
		} else {
			logEnvWarn("HUMAN_FILE_PRESIGN_MAX_BYTES", v)
		}
	}
	// AGENTTEAMS_PRESIGN_TTL is a Go duration ("5m", "30m", "1h"). An unset,
	// unparseable, or non-positive value falls back to the default; a valid
	// value above the cap is clamped to it.
	if v := os.Getenv("AGENTTEAMS_PRESIGN_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			logEnvWarn("AGENTTEAMS_PRESIGN_TTL", v)
		} else {
			if d > maxHumanPresignTTL {
				d = maxHumanPresignTTL
			}
			h.presignTTL = d
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

// presignGetter is the capability the ?presign=1 path needs from the
// storage backend (#1475 PR-4). It is a separate capability, not part of
// oss.StorageClient, so backends without presign support (in-memory test
// fakes, a future direct-S3 client) keep serving the non-presign routes
// unchanged; the presign route then fails closed with 500.
type presignGetter interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// isPresignRequest reports whether a files GET asked for a presigned
// download URL instead of the file bytes (#1475 PR-4).
func isPresignRequest(r *http.Request) bool {
	return r.URL.Query().Get("presign") == "1"
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
//	humans/<username>/.uploads/<session>/<rel>
//	                                 staging objects for direct uploads
//	                                 (this API): invisible to the file API
//	                                 and the quota, published to files/
//	                                 only by a successful commit
//
// memory/ and digest/ are intentionally not part of P1.
//
// .uploads/ and .trash/ are hidden area prefixes: the file API serves and
// lists only files/, and the quota walks only files/ plus .trash/, so both
// stay out of ordinary reads, listings, and usage accounting by
// construction.

func humanAreaPrefix(username string) string    { return "humans/" + username + "/" }
func humanFilesPrefix(username string) string   { return humanAreaPrefix(username) + "files/" }
func humanTrashPrefix(username string) string   { return humanAreaPrefix(username) + ".trash/" }
func humanUploadsPrefix(username string) string { return humanAreaPrefix(username) + ".uploads/" }

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
	// effectiveMode is the caller's resolved mode on the authorized path
	// (none/read/readwrite; readwrite for admin/manager/self). Write
	// operations (put/delete/restore) require readwrite.
	effectiveMode string
	// team is the caller's team that produced the winning effectiveMode
	// ("" for admin/manager/self), recorded in the audit Detail for write
	// attribution.
	team         string
	callerTeams  []string
	defaultTeams map[string]struct{}
}

// canReadPath reuses the request's resolved membership for each listed
// object, so a readable parent cannot disclose a restricted descendant.
func (a *humanFileAccess) canReadPath(rel string) bool {
	if a.isAdmin || a.isManager || a.isSelf {
		return true
	}
	if _, err := validateHumanFilePath(rel); err != nil {
		return false
	}
	mode, _ := humanWorkspaceEffectiveMode(&a.human.Spec, a.callerTeams, a.defaultTeams, rel)
	return mode != humanWorkspaceModeNone
}

// authorizeHumanFile resolves access to the file area of the Human CR
// named name. Access matrix (P1 + P2 workspace access):
//
//   - RoleAdmin: full access (read, write, trash restore, purge, quota)
//   - RoleManager: read, write, trash restore (any human's area)
//   - the human themself (RoleHuman whose Username equals the area's
//     EffectiveUsername): read and write their own area
//   - team leaders / workers / other humans: per-team rules — default
//     access follows membership: a caller team in the human's default set
//     (spec.accessibleTeams ∪ teams that list the human in
//     spec.humanMembers) has readwrite on files/**;
//     spec.workspaceAccess grants raise the mode for the teams they name,
//     and restrictions are a global deny: a restriction from any of the
//     caller's teams applies across all of the caller's teams (deny-wins,
//     fail closed), so a second, unrestricted team cannot override it. A
//     caller whose teams grant no access at all receives 404 — the same
//     response as a non-existent Human, to avoid existence probing.
//
// path is the files-relative path being authorized ("" = the files root,
// used by area-level endpoints). isWrite marks mutating operations; a
// readwrite requirement is denied with 403 (area access, no write grant).
// adminOnly additionally requires RoleAdmin (purge, quota set), denying
// with 403 when the caller has area access but lacks the role.
//
// A Team CR's spec.admin field is deliberately NOT a source of access:
// team administration never implies file-area rights.
func (h *humanFilesHandler) authorizeHumanFile(ctx context.Context, name, path string, isWrite, adminOnly bool) (*humanFileAccess, int, string) {
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
	if isAdmin || isManager || isSelf {
		if adminOnly && !isAdmin {
			return nil, http.StatusForbidden, "admin role required"
		}
		return &humanFileAccess{
			human:         &human,
			caller:        caller,
			isAdmin:       isAdmin,
			isManager:     isManager,
			isSelf:        isSelf,
			effectiveMode: humanWorkspaceModeReadWrite,
		}, 0, ""
	}
	// Team-scoped caller (leader, worker, or a human addressed through a
	// team identity). With no caller teams there is nothing to match.
	teams := callerTeamSet(caller)
	if len(teams) == 0 {
		return nil, http.StatusNotFound, "human not found"
	}
	defaultTeams, err := h.defaultTeams(ctx, &human)
	if err != nil {
		return nil, http.StatusInternalServerError, "team lookup failed"
	}
	mode, team := humanWorkspaceEffectiveMode(&human.Spec, teams, defaultTeams, path)
	if mode == humanWorkspaceModeNone {
		return nil, http.StatusNotFound, "human not found"
	}
	if adminOnly {
		return nil, http.StatusForbidden, "admin role required"
	}
	if isWrite && mode != humanWorkspaceModeReadWrite {
		return nil, http.StatusForbidden, "write access required"
	}
	return &humanFileAccess{human: &human, caller: caller, effectiveMode: mode, team: team, callerTeams: teams, defaultTeams: defaultTeams}, 0, ""
}

// callerTeamSet returns the teams the caller identity carries. Team
// leaders and workers are bound to their single Team; a human addressed
// through a team identity carries the Teams set from its Human CR.
// Roles with no team scoping return nil.
func callerTeamSet(caller *authpkg.CallerIdentity) []string {
	switch caller.Role {
	case authpkg.RoleTeamLeader, authpkg.RoleWorker:
		if caller.Team != "" {
			return []string{caller.Team}
		}
	case authpkg.RoleHuman:
		if len(caller.Teams) > 0 {
			return caller.Teams
		}
	}
	return nil
}

// defaultTeams computes the human's default team set:
// spec.accessibleTeams ∪ { team names that list the human in
// spec.humanMembers }. Teams in this set give the team's leaders and
// workers readwrite on files/** unless narrowed by a restriction.
func (h *humanFilesHandler) defaultTeams(ctx context.Context, human *v1beta1.Human) (map[string]struct{}, error) {
	sets := make(map[string]struct{})
	for _, t := range human.Spec.AccessibleTeams {
		if t != "" {
			sets[t] = struct{}{}
		}
	}
	var teams v1beta1.TeamList
	if err := h.client.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		return nil, err
	}
	for i := range teams.Items {
		t := &teams.Items[i]
		for _, m := range t.Spec.HumanMembers {
			if m.Name == human.Name {
				sets[t.Name] = struct{}{}
				break
			}
		}
	}
	return sets, nil
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

// humanFilePresignResponse is the ?presign=1 GET body (#1475 PR-4): a
// short-lived anonymous download URL plus the content-bound metadata the
// caller needs. The storage credentials used to sign the URL are never
// part of the response.
type humanFilePresignResponse struct {
	URL       string `json:"url"`
	ETag      string `json:"etag"`
	Size      int64  `json:"size"`
	ExpiresAt string `json:"expiresAt"`
}

// humanFilePresignUnsupportedResponse is the explicit capability boundary
// for ?presign=1 (#1501): the deployment has no caller-reachable storage
// origin (AGENTTEAMS_FS_PUBLIC_ENDPOINT unset), so no URL is minted — a
// URL signed for the internal endpoint is unusable from the caller. The
// stable error code lets clients detect the boundary and fall back to the
// streamed GET on the same path.
type humanFilePresignUnsupportedResponse struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
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

// recordAudit appends an audit event for an authorized operation. The
// Detail always carries the write attribution (#1414 P2): onBehalfOf names
// the human whose area was touched, and team names the caller's team when
// the access came from a team identity (leader/worker/human team identity);
// kv adds the operation-specific attributes as key/value pairs.
func (h *humanFilesHandler) recordAudit(ctx context.Context, a *humanFileAccess, action string, kv ...any) {
	if h.auditClient == nil {
		return
	}
	m := make(map[string]any, len(kv)/2+2)
	if len(kv) == 1 {
		// Callers may pass an already-rendered detail object (the variadic
		// humanFileDetail form used by the presign paths); merge it instead
		// of treating it as an unpaired key.
		if rendered, ok := kv[0].(string); ok && rendered != "" {
			_ = json.Unmarshal([]byte(rendered), &m)
		}
	} else {
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
	}
	m["onBehalfOf"] = a.human.Spec.EffectiveUsername(a.human.Name)
	if a.team != "" {
		m["team"] = a.team
	}
	var detail string
	if b, err := json.Marshal(m); err == nil {
		detail = string(b)
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

// md5Hex returns the lowercase MD5 hex digest of data — the S3 ETag of a
// single-part write (MinIO/RustFS semantics). Kept in this file so the
// snapshot logic has no private cross-file dependency.
func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// humanFileSnapshotAttempts bounds how many read cycles readFileSnapshot
// allows before declaring the object unstable.
const humanFileSnapshotAttempts = 6

// errHumanFileSnapshotUnstable is returned by readFileSnapshot when the
// object keeps changing between its reads; handlers map it to
// 503 snapshot_unstable.
var errHumanFileSnapshotUnstable = errors.New("file changed while taking a consistent snapshot")

// readFileSnapshot returns the object's bytes together with the storage
// version identifier that describes exactly those bytes.
//
// A plain GetObject+StatMeta pair can straddle a concurrent write (the
// bytes of version A with the ETag of version B); the ETag is the
// optimistic-concurrency token, so serving a mismatched pair lets an
// If-Match writer silently clobber a version the client never saw. The
// snapshot below re-reads until the bytes and the version provably agree:
//
//  1. single-part fast path: ETag == md5(bytes) binds them exactly (the
//     common case — files are single-part up to the multipart threshold);
//  2. multipart (md5-N), an unreported ETag, or a write that landed
//     between the two reads: a second, independent read must return the
//     same bytes and the same ETag;
//  3. six cycles without agreement -> errHumanFileSnapshotUnstable (fail
//     rather than serve a mismatched pair).
func (h *humanFilesHandler) readFileSnapshot(ctx context.Context, key string) ([]byte, oss.ObjectMeta, error) {
	for attempt := 0; attempt < humanFileSnapshotAttempts; attempt++ {
		data, err := h.oss.GetObject(ctx, key)
		if err != nil {
			return nil, oss.ObjectMeta{}, err
		}
		meta, err := h.oss.StatMeta(ctx, key)
		if err != nil {
			return nil, oss.ObjectMeta{}, err
		}
		if meta.ETag != "" && strings.EqualFold(strings.Trim(meta.ETag, `"`), md5Hex(data)) {
			return data, meta, nil
		}
		data2, err := h.oss.GetObject(ctx, key)
		if err != nil {
			return nil, oss.ObjectMeta{}, err
		}
		meta2, err := h.oss.StatMeta(ctx, key)
		if err != nil {
			return nil, oss.ObjectMeta{}, err
		}
		if bytes.Equal(data, data2) && meta.ETag == meta2.ETag {
			return data2, meta2, nil
		}
	}
	return nil, oss.ObjectMeta{}, errHumanFileSnapshotUnstable
}

// writeHumanFileSnapshotError maps a readFileSnapshot failure to its HTTP
// form: NotExist is a plain 404, an unstable snapshot is 503 with the
// stable code, a store-availability fault is 503 storage_unavailable,
// anything else a storage 500.
func writeHumanFileSnapshotError(w http.ResponseWriter, err error) {
	if errors.Is(err, errHumanFileSnapshotUnstable) {
		writeHumanFileCodedError(w, http.StatusServiceUnavailable, "snapshot_unstable",
			"the file changed while it was being read; retry the request")
		return
	}
	writeStorageError(w, err, "file not found", "read file failed")
}

// storageUnavailableBody is the stable body for every store-availability
// fault in the human files API (D1): the object store did not answer, the
// object's state is unknown, and the caller must retry — never treat the
// path as absent.
type storageUnavailableBody struct {
	Error string `json:"error"`
}

// writeStorageUnavailable is the single 503 storage_unavailable emitter
// for the human files API.
func writeStorageUnavailable(w http.ResponseWriter) {
	httputil.WriteJSON(w, http.StatusServiceUnavailable, storageUnavailableBody{Error: "storage_unavailable"})
}

// writeStorageError is the unified storage-error mapping for the human
// files API: os.ErrNotExist is a genuine absence (404 notFoundMsg),
// oss.ErrStorageUnavailable is 503 storage_unavailable, and everything
// else (auth/config faults like 403 AccessDenied, NoSuchBucket, ...) is a
// plain 500 otherMsg. The callers check the semantics this mapping must
// never disturb — tombstone 404, anti-probing 404, If-Match 412,
// conditional-write 503 — before reaching it.
func writeStorageError(w http.ResponseWriter, err error, notFoundMsg, otherMsg string) {
	if errors.Is(err, os.ErrNotExist) {
		httputil.WriteError(w, http.StatusNotFound, notFoundMsg)
		return
	}
	if errors.Is(err, oss.ErrStorageUnavailable) {
		writeStorageUnavailable(w)
		return
	}
	httputil.WriteError(w, http.StatusInternalServerError, otherMsg)
}

// humanFileTombstoneType marks a soft-deleted path. The object carries no
// bytes, so the quota is unaffected, and the marker lets reads and listings
// treat the path as absent without an unconditional delete.
const humanFileTombstoneType = "application/vnd.agentteams.tombstone"

// isHumanFileTombstone reports whether meta describes a deletion tombstone.
func isHumanFileTombstone(meta oss.ObjectMeta) bool {
	return strings.EqualFold(strings.TrimSpace(meta.ContentType), humanFileTombstoneType)
}

// --- GET files (read one file or list a directory) --------------------------

// getFile handles GET /api/v1/humans/{name}/files/{path...}. When the path
// is empty (the files root), ends with "/", or ?list=1 is present the
// directory at that path is listed; otherwise the single file is streamed
// back with its ETag — or, with ?presign=1, a short-lived presigned
// download URL is returned instead of the bytes (#1475 PR-4).
func (h *humanFilesHandler) getFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rawPath := r.PathValue("path")

	a, status, msg := h.authorizeHumanFile(r.Context(), name, rawPath, false, false)
	if a == nil {
		if status == http.StatusNotFound && isPresignRequest(r) {
			// A presign denial is an explicit 403, not a 404 (#1475 PR-4):
			// minting a temporary URL is a security-relevant operation. The
			// 403 is uniform across "human not found", "caller has no
			// access", and "no caller identity", so it still cannot be used
			// to probe which humans exist; only genuine lookup faults (500)
			// pass through unchanged.
			httputil.WriteError(w, http.StatusForbidden, "presign not allowed")
			return
		}
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
	if isPresignRequest(r) {
		h.presignFile(w, r, a, username, rel)
		return
	}
	ctx := r.Context()
	key := humanFilesPrefix(username) + rel

	// One StatMeta prices the answer (304 / 416 / 404 need only it); the
	// bytes stream lazily against the same snapshot, bound to its ETag,
	// so the GET path never double-reads the object. The consistency the
	// old snapshot double-read guaranteed (served bytes and served
	// validator from one version) is carried by the ETag-bound open: a
	// concurrent writer moves the ETag, the open 412s, and the request
	// re-stats — or answers 503 snapshot_unstable — never mixed bytes.
	rep, seeker, notModified, err := h.openForRead(ctx, r, key, rel)
	if err != nil {
		h.writeReadError(w, rep, err)
		return
	}

	hdr := w.Header()
	if notModified {
		// Revalidation hit: the validators of the current
		// representation, no body (RFC 9110 §15.4.5).
		if rep.etag != "" {
			hdr.Set("ETag", httputil.EtagQuoted(rep.etag))
		}
		hdr.Set("Cache-Control", httputil.ETagCacheControl)
		hdr.Set("Vary", "Authorization")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if rep.etag != "" {
		hdr.Set("ETag", httputil.EtagQuoted(rep.etag))
	}
	hdr.Set("Cache-Control", httputil.ETagCacheControl)
	hdr.Set("Vary", "Authorization")
	hdr.Set("Accept-Ranges", "bytes")
	hdr.Set("Content-Type", rep.contentType)

	// ServeContent does not close the reader; release the SDK stream on
	// success, client disconnect and panic alike.
	if seeker != nil {
		defer func() { _ = seeker.Close() }()
	}

	// ServeContent owns the content from here on (206/416, Content-Range,
	// Content-Length); If-None-Match and If-Range were resolved — or the
	// headers stripped — in openForRead, so its internal condition
	// evaluation never sees them.
	http.ServeContent(w, r, rel, rep.modTime, seeker)
}

// presignFile handles GET .../files/{path}?presign=1 (#1475 PR-4): it mints
// a short-lived presigned download URL so the bytes travel directly between
// the caller and storage instead of through the controller. Authorization
// already happened in getFile with the same access matrix as a plain read;
// this method checks existence and the tombstone marker, mints the URL, and
// records the issuance in the audit trail. The object content is never read
// and the signing credentials are never part of the response.
//
// When the deployment has no caller-reachable storage origin
// (AGENTTEAMS_FS_PUBLIC_ENDPOINT unset, #1501) no URL is minted: the
// response is 501 with a stable {"error":"presign_unsupported"} body so
// clients detect the capability boundary and fall back to the streamed GET
// on the same path.
func (h *humanFilesHandler) presignFile(w http.ResponseWriter, r *http.Request, a *humanFileAccess, username, rel string) {
	ctx := r.Context()
	key := humanFilesPrefix(username) + rel

	meta, err := h.oss.StatMeta(ctx, key)
	if err != nil {
		writeStorageError(w, err, "file not found", "stat file failed")
		return
	}
	if isHumanFileTombstone(meta) {
		httputil.WriteError(w, http.StatusNotFound, "file not found")
		return
	}

	presigner, ok := h.oss.(presignGetter)
	if !ok {
		httputil.WriteError(w, http.StatusInternalServerError, "presign failed")
		return
	}
	ttl := h.presignTTL
	downloadURL, err := presigner.PresignGet(ctx, key, ttl)
	if err != nil {
		if errors.Is(err, oss.ErrPresignUnsupported) {
			httputil.WriteJSON(w, http.StatusNotImplemented, humanFilePresignUnsupportedResponse{
				Error:  "presign_unsupported",
				Reason: "storage endpoint is not caller-reachable; set AGENTTEAMS_FS_PUBLIC_ENDPOINT or use the streamed GET",
			})
			return
		}
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "presign failed")
		return
	}
	expiresAt := time.Now().UTC().Add(ttl)
	h.recordAudit(ctx, a, "human_file_presign",
		"path", "files/"+rel, "ttl", ttl.String(), "expiresAt", expiresAt.Format(time.RFC3339))
	httputil.WriteJSON(w, http.StatusOK, humanFilePresignResponse{
		URL:       downloadURL,
		ETag:      meta.ETag,
		Size:      meta.Size,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	})
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
		writeStorageError(w, err, "list directory failed", "list directory failed")
		return
	}
	entries := make(map[string]*humanFileEntry)
	for _, info := range infos {
		rel := info.Name
		if rel == "" {
			continue
		}
		fullPath := rel
		if dir != "" {
			fullPath = dir + "/" + rel
		}
		if !a.canReadPath(strings.TrimSuffix(fullPath, "/")) {
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
		if info.Size > 0 {
			// The listing already carries the exact byte count; a
			// non-empty body can never be a tombstone, so no stat.
			e.Size = info.Size
			continue
		}
		// A zero-byte entry is either a legitimate empty file or a
		// deletion tombstone (an empty object with the marker content
		// type); only this shape needs the stat. On the `mc find`
		// fallback (no sizes reported) every file takes this path,
		// which is exactly the pre-P4a behavior.
		if meta, merr := h.oss.StatMeta(ctx, prefix+rel); merr == nil {
			if isHumanFileTombstone(meta) {
				delete(entries, base) // soft-deleted: not part of the listing
				continue
			}
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

// --- GET files root (directory or recursive flat listing) -------------------

// listFiles handles GET /api/v1/humans/{name}/files — the exact route for
// the files root. The {path...} wildcard route cannot serve the bare root
// without a redirect, so the root gets its own pattern; without
// ?recursive=1 the behavior is exactly listDirectory of the root, and with
// it the whole area is walked flat with key-cursor pagination.
func (h *humanFilesHandler) listFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	a, status, msg := h.authorizeHumanFile(r.Context(), name, "", false, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	if r.URL.Query().Get("recursive") == "1" {
		h.listRecursiveFiles(w, r, a, username)
		return
	}
	h.listDirectory(w, r, a, username, "")
}

// humanFileRecursiveEntry is one entry of the recursive (flat) listing: a
// file at any depth, addressed relative to the files/ root.
type humanFileRecursiveEntry struct {
	Path      string `json:"path"` // "files/<rel>"
	Size      int64  `json:"size"`
	ETag      string `json:"etag,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// humanFileRecursiveResponse is the recursive listing body. NextKey is the
// key cursor for the next page — pass it back as ?cursor= — and is nil
// when the listing is exhausted. It is deliberately named nextKey (not
// cursor/nextCursor): cursor-shaped fields are reserved for the
// incremental delta sync's sequence cursors.
type humanFileRecursiveResponse struct {
	Entries []humanFileRecursiveEntry `json:"entries"`
	NextKey *string                   `json:"nextKey"`
}

// Bounds for the recursive listing's page size.
const (
	defaultHumanFileListLimit = 1000
	maxHumanFileListLimit     = 10000
)

// clampHumanFileListLimit parses and bounds the recursive listing's limit
// parameter: empty → default, non-integer or < 1 → error, above the cap →
// clamped to maxHumanFileListLimit.
func clampHumanFileListLimit(v string) (int, error) {
	if v == "" {
		return defaultHumanFileListLimit, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("limit must be an integer, got %q", v)
	}
	if n < 1 {
		return 0, fmt.Errorf("limit must be a positive integer, got %d", n)
	}
	if n > maxHumanFileListLimit {
		n = maxHumanFileListLimit
	}
	return n, nil
}

// isReservedHumanFileSegment reports whether rel lives under one of the
// reserved top-level names (recycle bin, sync state, control state). The
// files API cannot create such paths, but out-of-band writes can; the
// recursive listing filters them the way validateHumanFilePath rejects
// them.
func isReservedHumanFileSegment(rel string) bool {
	seg := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		seg = rel[:i]
	}
	for _, reserved := range reservedHumanFilePrefixes {
		if seg == reserved {
			return true
		}
	}
	return false
}

// listRecursiveFiles serves the recursive flat listing of the whole files/
// area: every file at every depth in one sorted stream (no synthesized
// directory entries), paged by limit (default 1000, cap 10000) and walked
// with a key cursor — the previous page's nextKey is passed back as
// ?cursor= and the store skips keys up to and including it, so pages are
// contiguous (no key twice, none skipped). Tombstones (soft-deleted paths
// are empty objects with the marker content type) are excluded by statting
// zero-byte entries only.
func (h *humanFilesHandler) listRecursiveFiles(w http.ResponseWriter, r *http.Request, a *humanFileAccess, username string) {
	q := r.URL.Query()
	limit, err := clampHumanFileListLimit(q.Get("limit"))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The cursor is the previous page's nextKey: a full storage key under
	// this area's files/ prefix, generated by the store and passed back
	// verbatim as the StartAfter bound. Re-asserting the caller's prefix
	// keeps a forged cursor from reaching another human's area (and the
	// store itself rejects a StartAfter outside its Prefix).
	startAfter := ""
	if cur := q.Get("cursor"); cur != "" {
		if !strings.HasPrefix(cur, humanFilesPrefix(username)) {
			httputil.WriteError(w, http.StatusBadRequest, "invalid cursor: not a key inside this file area")
			return
		}
		startAfter = cur
	}
	ctx := r.Context()
	prefix := humanFilesPrefix(username)
	for {
		infos, nextKey, err := h.oss.ListObjectsRecursive(ctx, prefix, startAfter, limit)
		if err != nil {
			writeStorageError(w, err, "list files failed", "list files failed")
			return
		}
		entries := make([]humanFileRecursiveEntry, 0, len(infos))
		for _, info := range infos {
			rel := info.Name
			if rel == "" || rel == ".keep" || strings.HasSuffix(rel, "/.keep") || strings.HasSuffix(rel, "/") {
				continue // prefix marker, .keep marker object, directory marker
			}
			if isReservedHumanFileSegment(rel) {
				continue // .trash / .sync / .control written out of band
			}
			if !a.canReadPath(rel) {
				continue
			}
			if info.Size == 0 {
				// A zero-byte entry is either a legitimate empty file or a
				// deletion tombstone; only this shape needs the stat. A stat
				// that fails for any other reason keeps the entry: the client
				// reconciles against the manifest, and a false absence is how
				// a listing would trigger a spurious local delete.
				meta, merr := h.oss.StatMeta(ctx, prefix+rel)
				if merr == nil && isHumanFileTombstone(meta) {
					continue // soft-deleted: absent from the flat list
				}
			}
			entries = append(entries, humanFileRecursiveEntry{
				Path:      "files/" + rel,
				Size:      info.Size,
				ETag:      info.ETag,
				UpdatedAt: info.UpdatedAt,
			})
		}
		// Skip pages containing only hidden paths without exposing their keys.
		if len(entries) == 0 && nextKey != "" {
			startAfter = nextKey
			continue
		}
		resp := humanFileRecursiveResponse{Entries: entries}
		if nextKey != "" {
			// Only a readable path may appear in the client-visible cursor.
			cursor := prefix + strings.TrimPrefix(entries[len(entries)-1].Path, "files/")
			resp.NextKey = &cursor
		}
		httputil.WriteJSON(w, http.StatusOK, resp)
		return
	}
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
	a, status, msg := h.authorizeHumanFile(r.Context(), name, rawPath, true, false)
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
	// The per-path lease serializes this write against concurrent
	// deletes/restores on the same rel, so the oldSize stat, the quota
	// reservation, and the post-write snapshot see no in-API interleave.
	lock, err := h.acquireHumanPathLock(ctx, username, rel, h.pathLockTTL)
	if err != nil {
		if errors.Is(err, errHumanPathBusy) {
			writeHumanFileCodedError(w, http.StatusConflict, "file_busy",
				"another operation is in flight on this file; retry shortly")
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
		if !isHumanFileTombstone(meta) {
			oldSize = meta.Size
		}
	} else if !errors.Is(merr, os.ErrNotExist) {
		writeStorageError(w, merr, "stat file failed", "stat file failed")
		return
	}
	reservedToken, status, code, msg := h.reserveQuota(ctx, a, username, rel, oldSize, int64(len(data)))
	if status != 0 {
		switch {
		case status == http.StatusRequestEntityTooLarge:
			writeHumanFileCodedError(w, status, "quota_exceeded", msg)
		case status == http.StatusServiceUnavailable && code == "storage_unavailable":
			// The ledger could not be read or written: the store did not
			// answer, so the write is refused as a retryable 503 — never
			// a 500 (not retryable by the sync contract) and never a 404.
			writeStorageUnavailable(w)
			h.recordAudit(ctx, a, "human_file_write_blocked",
				"path", "files/"+rel, "size", len(data), "reason", "storage_unavailable")
		case status == http.StatusServiceUnavailable:
			// The storage provider rejected the atomic ledger write
			// (501/NotImplemented): without it the quota cannot be
			// reserved, so the write is refused rather than applied
			// unguarded (fail closed, #1426 P1).
			writeHumanFileCodedError(w, status, "conditional_write_unsupported", msg)
			h.recordAudit(ctx, a, "human_file_write_blocked",
				"path", "files/"+rel, "size", len(data), "reason", "conditional_write_unsupported")
		default:
			httputil.WriteError(w, status, msg)
		}
		return
	}
	// The ledger reserves before the object write. If any later step fails,
	// release the reservation; after a successful write, keep it because the
	// new object's bytes are now part of usedBytes.
	committed := false
	if reservedToken != "" {
		defer func() {
			if !committed {
				h.releaseQuota(ctx, username, rel, reservedToken)
			}
		}()
	}

	// Fence: an expired holder whose lease was taken over must not keep
	// mutating the file. The lease object is CAS-released, never deleted, so
	// this read can tell "still mine" from "replaced by a successor".
	if verr := lock.Verify(ctx); verr != nil {
		if errors.Is(verr, oss.ErrStorageUnavailable) {
			// The fence read itself failed because the store did not
			// answer: report the availability fault, not a takeover.
			writeStorageUnavailable(w)
			return
		}
		writeHumanFileCodedError(w, http.StatusConflict, "lease_lost",
			"the path lease was taken over by another operation; nothing was written")
		return
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
			writeStorageError(w, merr, "stat file failed", "stat file failed")
			return
		case isHumanFileTombstone(meta):
			// A soft delete leaves the path as a zero-byte tombstone,
			// which is logically absent. An empty file's MD5 ETag equals
			// the tombstone's, so without this case a PUT carrying the
			// pre-deletion If-Match would pass the ETag comparison below
			// and revive the deleted file. The response mirrors the
			// missing-file denial: the tombstone is an implementation
			// detail the API does not expose.
			writeHumanFileCodedError(w, http.StatusPreconditionFailed, "etag_mismatch",
				"If-Match failed: the file does not exist")
			return
		case meta.ETag != "":
			// Compare the bare form: a client echoing the quoted ETag
			// straight back from a GET must not 412 (the single
			// validator source is the storage form, normalized once).
			if httputil.BareETag(meta.ETag) != httputil.BareETag(match) {
				writeHumanFileCodedError(w, http.StatusPreconditionFailed, "etag_mismatch",
					"If-Match failed: the file changed since it was read")
				return
			}
		}
		// Pass the storage form down: the object's current ETag when the
		// backend reports one (exactly what the comparison checked), the
		// normalized client value otherwise (a backend without ETags
		// accepts any validator).
		matchETag := httputil.BareETag(match)
		if meta.ETag != "" {
			matchETag = httputil.BareETag(meta.ETag)
		}
		if err := h.oss.PutObjectIfMatch(ctx, key, data, matchETag); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				writeHumanFileCodedError(w, http.StatusPreconditionFailed, "etag_mismatch",
					"If-Match failed: the file changed since it was read")
				return
			}
			if errors.Is(err, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "write file failed")
			return
		}
		committed = true
		h.commitQuota(ctx, username, rel, reservedToken)
	} else {
		if err := h.oss.PutObject(ctx, key, data); err != nil {
			if errors.Is(err, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "write file failed")
			return
		}
		committed = true
		h.commitQuota(ctx, username, rel, reservedToken)
	}

	// Confirm the write with a content-bound snapshot: the ETag we report
	// must describe exactly the bytes we wrote. A stored body that differs
	// from what we sent means a storage-level change outside the API
	// protocol (or a backend that rewrites content); report 409 rather
	// than an ETag that would silently authorize a future If-Match.
	snapData, snapMeta, serr := h.readFileSnapshot(ctx, key)
	if serr != nil {
		if errors.Is(serr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "confirm file write failed")
		return
	}
	if !bytes.Equal(snapData, data) {
		writeHumanFileCodedError(w, http.StatusConflict, "version_conflict",
			"the stored content does not match the bytes we wrote")
		return
	}
	etag := snapMeta.ETag
	h.appendHumanChange(ctx, username, humanChangeEntry{
		Op:   "put",
		Path: "files/" + rel,
		Size: int64(len(data)),
		ETag: etag,
	})
	h.recordAudit(ctx, a, "human_file_write",
		"path", "files/"+rel, "size", len(data), "etag", etag)
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
	a, status, msg := h.authorizeHumanFile(r.Context(), name, rawPath, true, false)
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
	// Same per-path lease as putFile/restoreTrash: a concurrent in-API
	// writer on this rel is rejected with file_busy instead of racing the
	// archive step below.
	lock, err := h.acquireHumanPathLock(ctx, username, rel, h.pathLockTTL)
	if err != nil {
		if errors.Is(err, errHumanPathBusy) {
			writeHumanFileCodedError(w, http.StatusConflict, "file_busy",
				"another operation is in flight on this file; retry shortly")
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

	key := humanFilesPrefix(username) + rel
	// Archive the content-bound snapshot, not a bare GetObject: the bytes
	// in the trash must be exactly the bytes whose ETag we verify before
	// removing the original.
	data, snapMeta, err := h.readFileSnapshot(ctx, key)
	if err != nil {
		writeHumanFileSnapshotError(w, err)
		return
	}
	if isHumanFileTombstone(snapMeta) {
		httputil.WriteError(w, http.StatusNotFound, "file not found")
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
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "write trash metadata failed")
		return
	}
	if err := h.oss.PutObject(ctx, trashPrefix+"content", data); err != nil {
		_ = h.oss.DeleteObject(ctx, trashPrefix+"meta.json") // roll back the partial entry
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "write trash content failed")
		return
	}
	// Re-verify the version between the snapshot and the remove.
	//
	// Neither MinIO nor RustFS 1.0.4 exposes a conditional DELETE (RustFS
	// app/object/delete.rs has no if_match), so this re-stat is the
	// strongest guard the storage layer offers: the sub-millisecond
	// window between this check and DeleteObject cannot be closed by any
	// conditional write. Within the API the per-path lease above already
	// excludes a concurrent writer; a write that bypasses the API (mc/ops)
	// is outside the protocol. A missing object means a concurrent delete
	// won; the archive is a redundant copy of the same bytes, so report
	// success. A changed ETag means a newer version exists: roll the
	// trash entry back and keep the live file intact.
	fresh, merr := h.oss.StatMeta(ctx, key)
	switch {
	case errors.Is(merr, os.ErrNotExist):
		// Gone between snapshot and now: nothing left to remove.
	case merr != nil:
		_ = h.oss.DeleteObject(ctx, trashPrefix+"content")
		_ = h.oss.DeleteObject(ctx, trashPrefix+"meta.json")
		if errors.Is(merr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "stat file failed")
		return
	case !strings.EqualFold(fresh.ETag, snapMeta.ETag):
		_ = h.oss.DeleteObject(ctx, trashPrefix+"content")
		_ = h.oss.DeleteObject(ctx, trashPrefix+"meta.json")
		writeHumanFileCodedError(w, http.StatusConflict, "version_conflict",
			"the file changed since it was read; nothing was deleted")
		return
	default:
		if verr := lock.Verify(ctx); verr != nil {
			_ = h.oss.DeleteObject(ctx, trashPrefix+"content")
			_ = h.oss.DeleteObject(ctx, trashPrefix+"meta.json")
			if errors.Is(verr, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			writeHumanFileCodedError(w, http.StatusConflict, "lease_lost",
				"the path lease was taken over by another operation; nothing was deleted")
			return
		}
		// The removal is itself conditional: replacing the archived version
		// with a tombstone only succeeds while the live object is still that
		// exact version. An unconditional DeleteObject would remove whatever
		// landed after this handler read the file (for example a successor's
		// write once the lease expired); a failed CAS keeps that version.
		if err := h.oss.PutObjectIfMatchWithType(ctx, key, nil, humanFileTombstoneType, snapMeta.ETag); err != nil {
			_ = h.oss.DeleteObject(ctx, trashPrefix+"content")
			_ = h.oss.DeleteObject(ctx, trashPrefix+"meta.json")
			if errors.Is(err, oss.ErrPreconditionFailed) {
				writeHumanFileCodedError(w, http.StatusConflict, "version_conflict",
					"the file changed since it was read; nothing was deleted")
				return
			}
			if errors.Is(err, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError,
				"remove original failed; the file is preserved in the trash and may still be readable")
			return
		}
		// Only this request's own successful soft delete is recorded: the
		// ErrNotExist fall-through above means a concurrent delete won and
		// recorded its own entry.
		h.appendHumanChange(ctx, username, humanChangeEntry{
			Op:   "delete",
			Path: "files/" + rel,
			Size: int64(len(data)),
			ETag: snapMeta.ETag,
		})
	}
	h.recordAudit(ctx, a, "human_file_delete",
		"path", "files/"+rel, "size", len(data), "trashId", trashID)
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"trashId": trashID})
}

// --- Trash --------------------------------------------------------------------

// listTrash handles GET /api/v1/humans/{name}/trash. Entries are newest
// first (the trash id is time-prefixed).
func (h *humanFilesHandler) listTrash(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, status, msg := h.authorizeHumanFile(r.Context(), name, "", false, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	prefix := humanTrashPrefix(username)
	infos, err := h.oss.ListObjectsDetailed(ctx, prefix)
	if err != nil {
		writeStorageError(w, err, "list trash failed", "list trash failed")
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
		data, gerr := h.oss.GetObject(ctx, prefix+id+"/meta.json")
		if gerr != nil {
			if errors.Is(gerr, oss.ErrStorageUnavailable) {
				// A listing whose entries cannot be verified answers
				// the availability fault, not a 200 with empty fields.
				writeStorageUnavailable(w)
				return
			}
			// Any other read error keeps the entry with empty fields.
		} else {
			var m trashMeta
			if json.Unmarshal(data, &m) == nil {
				e.Path, e.Size, e.DeletedAt, e.Caller, e.Role = m.Path, m.Size, m.DeletedAt, m.Caller, m.Role
			}
		}
		if !a.isAdmin && !a.isManager && !a.isSelf {
			rel, ok := strings.CutPrefix(e.Path, "files/")
			if !ok || rel == "" || !a.canReadPath(rel) {
				continue
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
	a, status, msg := h.authorizeHumanFile(r.Context(), name, "", false, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	trashPrefix := humanTrashPrefix(username) + id + "/"
	// The trash entry lease serializes this restore against a purge (and a
	// second restore) of the same entry. Lock order: the entry lease is
	// taken before the restored path's lease, and no operation takes an
	// entry lease while holding a path lease (purge holds only the
	// entry's), so the two lease kinds cannot cycle.
	trashLock, err := h.acquireHumanPathLock(ctx, username, ".trash/"+id, h.pathLockTTL)
	if err != nil {
		if errors.Is(err, errHumanPathBusy) {
			writeHumanFileCodedError(w, http.StatusConflict, "file_busy",
				"another operation is in flight on this trash entry; retry shortly")
			return
		}
		if errors.Is(err, oss.ErrConditionalUnsupported) {
			writeHumanFileCodedError(w, http.StatusServiceUnavailable, "conditional_write_unsupported",
				"storage provider does not support conditional writes; the trash entry lease cannot be taken (fail closed)")
			return
		}
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "acquire path lease failed")
		return
	}
	defer trashLock.Release(ctx)

	metaData, err := h.oss.GetObject(ctx, trashPrefix+"meta.json")
	if err != nil {
		writeStorageError(w, err, "trash entry not found", "read trash metadata failed")
		return
	}
	var m trashMeta
	if err := json.Unmarshal(metaData, &m); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "trash metadata is corrupt")
		return
	}
	content, err := h.oss.GetObject(ctx, trashPrefix+"content")
	if err != nil {
		writeStorageError(w, err, "trash content not found", "read trash content failed")
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
	// A restore rewrites the original path, so the caller must hold
	// readwrite on that specific path. The area-level check above ran on
	// the root; the path-specific mode is what a grant/restriction
	// actually decided for this rel (a read grant may allow listing the
	// trash but never putting bytes back).
	if !a.isAdmin && !a.isManager && !a.isSelf {
		defaultTeams, terr := h.defaultTeams(ctx, a.human)
		if terr != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "team lookup failed")
			return
		}
		mode, _ := humanWorkspaceEffectiveMode(&a.human.Spec, callerTeamSet(a.caller), defaultTeams, rel)
		if mode != humanWorkspaceModeReadWrite {
			httputil.WriteError(w, http.StatusForbidden, "write access required")
			return
		}
	}

	override := r.URL.Query().Get("overwrite") == "1"
	if override && !a.isAdmin {
		httputil.WriteError(w, http.StatusForbidden, "only an admin may restore with overwrite")
		return
	}
	target := humanFilesPrefix(username) + rel
	// The same per-path lease as putFile/deleteFile: a restore mutates
	// this rel, so it must serialize against in-flight writes and deletes.
	lock, err := h.acquireHumanPathLock(ctx, username, rel, h.pathLockTTL)
	if err != nil {
		if errors.Is(err, errHumanPathBusy) {
			writeHumanFileCodedError(w, http.StatusConflict, "file_busy",
				"another operation is in flight on this file; retry shortly")
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

	var existingSize int64
	var existingETag string
	var tombstoneETag string
	exists := false
	switch meta, serr := h.oss.StatMeta(ctx, target); {
	case serr == nil:
		if isHumanFileTombstone(meta) {
			// A soft-deleted path is an absent target, but its tombstone is
			// still an object: replace that exact version instead of relying
			// on If-None-Match, which would see the object as present.
			tombstoneETag = meta.ETag
		} else {
			exists = true
			existingSize = meta.Size
			existingETag = meta.ETag
		}
	case errors.Is(serr, os.ErrNotExist):
	default:
		if errors.Is(serr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
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
	// quota-gated; a restore must not be rejected at exactly the quota. The
	// entry lease taken above is what guarantees it: while this restore
	// holds it, the entry cannot be purged, so its bytes stay counted in
	// usage until the restore itself removes the entry, one step after the
	// write lands.
	// Fence: an expired holder whose lease was taken over must not keep
	// mutating the file. The lease object is CAS-released, never deleted, so
	// this read can tell "still mine" from "replaced by a successor".
	if verr := lock.Verify(ctx); verr != nil {
		if errors.Is(verr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		writeHumanFileCodedError(w, http.StatusConflict, "lease_lost",
			"the path lease was taken over by another operation; nothing was written")
		return
	}
	// Commit-boundary recheck: the entry must still exist and this holder
	// must still own the entry lease before the restored bytes land. While
	// the entry lease is held a purge cannot remove the entry or release
	// its bytes, so both checks fail only if the lease expired and was
	// taken over in the meantime — in which case the entry may already be
	// gone and the restore must not write.
	if _, serr := h.oss.StatMeta(ctx, trashPrefix+"content"); serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			writeHumanFileCodedError(w, http.StatusConflict, "file_busy",
				"trash entry is no longer available; retry")
			return
		}
		if errors.Is(serr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "stat trash entry failed")
		return
	}
	if verr := trashLock.Verify(ctx); verr != nil {
		if errors.Is(verr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		writeHumanFileCodedError(w, http.StatusConflict, "lease_lost",
			"the trash entry lease was taken over by another operation; nothing was written")
		return
	}
	if !exists {
		// Use If-None-Match:* so a file created between StatMeta and this
		// write cannot be silently overwritten. The check and create are
		// one storage operation, not a handler-local lock.
		var werr error
		if tombstoneETag != "" {
			// The target is a deletion tombstone: CAS it away so a concurrent
			// recreate is not overwritten either.
			werr = h.oss.PutObjectIfMatchWithType(ctx, target, content, "", tombstoneETag)
		} else {
			werr = h.oss.PutObjectIfAbsent(ctx, target, content)
		}
		if werr != nil {
			if errors.Is(werr, oss.ErrPreconditionFailed) {
				writeHumanFileCodedError(w, http.StatusConflict, "path_conflict",
					"a file already exists at the restore target")
				return
			}
			if errors.Is(werr, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "write restored file failed")
			return
		}
	} else {
		// Re-stat before overwriting: the target's version must be the one
		// captured above, or a writer changed it in between (in-API it is
		// blocked by the lease, but a storage-level change still shows up
		// here). Never overwrite a version we have not seen.
		fresh, serr := h.oss.StatMeta(ctx, target)
		switch {
		case errors.Is(serr, os.ErrNotExist):
			// Deleted between the stats: recreate as a fresh object.
		case serr != nil:
			if errors.Is(serr, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "stat restore target failed")
			return
		case !strings.EqualFold(fresh.ETag, existingETag):
			writeHumanFileCodedError(w, http.StatusConflict, "version_conflict",
				"the restore target changed since it was read; nothing was overwritten")
			return
		}
		if err := h.oss.PutObject(ctx, target, content); err != nil {
			if errors.Is(err, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "write restored file failed")
			return
		}
	}
	// Fence for the tail accounting: the trash deletes below are a
	// recoverable copy (a failure only leaks the entry), but the ledger
	// release is not reversible. An overwrite restore releases the
	// pre-existing target's bytes exactly once — and only while this
	// holder still owns the entry lease. A lease taken over in the
	// meantime means a second restore of the same entry already removed
	// the entry and did this release, so deducting again would understate
	// usage. The deletes therefore proceed regardless; on a lost lease
	// only the accounting is skipped (overstate, never understate, until
	// the next reconciliation). A net-neutral restore (existingSize == 0)
	// has nothing to release and needs no fence.
	leaseHeld := existingSize == 0 || trashLock.Verify(ctx) == nil
	// Remove the trash entry; a failure here only leaks a recoverable copy.
	_ = h.oss.DeleteObject(ctx, trashPrefix+"content")
	_ = h.oss.DeleteObject(ctx, trashPrefix+"meta.json")
	if existingSize > 0 {
		if leaseHeld {
			// An overwrite restore removes the pre-existing target; that
			// target's bytes were part of the ledger's usedBytes, so
			// release them.
			h.adjustQuotaLedger(ctx, username, -existingSize)
		} else {
			log.FromContext(ctx).Info("warning: trash restore removed the entry but lost the entry lease before the ledger release; the replaced bytes stay counted (conservative overstate)",
				"human", username, "trashId", id, "size", existingSize)
		}
	}
	h.appendHumanChange(ctx, username, humanChangeEntry{
		Op:   "restore",
		Path: "files/" + rel,
		Size: int64(len(content)),
		ETag: md5Hex(content),
	})
	h.recordAudit(ctx, a, "human_trash_restore",
		"path", "files/"+rel, "size", len(content), "trashId", id, "overwrite", override)
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
	a, status, msg := h.authorizeHumanFile(r.Context(), name, "", false, true)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	trashPrefix := humanTrashPrefix(username) + id + "/"
	// The same entry lease a restore of this entry holds: purge must
	// serialize against it (lock order — purge holds only the entry lease,
	// never a path lease, so the two cannot cycle). While a restore holds
	// the lease, the entry's bytes are still in use and must not be
	// released from the quota.
	trashLock, err := h.acquireHumanPathLock(ctx, username, ".trash/"+id, h.pathLockTTL)
	if err != nil {
		if errors.Is(err, errHumanPathBusy) {
			writeHumanFileCodedError(w, http.StatusConflict, "file_busy",
				"another operation is in flight on this trash entry; retry shortly")
			return
		}
		if errors.Is(err, oss.ErrConditionalUnsupported) {
			writeHumanFileCodedError(w, http.StatusServiceUnavailable, "conditional_write_unsupported",
				"storage provider does not support conditional writes; the trash entry lease cannot be taken (fail closed)")
			return
		}
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "acquire path lease failed")
		return
	}
	defer trashLock.Release(ctx)

	var m trashMeta
	if data, err := h.oss.GetObject(ctx, trashPrefix+"meta.json"); err == nil {
		_ = json.Unmarshal(data, &m)
	} else if !errors.Is(err, os.ErrNotExist) {
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "read trash metadata failed")
		return
	}
	if serr := h.oss.Stat(ctx, trashPrefix+"content"); serr != nil && !errors.Is(serr, os.ErrNotExist) {
		if errors.Is(serr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "stat trash entry failed")
		return
	}
	if m.Path == "" && errors.Is(h.oss.Stat(ctx, trashPrefix+"content"), os.ErrNotExist) {
		httputil.WriteError(w, http.StatusNotFound, "trash entry not found")
		return
	}
	// Commit-boundary fence: MinIO and RustFS lack a conditional DELETE, so
	// the DeletePrefix below is unconditional — this re-Verify is the only
	// thing that keeps a stale holder from deleting. If the entry lease
	// expired and was taken over in the meantime (a restore of this entry
	// is the in-API takeover), the entry may already be gone or about to
	// be written back; abort without deleting and without touching the
	// ledger.
	if verr := trashLock.Verify(ctx); verr != nil {
		if errors.Is(verr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		writeHumanFileCodedError(w, http.StatusConflict, "lease_lost",
			"the trash entry lease was taken over by another operation; nothing was deleted")
		return
	}
	// Commit-boundary recheck: the entry must still exist. A restore that
	// took over the lease removed it, and purging an entry that is no
	// longer there would release bytes the area still holds.
	if _, serr := h.oss.StatMeta(ctx, trashPrefix+"content"); serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			writeHumanFileCodedError(w, http.StatusConflict, "file_busy",
				"trash entry is no longer available; retry")
			return
		}
		if errors.Is(serr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "stat trash entry failed")
		return
	}
	if err := h.oss.DeletePrefix(ctx, trashPrefix); err != nil {
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "purge trash entry failed")
		return
	}
	// The delete has landed and cannot be undone, so a lease lost in that
	// window must not be rolled back — but it must not deduct either: the
	// takeover (a restore) moved the bytes back into files/ and they are
	// still counted in usage, so releasing them would understate it.
	// Overstating is the safe direction: the ledger converges back on the
	// next quota write.
	if verr := trashLock.Verify(ctx); verr == nil {
		// Purged bytes are gone from both files/ and .trash/; release them
		// from the ledger when one exists. A missing ledger is fine: the
		// next quota write seeds it from live usage (already reduced).
		h.adjustQuotaLedger(ctx, username, -m.Size)
	} else {
		log.FromContext(ctx).Info("warning: trash purge deleted the entry but lost the entry lease before the ledger release; the bytes stay counted (conservative overstate)",
			"human", username, "trashId", id, "size", m.Size)
	}
	// A degenerate entry without a recorded path is not surfaced in the
	// change feed; the purge itself (and its audit) still happens.
	if m.Path != "" {
		h.appendHumanChange(ctx, username, humanChangeEntry{
			Op:   "purge",
			Path: m.Path,
			Size: m.Size,
		})
	}
	h.recordAudit(ctx, a, "human_trash_purge",
		"path", m.Path, "size", m.Size, "trashId", id)
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
	seen := make(map[string]struct{})
	// A listed key whose stat answers NotExist may still hold bytes (the
	// listing and the stat raced, or the key was written out-of-band).
	// Counting it zero is exactly how the space-name truncation bypassed
	// the quota, so it is re-verified below instead of skipped.
	var unverified []string
	var walk func(prefix string) error
	walk = func(prefix string) error {
		infos, err := h.oss.ListObjectsDetailed(ctx, prefix)
		if err != nil {
			return err
		}
		for _, info := range infos {
			if info.Name == "" || info.Name == ".keep" {
				continue
			}
			full := prefix + info.Name
			if strings.HasSuffix(info.Name, "/") {
				if err := walk(full); err != nil {
					return err
				}
				continue
			}
			// In-memory listings may already be recursive (names contain a
			// slash) while the production mc listing is one directory level.
			// Walking directories plus de-duplicating full keys covers both.
			if _, ok := seen[full]; ok {
				continue
			}
			meta, err := h.oss.StatMeta(ctx, full)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					unverified = append(unverified, full)
					continue
				}
				return err
			}
			seen[full] = struct{}{}
			total += meta.Size
		}
		return nil
	}
	for _, prefix := range []string{humanFilesPrefix(username), humanTrashPrefix(username)} {
		if err := walk(prefix); err != nil {
			return 0, err
		}
	}
	// Re-verify the keys the first pass could not stat. Re-listing would not
	// work: a key deleted in the window no longer appears in a fresh
	// listing, so a recount would silently zero it — the same bypass. A key
	// that still does not exist after the retry fails the scan (fail closed,
	// like every other unverifiable stat on the ledger path) instead of
	// counting its bytes as zero.
	for _, full := range unverified {
		meta, err := h.oss.StatMeta(ctx, full)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return 0, fmt.Errorf("listed object %s vanished from the store; usage cannot be verified", full)
			}
			return 0, err
		}
		total += meta.Size
	}
	return total, nil
}

// getQuota handles GET /api/v1/humans/{name}/quota: the area's live usage
// and its configured quota (null = unlimited).
func (h *humanFilesHandler) getQuota(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, status, msg := h.authorizeHumanFile(r.Context(), name, "", false, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	used, err := h.usedBytes(r.Context(), username)
	if err != nil {
		writeStorageError(w, err, "compute usage failed", "compute usage failed")
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
	a, status, msg := h.authorizeHumanFile(r.Context(), name, "", false, true)
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
		if body.QuotaBytes != nil {
			// Writes made while the quota was disabled may not have advanced
			// the ledger (or the workspace may predate it). Rebuild from live
			// usage before enforcing the new limit.
			if err := h.reconcileQuotaLedger(ctx, name); err != nil {
				if errors.Is(err, oss.ErrConditionalUnsupported) {
					writeHumanFileCodedError(w, http.StatusServiceUnavailable, "conditional_write_unsupported",
						"storage provider does not support conditional writes; the quota ledger cannot be reconciled (fail closed)")
					return
				}
				if errors.Is(err, oss.ErrStorageUnavailable) {
					writeStorageUnavailable(w)
					return
				}
				httputil.WriteError(w, http.StatusInternalServerError, "quota ledger reconciliation failed")
				return
			}
		}
		h.recordAudit(ctx, a, "human_quota_set", "quotaBytes", body.QuotaBytes)
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"quotaBytes": body.QuotaBytes})
		return
	}
}

// --- Presigned (direct) uploads, #P4a ---------------------------------------
//
// Large files bypass the controller's request body: the API issues a
// short-lived direct upload URL (presign), the client uploads straight to
// the object store, and a follow-up commit performs server-side
// verification, quota accounting, and the publish.
//
// The presigned URL targets a STAGING key, not the final path:
//
//	humans/<username>/.uploads/<session>/<rel>
//
// so the upload is invisible to ordinary GET/list (which serve only files/)
// and to the quota (which walks only files/ plus .trash/) until a commit
// explicitly publishes it. A commit that is rejected — or never made —
// leaves the bytes in the staging area only: the previously live file is
// untouched.
//
// The per-human state object under .control/ (outside the walked prefixes)
// binds the session to the target path:
//
//	humans/<username>/.control/presign-commits.json
//
//   - presign records {Session} for the rel: only the session this API
//     signed is committable, and only to the path it was signed for;
//   - commit verifies the staged object (size gate, declared sha256) and
//     publishes its content-bound snapshot to the final path under the
//     same per-path lease and quota reservation/commit as the in-body PUT;
//   - a commit with no in-flight session whose final object still carries
//     the last published ETag is a retry: idempotent 200, no re-charge.
//
// Cleanup of uncommitted staging objects is a TTL, not an in-repo task:
// the objects are orphaned and invisible, and an operator-configured
// bucket lifecycle expiration on humans/*/.uploads/ reclaims them (the
// presign TTL bounds when new bytes can still land). No background job is
// introduced by this API.

// humanPresignResponse is the presign (issue) body: the direct upload
// recipe for the client and the limit the upload must respect.
type humanPresignResponse struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Fields    map[string]string `json:"fields"`
	ExpiresAt string            `json:"expiresAt"`
	MaxBytes  int64             `json:"maxBytes"`
}

// humanPresignCommitResponse is the commit body.
type humanPresignCommitResponse struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	ETag string `json:"etag"`
}

// humanPresignStatePath is the per-human state object recording, for each
// path, which direct upload session is in flight and which version a commit
// last published — see the section comment.
const humanPresignStatePath = ".control/presign-commits.json"

func humanPresignStateKey(username string) string {
	return humanAreaPrefix(username) + humanPresignStatePath
}

// presignCommitEntry is the per-path state of the direct upload flow.
type presignCommitEntry struct {
	// Session is the staging session of the in-flight direct upload: the
	// bytes land at .uploads/<Session>/<rel> and reach the final path only
	// through a successful commit of that session. Empty when no upload is
	// in flight.
	Session string `json:"session,omitempty"`
	// Committed is the ETag of the version a commit last published to the
	// final path ("" when none). A commit with no in-flight session whose
	// final object still carries it is a retry: idempotent 200, no
	// re-charge.
	Committed string `json:"committed,omitempty"`
}

// presignCommitState is the whole per-human state object.
type presignCommitState struct {
	Commits map[string]presignCommitEntry `json:"commits"`
}

// newUploadSessionID builds a staging session id in the same shape as a
// trash id (sortable, unique within the same second).
func newUploadSessionID() (string, error) {
	return newTrashID()
}

// parseHumanPresignTTL decodes ?ttl=<seconds>: empty uses the default, and a
// value above maxHumanPresignTTL is rejected (a longer URL is a wider
// tampering window, not a convenience).
func parseHumanPresignTTL(v string) (time.Duration, error) {
	if v == "" {
		return defaultHumanPresignTTL, nil
	}
	seconds, err := strconv.ParseInt(v, 10, 64)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("ttl must be a positive integer number of seconds")
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl > maxHumanPresignTTL {
		return 0, fmt.Errorf("ttl must not exceed %s", maxHumanPresignTTL)
	}
	return ttl, nil
}

// parseHumanPresignSHA256 decodes ?sha256=<hex>; empty means the caller made
// no digest promise (the commit then skips content verification).
func parseHumanPresignSHA256(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	d := strings.ToLower(strings.TrimSpace(v))
	if _, err := hex.DecodeString(d); err != nil || len(d) != 64 {
		return "", fmt.Errorf("sha256 must be a 64-character hex digest")
	}
	return d, nil
}

// readPresignCommitState returns the per-human commit state bound to the
// ETag it was read at. os.ErrNotExist means no state yet (the zero value is
// the answer). The bytes must be bound to the ETag exactly like the quota
// ledger: a CAS on an unverified version would clobber a concurrent
// commit's baseline.
func (h *humanFilesHandler) readPresignCommitState(ctx context.Context, username string) (presignCommitState, string, error) {
	key := humanPresignStateKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		data, err := h.oss.GetObject(ctx, key)
		if err != nil {
			return presignCommitState{}, "", err
		}
		var state presignCommitState
		if err := json.Unmarshal(data, &state); err != nil {
			return presignCommitState{}, "", fmt.Errorf("decode presign commit state: %w", err)
		}
		meta, err := h.oss.StatMeta(ctx, key)
		if err != nil {
			return presignCommitState{}, "", err
		}
		if meta.ETag == "" {
			return presignCommitState{}, "", fmt.Errorf("presign commit state has no ETag")
		}
		if !quotaLedgerETagMatches(data, meta.ETag) {
			continue // bytes and ETag from different versions; re-snapshot
		}
		return state, meta.ETag, nil
	}
	return presignCommitState{}, "", fmt.Errorf("presign commit state changed while taking a consistent snapshot")
}

// writePresignCommitState applies mutate to the latest state under a strict
// CAS loop (If-None-Match create when the state does not exist yet). mutate
// must be idempotent: it re-runs on every lost race.
func (h *humanFilesHandler) writePresignCommitState(ctx context.Context, username string, mutate func(*presignCommitState)) error {
	key := humanPresignStateKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		state, etag, err := h.readPresignCommitState(ctx, username)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if state.Commits == nil {
			state.Commits = make(map[string]presignCommitEntry)
		}
		mutate(&state)
		payload, merr := json.Marshal(state)
		if merr != nil {
			return merr
		}
		var werr error
		if etag == "" {
			werr = h.oss.PutObjectIfAbsent(ctx, key, payload)
		} else {
			werr = strictPut(ctx, h.oss, key, payload, etag)
		}
		switch {
		case werr == nil:
			return nil
		case errors.Is(werr, oss.ErrPreconditionFailed):
			continue // concurrent writer; re-snapshot and re-apply
		default:
			return werr
		}
	}
	return fmt.Errorf("presign commit state write did not converge")
}

// postFile handles POST /api/v1/humans/{name}/files/{path...}: the presign
// issuer (?presign=1) and the direct-upload commit (a path ending in
// "/commit"). Go's ServeMux requires the wildcard to be the final segment
// of a pattern, so the two forms share one route and dispatch here.
func (h *humanFilesHandler) postFile(w http.ResponseWriter, r *http.Request) {
	rawPath := r.PathValue("path")
	switch {
	case r.URL.Query().Get("presign") == "1":
		h.presignUploadFile(w, r, rawPath)
	case strings.HasSuffix(rawPath, "/commit"):
		h.commitPresignedFile(w, r, strings.TrimSuffix(rawPath, "/commit"))
	default:
		httputil.WriteError(w, http.StatusNotFound, "unknown files POST")
	}
}

// presignFile handles POST .../files/{path...}?presign=1: it issues a
// short-lived direct upload URL for the file at the path.
//
// Quota pre-check: an area at its full quota cannot absorb any upload, so
// no URL is issued (413). The real enforcement still happens at commit,
// against the bytes that actually landed.
//
// The capability check (oss.PresignUploader) fails closed: a backend that
// cannot issue direct upload URLs answers 500 presign_unsupported rather
// than degrading.
func (h *humanFilesHandler) presignUploadFile(w http.ResponseWriter, r *http.Request, rawPath string) {
	name := r.PathValue("name")
	if strings.HasSuffix(rawPath, "/") {
		httputil.WriteError(w, http.StatusBadRequest, "path must name a file")
		return
	}
	a, status, msg := h.authorizeHumanFile(r.Context(), name, rawPath, true, false)
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
	ttl, err := parseHumanPresignTTL(r.URL.Query().Get("ttl"))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	sha256Hex, err := parseHumanPresignSHA256(r.URL.Query().Get("sha256"))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Quota pre-check (fail closed): an area at its full quota issues no
	// URL. A missing ledger means the usage comes from the live walk; any
	// other ledger fault is a 500 — issuing a URL against an unverifiable
	// ledger would open a bypass.
	if quota := a.human.Spec.WorkspaceQuotaBytes; quota != nil {
		var used int64
		switch ledger, _, lerr := h.readQuotaLedger(ctx, username); {
		case lerr == nil:
			used = ledger.UsedBytes
		case errors.Is(lerr, os.ErrNotExist):
			used, err = h.usedBytes(ctx, username)
			if err != nil {
				writeStorageError(w, err, "compute usage failed", "compute usage failed")
				return
			}
		default:
			if errors.Is(lerr, oss.ErrStorageUnavailable) {
				writeStorageUnavailable(w)
				return
			}
			httputil.WriteError(w, http.StatusInternalServerError, "read quota ledger failed")
			return
		}
		if used >= *quota {
			writeHumanFileCodedError(w, http.StatusRequestEntityTooLarge, "quota_exceeded",
				fmt.Sprintf("workspace quota of %d bytes is exhausted (%d in use); no upload URL is issued",
					*quota, used))
			return
		}
	}
	up, ok := h.oss.(oss.PresignUploader)
	if !ok {
		writeHumanFileCodedError(w, http.StatusInternalServerError, "presign_unsupported",
			"the object store backend cannot issue direct upload URLs")
		return
	}
	// The URL targets a staging key, not the final path: bytes the client
	// uploads land in the hidden .uploads/ area, where they are invisible to
	// the file API and the quota until a commit publishes them. A presign
	// for a path supersedes any earlier in-flight session for it (its
	// staging object is simply orphaned and ages out with the staging TTL).
	session, err := newUploadSessionID()
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "generate upload session failed")
		return
	}
	stageKey := humanUploadsPrefix(username) + session + "/" + rel
	method, url, fields, err := up.PresignUpload(ctx, stageKey, h.presignMaxBytes, contentTypeForName(rel), sha256Hex, ttl)
	if err != nil {
		if errors.Is(err, oss.ErrPresignUnsupported) {
			writeHumanFileCodedError(w, http.StatusNotImplemented, "presign_unsupported",
				"configure AGENTTEAMS_FS_PUBLIC_ENDPOINT for direct uploads or use the existing file PUT")
			return
		}
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "issue presigned upload failed")
		return
	}
	if fields == nil {
		fields = map[string]string{}
	}
	if err := h.writePresignCommitState(ctx, username, func(s *presignCommitState) {
		// Keep Committed: the last published version is still the last
		// published one while a new upload is in flight, and the retry
		// check at commit compares against it.
		prev := s.Commits[rel]
		s.Commits[rel] = presignCommitEntry{Session: session, Committed: prev.Committed}
	}); err != nil {
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "record presign session failed")
		return
	}
	h.recordAudit(ctx, a, "human_file_presign_issue",
		"path", "files/"+rel, "session", session, "maxBytes", h.presignMaxBytes, "ttlSeconds", int64(ttl.Seconds()))
	httputil.WriteJSON(w, http.StatusOK, humanPresignResponse{
		Method:    method,
		URL:       url,
		Fields:    fields,
		ExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339),
		MaxBytes:  h.presignMaxBytes,
	})
}

// commitPresignedFile handles POST .../files/{rel}/commit: the server-side
// half of a direct upload. The object already exists (the client uploaded
// straight to the staging key); this request verifies the staged object,
// books the quota charge, and publishes the bytes to the final path.
//
// Rejection and skip are both invisible: a rejected (or never made) commit
// leaves the bytes at the staging key only — invisible to GET/list and the
// quota, and the previously live file is never touched, because the
// publish happens after every check and under the same per-path lease the
// in-body PUT uses.
func (h *humanFilesHandler) commitPresignedFile(w http.ResponseWriter, r *http.Request, rawPath string) {
	name := r.PathValue("name")
	if strings.HasSuffix(rawPath, "/") {
		httputil.WriteError(w, http.StatusBadRequest, "path must name a file")
		return
	}
	a, status, msg := h.authorizeHumanFile(r.Context(), name, rawPath, true, false)
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
	state, _, serr := h.readPresignCommitState(ctx, username)
	if serr != nil && !errors.Is(serr, os.ErrNotExist) {
		if errors.Is(serr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "read presign commit state failed")
		return
	}
	entry := state.Commits[rel]
	// Session binding: only the session this API signed for this exact
	// path is committable, and only to that path.
	if entry.Session == "" {
		// No in-flight upload: a commit can only be a retry of one that
		// already published.
		if entry.Committed == "" {
			writeHumanFileCodedError(w, http.StatusNotFound, "object_missing",
				"no direct upload was signed for this path; issue a presign first")
			return
		}
		h.finishCommittedRetry(w, r, a, username, rel, key, entry.Committed)
		return
	}
	stageKey := humanUploadsPrefix(username) + entry.Session + "/" + rel
	meta, merr := h.oss.StatMeta(ctx, stageKey)
	switch {
	case errors.Is(merr, os.ErrNotExist):
		writeHumanFileCodedError(w, http.StatusNotFound, "object_missing",
			"no object at this path; the direct upload has not landed")
		return
	case merr != nil:
		if errors.Is(merr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "stat file failed")
		return
	}
	// Hard size gate: an over-limit upload is rejected, not accounted. The
	// staged object is kept (it is invisible to the file API and ages out
	// with the staging TTL); the live file is untouched.
	if meta.Size > h.presignMaxBytes {
		writeHumanFileCodedError(w, http.StatusRequestEntityTooLarge, "size_exceeded",
			fmt.Sprintf("file size %d exceeds the %d byte direct upload limit", meta.Size, h.presignMaxBytes))
		h.recordAudit(ctx, a, "human_file_presign_commit_rejected",
			"path", "files/"+rel, "size", meta.Size, "reason", "size_exceeded")
		return
	}
	// The per-path lease serializes the publish against in-API writers on
	// the same rel (the same protection putFile uses): while it is held,
	// the live file cannot be half-written or deleted out from under the
	// publish.
	lock, err := h.acquireHumanPathLock(ctx, username, rel, h.pathLockTTL)
	if err != nil {
		if errors.Is(err, errHumanPathBusy) {
			writeHumanFileCodedError(w, http.StatusConflict, "file_busy",
				"another operation is in flight on this file; retry shortly")
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
	// Re-check the session under the lease: the pre-lease read and the
	// publish are not one transaction. A concurrent commit of the same
	// session may have settled in between (publish, staging delete,
	// session clear), and a newer presign supersedes this session.
	freshState, _, fserr := h.readPresignCommitState(ctx, username)
	if fserr != nil && !errors.Is(fserr, os.ErrNotExist) {
		if errors.Is(fserr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "read presign commit state failed")
		return
	}
	fresh := freshState.Commits[rel]
	if fresh.Session != entry.Session {
		if fresh.Session == "" {
			// Settled by a concurrent commit of this session: answer the
			// same way a plain retry would.
			h.finishCommittedRetry(w, r, a, username, rel, key, fresh.Committed)
			return
		}
		writeHumanFileCodedError(w, http.StatusConflict, "version_conflict",
			"a newer upload was signed for this path; this session is stale")
		return
	}
	// Content-bound snapshot of the staged bytes: the size we gate and the
	// ETag we report describe exactly the bytes we publish.
	data, _, err := h.readFileSnapshot(ctx, stageKey)
	if err != nil {
		writeHumanFileSnapshotError(w, err)
		return
	}
	// The client may re-upload to the staging key while the presigned URL
	// is still valid, so the gate runs on the snapshot, not the earlier
	// stat.
	if int64(len(data)) > h.presignMaxBytes {
		writeHumanFileCodedError(w, http.StatusRequestEntityTooLarge, "size_exceeded",
			fmt.Sprintf("file size %d exceeds the %d byte direct upload limit", len(data), h.presignMaxBytes))
		h.recordAudit(ctx, a, "human_file_presign_commit_rejected",
			"path", "files/"+rel, "size", len(data), "reason", "size_exceeded")
		return
	}
	// Content verification: the sha256 the caller passes is checked against
	// the exact bytes about to be published. The ETag is deliberately NOT
	// used as a digest (multipart etags are not content hashes).
	if want := r.URL.Query().Get("sha256"); want != "" {
		digest, perr := parseHumanPresignSHA256(want)
		if perr != nil {
			httputil.WriteError(w, http.StatusBadRequest, perr.Error())
			return
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != digest {
			writeHumanFileCodedError(w, http.StatusConflict, "checksum_mismatch",
				"the staged content does not match the declared sha256; the upload was rejected")
			h.recordAudit(ctx, a, "human_file_presign_commit_rejected",
				"path", "files/"+rel, "size", len(data), "reason", "checksum_mismatch")
			return
		}
	}
	// Delta baseline: the version actually being replaced, statted under
	// the lease (the same source putFile uses; a missing or tombstoned
	// target is a fresh object charged in full).
	var oldSize int64
	if fmeta, ferr := h.oss.StatMeta(ctx, key); ferr == nil {
		if !isHumanFileTombstone(fmeta) {
			oldSize = fmeta.Size
		}
	} else if !errors.Is(ferr, os.ErrNotExist) {
		if errors.Is(ferr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "stat file failed")
		return
	}
	token, status, code, msg := h.reserveQuota(ctx, a, username, rel, oldSize, int64(len(data)))
	if status != 0 {
		switch status {
		case http.StatusRequestEntityTooLarge:
			writeHumanFileCodedError(w, status, "quota_exceeded", msg)
		case http.StatusServiceUnavailable:
			if code == "storage_unavailable" {
				writeStorageUnavailable(w)
				h.recordAudit(ctx, a, "human_file_write_blocked",
					"path", "files/"+rel, "size", int64(len(data)), "reason", "storage_unavailable")
			} else {
				writeHumanFileCodedError(w, status, code, msg)
			}
		default:
			httputil.WriteError(w, status, msg)
		}
		return
	}
	// Release the reservation only while the publish has not landed: after
	// the write, the bytes are real and an outstanding reservation is a
	// conservative overstatement the next sync corrects (I4).
	committed := false
	if token != "" {
		defer func() {
			if !committed {
				h.releaseQuota(ctx, username, rel, token)
			}
		}()
	}
	// Fence: an expired holder whose lease was taken over must not keep
	// mutating the file.
	if verr := lock.Verify(ctx); verr != nil {
		if errors.Is(verr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		writeHumanFileCodedError(w, http.StatusConflict, "lease_lost",
			"the path lease was taken over by another operation; nothing was written")
		return
	}
	// Publish: the same write path putFile uses without If-Match. Under
	// the lease the target cannot change in-API, so a plain put is safe;
	// the confirm snapshot below re-verifies the bytes that landed.
	if err := h.oss.PutObject(ctx, key, data); err != nil {
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "write file failed")
		return
	}
	committed = true
	h.commitQuota(ctx, username, rel, token)
	snapData, finalMeta, cerr := h.readFileSnapshot(ctx, key)
	if cerr != nil {
		if errors.Is(cerr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "confirm file write failed")
		return
	}
	if !bytes.Equal(snapData, data) {
		writeHumanFileCodedError(w, http.StatusConflict, "version_conflict",
			"the stored content does not match the bytes we wrote")
		return
	}
	// The staging object has served its purpose; removal is best effort —
	// a failure leaves an orphan that ages out with the staging TTL.
	_ = h.oss.DeleteObject(ctx, stageKey)
	// Remember the published version: retries are no-ops and a follow-up
	// presign keeps it as the Committed baseline.
	if err := h.writePresignCommitState(ctx, username, func(s *presignCommitState) {
		// The publish happened under the path lease, but a presign issued
		// while it was in flight is recorded outside that lease: overwriting
		// the entry unconditionally would erase that newer session and make
		// its commit look like a retry of ours (200 while GET still serves
		// our bytes). Advance the Committed baseline, keep the newer session.
		current := s.Commits[rel]
		if current.Session != "" && current.Session != entry.Session {
			current.Committed = finalMeta.ETag
			s.Commits[rel] = current
			return
		}
		s.Commits[rel] = presignCommitEntry{Committed: finalMeta.ETag}
	}); err != nil {
		if errors.Is(err, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "record presign commit failed")
		return
	}
	h.appendHumanChange(ctx, username, humanChangeEntry{
		Op: "put", Path: "files/" + rel, Size: finalMeta.Size, ETag: finalMeta.ETag,
	})
	h.recordAudit(ctx, a, "human_file_presign_commit",
		"path", "files/"+rel, "size", len(data), "oldSize", oldSize, "etag", finalMeta.ETag)
	httputil.WriteJSON(w, http.StatusOK, humanPresignCommitResponse{
		Path: "files/" + rel,
		Size: int64(len(data)),
		ETag: finalMeta.ETag,
	})
}

// finishCommittedRetry answers a commit whose session is already settled:
// idempotent 200 (no re-charge) while the final object is still the version
// the original commit published, 409 once that version is gone or replaced
// (a retry of a lost response must not re-publish or re-charge).
func (h *humanFilesHandler) finishCommittedRetry(w http.ResponseWriter, r *http.Request, a *humanFileAccess, username, rel, key, committedETag string) {
	ctx := r.Context()
	meta, merr := h.oss.StatMeta(ctx, key)
	if merr != nil && !errors.Is(merr, os.ErrNotExist) {
		// The retry check cannot answer on an unverifiable store: a
		// storage fault is 503, never the 409 a gone version would give.
		if errors.Is(merr, oss.ErrStorageUnavailable) {
			writeStorageUnavailable(w)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "stat file failed")
		return
	}
	if merr == nil && !isHumanFileTombstone(meta) && strings.EqualFold(meta.ETag, committedETag) {
		h.recordAudit(ctx, a, "human_file_presign_commit_idempotent",
			"path", "files/"+rel, "size", meta.Size, "etag", meta.ETag)
		httputil.WriteJSON(w, http.StatusOK, humanPresignCommitResponse{
			Path: "files/" + rel,
			Size: meta.Size,
			ETag: meta.ETag,
		})
		return
	}
	writeHumanFileCodedError(w, http.StatusConflict, "version_conflict",
		"the committed version is no longer at this path; issue a presign to upload again")
}
