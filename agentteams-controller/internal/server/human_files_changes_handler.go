package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Incremental change feed for the per-human file area: one change-log
// object per mutation, served as a resumable ?since= delta.
//
// Storage layout — a hidden area sibling of files/, .trash/ and .uploads/,
// so file listings, the file API, and the quota walk (which covers only
// files/ plus .trash/) never see change-log objects:
//
//	humans/<username>/.changes/<changeID>.json
//
// Each mutation appends exactly one entry:
//
//	{"op":"put|delete|restore|purge","path":"files/a.md","size":12,"etag":"...","ts":"<RFC3339>"}
//
// Write points: putFile and commitPresignedFile (put), deleteFile (delete, recorded only when this
// request performed the soft delete), restoreTrash (restore), purgeTrash
// (purge, restating the original files/ path from the trash metadata;
// skipped when the metadata carries no path). Appends are best-effort: a
// failure is logged and never fails the main operation — a lost entry
// costs the client at most one missed delta, repairable by the next
// resync, while the main operation's own durability is untouched.
//
// Read contract (GET /api/v1/humans/{name}/files/changes):
//
//   - The cursor is opaque to clients: "v1:<change id>". "v1:" (the empty
//     id) addresses the start of the surviving log. An explicit since that does not parse -> 400
//     {"code":"InvalidCursor"}. Without since, the HTTP route continues to
//     read the ordinary file named "changes".
//   - A cursor older than the oldest surviving entry -> 410
//     {"code":"CursorExpired","latest":"v1:<max surviving id>"} (bare
//     "v1:" when nothing survives). Retention prunes are always a prefix
//     of the ascending id order, so "id < oldest survivor" is exactly
//     "the cursor's entry is gone" for cursors this API issued; a client
//     that receives a 410 must retake a full snapshot and resume from
//     latest, because the delta after the expired cursor is incomplete.
//   - No changes after the cursor -> 200 with empty items/deleted and the
//     cursor returned verbatim (the client's steady-state zero-byte poll).
//   - put/restore entries surface in items (ascending change order) with
//     the file's current size and etag; delete entries surface as paths
//     in deleted (ascending change order). Purging a trash entry only
//     advances the cursor: it does not delete the live files/ path, which
//     may have been recreated since the soft delete. A path may appear in
//     both within one page (a delete followed by a recreate); the feed is
//     a log, not a state, and the client protocol applies it.
//   - changeID = 20 zero-padded UnixNano digits + 6 random hex digits:
//     lexicographic order equals chronological order, which the cursor
//     arithmetic and the retention walk both rely on. The random tail only
//     breaks same-nanosecond ties. No ULID dependency is introduced.
//
// Retention: at most humanChangeRetentionMax entries, each no older than
// humanChangeRetentionWindow. The write path enforces it with a best-effort
// walk that is time-gated per human so an append burst does not re-list
// the whole log on every write.

const (
	// humanChangeCursorPrefix versions the opaque cursor: "v1:<change id>".
	humanChangeCursorPrefix = "v1:"

	// humanChangeRetentionWindow and humanChangeRetentionMax bound the
	// per-human change log (the retention window): the most recent 7 days,
	// at most 20000 entries.
	humanChangeRetentionWindow = 7 * 24 * time.Hour
	humanChangeRetentionMax    = 20000

	// humanChangeDefaultLimit is the ?limit= default and the page size of
	// every change-log listing; it is also the hard cap because the S3
	// ListObjectsV2 MaxKeys ceiling (which ListObjectsRecursive passes
	// through) is 1000.
	humanChangeDefaultLimit = 1000

	// humanChangePruneInterval gates the per-human retention walk; entries
	// that fall out of the window between walks stay cursor-reachable
	// until the next walk.
	humanChangePruneInterval = 30 * time.Second
)

var humanChangeIDRe = regexp.MustCompile(`^[0-9]{20}[0-9a-f]{6}$`)

// humanChangesPrefix is the change-log prefix of one human's file area.
func humanChangesPrefix(username string) string { return humanAreaPrefix(username) + ".changes/" }

// newHumanChangeID builds a change-log sequence id: 20 zero-padded digits
// of the current UnixNano clock plus 6 random hex digits.
func newHumanChangeID() (string, error) {
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%020d%s", time.Now().UTC().UnixNano(), hex.EncodeToString(buf)), nil
}

// changeIDFromObjectName extracts the change id from a listed object name
// under the change-log prefix; anything that is not a well-formed
// <change id>.json is rejected so foreign objects in the prefix never
// enter the cursor arithmetic.
func changeIDFromObjectName(name string) (string, bool) {
	id, ok := strings.CutSuffix(name, ".json")
	if !ok || !humanChangeIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// parseHumanChangeCursor validates the ?since= cursor. Accepted: "v1:"
// (the initial cursor — the whole surviving log) and "v1:<change id>".
// Anything else, including a missing since, is malformed.
func parseHumanChangeCursor(since string) (string, bool) {
	id, ok := strings.CutPrefix(since, humanChangeCursorPrefix)
	if !ok {
		return "", false
	}
	if id == "" {
		return "", true
	}
	return id, humanChangeIDRe.MatchString(id)
}

// humanChangeEntry is one change-log object.
type humanChangeEntry struct {
	Op   string `json:"op"` // "put" | "delete" | "restore" | "purge"
	Path string `json:"path"`
	Size int64  `json:"size"`
	ETag string `json:"etag"`
	TS   string `json:"ts"` // RFC3339 UTC
}

// humanFileChangeItem is one present-state entry of the delta: after the
// change the file exists at Path with the reported size and etag (op
// "put" for a write, "restore" for a trash restore).
type humanFileChangeItem struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	ETag string `json:"etag"`
	Op   string `json:"op"`
}

// humanFileChangesResponse is the GET files/changes body. Items and
// Deleted are empty arrays, never null; Cursor is the cursor to resume
// from — the request's since verbatim when no entry advanced it.
type humanFileChangesResponse struct {
	Items   []humanFileChangeItem `json:"items"`
	Deleted []string              `json:"deleted"`
	Cursor  string                `json:"cursor"`
}

// humanFileCursorError is the stable body of the cursor rejections: 400
// code "InvalidCursor" (missing/malformed since) and 410 code
// "CursorExpired" with latest = the newest surviving cursor.
type humanFileCursorError struct {
	Code   string `json:"code"`
	Latest string `json:"latest,omitempty"`
}

func writeHumanFileCursorError(w http.ResponseWriter, status int, e humanFileCursorError) {
	httputil.WriteJSON(w, status, e)
}

// appendHumanChange records one file-area change in the per-human change
// log. Best-effort by contract: any failure (id generation, encoding,
// storage) is logged and never fails the main operation.
func (h *humanFilesHandler) appendHumanChange(ctx context.Context, username string, e humanChangeEntry) {
	id, err := newHumanChangeID()
	if err != nil {
		log.FromContext(ctx).Info("warning: change-log append skipped: could not generate a change id",
			"human", username, "op", e.Op, "error", err.Error())
		return
	}
	e.TS = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(e)
	if err != nil {
		log.FromContext(ctx).Info("warning: change-log append skipped: could not encode the entry",
			"human", username, "op", e.Op, "error", err.Error())
		return
	}
	key := humanChangesPrefix(username) + id + ".json"
	if err := h.oss.PutObject(ctx, key, data); err != nil {
		log.FromContext(ctx).Info("warning: change-log append failed; the change is missing from the delta feed",
			"human", username, "op", e.Op, "path", e.Path, "error", err.Error())
		return
	}
	h.pruneHumanChanges(ctx, username)
}

// pruneHumanChanges enforces the retention window with one ascending
// walk: the deletable set is the union of the age-expired prefix and the
// count-excess prefix, both prefixes of the ascending id order, so a
// single pass decides them. The walk is time-gated per human
// (changesPruneInterval) so an append burst does not re-list the whole
// log on every write; retention still holds with at most the gate's drift.
func (h *humanFilesHandler) pruneHumanChanges(ctx context.Context, username string) {
	now := time.Now()
	h.changesPruneMu.Lock()
	if last, ok := h.changesPruneAt[username]; ok && h.changesPruneInterval > 0 && now.Sub(last) < h.changesPruneInterval {
		h.changesPruneMu.Unlock()
		return
	}
	h.changesPruneAt[username] = now
	h.changesPruneMu.Unlock()

	prefix := humanChangesPrefix(username)
	// The age cut as a 20-digit id prefix: zero-padded timestamps compare
	// lexicographically as numbers.
	ageCut := fmt.Sprintf("%020d", now.Add(-humanChangeRetentionWindow).UnixNano())
	ids := make([]string, 0)
	startAfter := ""
	for {
		infos, next, err := h.oss.ListObjectsRecursive(ctx, prefix, startAfter, humanChangeDefaultLimit)
		if err != nil {
			log.FromContext(ctx).Info("warning: change-log retention walk failed",
				"human", username, "error", err.Error())
			return
		}
		for i := range infos {
			if id, ok := changeIDFromObjectName(infos[i].Name); ok {
				ids = append(ids, id)
			}
		}
		if next == "" {
			break
		}
		startAfter = next
	}
	dropCount := len(ids) - humanChangeRetentionMax
	if dropCount < 0 {
		dropCount = 0
	}
	for i, id := range ids {
		if i >= dropCount && id[:20] >= ageCut {
			continue
		}
		key := prefix + id + ".json"
		if err := h.oss.DeleteObject(ctx, key); err != nil {
			log.FromContext(ctx).Info("warning: change-log retention delete failed",
				"human", username, "key", key, "error", err.Error())
		}
	}
}

// humanChangesMinID returns the oldest surviving change id. The probe
// skips unparseable objects in the prefix instead of failing on them.
func (h *humanFilesHandler) humanChangesMinID(ctx context.Context, prefix string) (string, bool, error) {
	startAfter := ""
	for {
		infos, next, err := h.oss.ListObjectsRecursive(ctx, prefix, startAfter, 100)
		if err != nil {
			return "", false, err
		}
		for i := range infos {
			if id, ok := changeIDFromObjectName(infos[i].Name); ok {
				return id, true, nil
			}
		}
		if next == "" {
			return "", false, nil
		}
		startAfter = next
	}
}

// humanChangesLatest walks the change log to its end and returns the
// newest surviving cursor. It runs only on the 410 path, where the client
// must retake a full snapshot anyway; the walk is bounded by the
// retention cap (plus the prune gate's drift). A walk fault falls back to
// the bare initial cursor, which is still a valid resync point.
func (h *humanFilesHandler) humanChangesLatest(ctx context.Context, prefix string) string {
	startAfter := ""
	latest := ""
	for {
		infos, next, err := h.oss.ListObjectsRecursive(ctx, prefix, startAfter, humanChangeDefaultLimit)
		if err != nil {
			break
		}
		for i := range infos {
			if id, ok := changeIDFromObjectName(infos[i].Name); ok {
				latest = id
			}
		}
		if next == "" {
			break
		}
		startAfter = next
	}
	return humanChangeCursorPrefix + latest
}

// fileChanges handles GET /api/v1/humans/{name}/files/changes?since=<cursor>&limit=N.
//
// Authorization reuses the read-file matrix through the area root
// (self/manager/admin, plus the team-grant resolution a root listing
// gets): callers without access receive the same indistinguishable 404 as
// every other file-area read, so the feed cannot probe for humans. Each
// entry also checks descendant access, as a root listing does.
func (h *humanFilesHandler) fileChanges(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, status, msg := h.authorizeHumanFile(r.Context(), name, "", false, false)
	if a == nil {
		httputil.WriteError(w, status, msg)
		return
	}
	ctx := r.Context()
	username := a.human.Spec.EffectiveUsername(a.human.Name)
	prefix := humanChangesPrefix(username)

	since := r.URL.Query().Get("since")
	id, ok := parseHumanChangeCursor(since)
	if !ok {
		writeHumanFileCursorError(w, http.StatusBadRequest, humanFileCursorError{Code: "InvalidCursor"})
		return
	}
	limit := humanChangeDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			httputil.WriteError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > humanChangeDefaultLimit {
			n = humanChangeDefaultLimit
		}
		limit = n
	}

	// Cursor expiry: retention prunes the oldest entries, so a cursor
	// older than the oldest surviving entry addresses an incomplete delta
	// and is answered 410 with the newest surviving cursor as the resync
	// point. A non-empty cursor against an empty log means the whole
	// history the cursor belonged to has been pruned: the same answer.
	if id != "" {
		minID, found, err := h.humanChangesMinID(ctx, prefix)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "list change log failed")
			return
		}
		if !found {
			writeHumanFileCursorError(w, http.StatusGone, humanFileCursorError{Code: "CursorExpired", Latest: humanChangeCursorPrefix})
			return
		}
		if id < minID {
			writeHumanFileCursorError(w, http.StatusGone, humanFileCursorError{
				Code:   "CursorExpired",
				Latest: h.humanChangesLatest(ctx, prefix),
			})
			return
		}
	}

	startAfter := ""
	if id != "" {
		startAfter = prefix + id + ".json"
	}
	items := make([]humanFileChangeItem, 0)
	deleted := make([]string, 0)
	lastID := ""
	stop := false
	for len(items)+len(deleted) < limit && !stop {
		infos, next, err := h.oss.ListObjectsRecursive(ctx, prefix, startAfter, limit)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "list change log failed")
			return
		}
		for i := range infos {
			eid, ok := changeIDFromObjectName(infos[i].Name)
			if !ok {
				continue // a foreign object in the prefix; leave it, keep walking
			}
			data, gerr := h.oss.GetObject(ctx, prefix+infos[i].Name)
			if gerr != nil {
				// A read fault (or a retention-prune race) stops the walk
				// here: advancing the cursor past this entry would turn a
				// transient fault into a permanent miss. The next poll
				// resumes at the same point; a pruned key simply no longer
				// lists and the walk self-heals.
				stop = true
				break
			}
			var e humanChangeEntry
			if json.Unmarshal(data, &e) != nil {
				continue // corrupt content is permanent; skip it, cursor advances
			}
			rel, isFile := strings.CutPrefix(e.Path, "files/")
			if !isFile || !a.canReadPath(rel) {
				lastID = eid
				continue
			}
			switch e.Op {
			case "put", "restore":
				items = append(items, humanFileChangeItem{Path: e.Path, Size: e.Size, ETag: e.ETag, Op: e.Op})
			case "delete":
				if e.Path != "" {
					deleted = append(deleted, e.Path)
				}
			}
			lastID = eid
			if len(items)+len(deleted) >= limit {
				break
			}
		}
		if stop || next == "" || len(items)+len(deleted) >= limit {
			break
		}
		startAfter = next
	}
	if lastID == "" {
		// No change after the cursor: return the cursor verbatim so the
		// steady-state poll is a zero-byte round trip.
		httputil.WriteJSON(w, http.StatusOK, humanFileChangesResponse{Items: items, Deleted: deleted, Cursor: since})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, humanFileChangesResponse{Items: items, Deleted: deleted, Cursor: humanChangeCursorPrefix + lastID})
}
