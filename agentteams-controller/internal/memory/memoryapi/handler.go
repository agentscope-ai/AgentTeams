// Package memoryapi is the HTTP contract layer of the Memory API
// (#1412 W0a). The handlers are tested with httptest against the
// contract suite, but are intentionally NOT registered on the
// controller's server mux: registration, authn and actor authz are
// the W1 gateway's job.
//
// Routes:
//
//	POST   /memory/{anchor}            create (optional Idempotency-Key)
//	PUT    /memory/{anchor}/{id}       replace (If-Match required)
//	DELETE /memory/{anchor}/{id}       forget   (If-Match required)
//	POST   /memory/{anchor}/{id}/restore restore (If-Match required)
//	GET    /memory/{anchor}?q=         search
//	GET    /memory/{anchor}/export     export
//
// Status contract: 201 create (+ETag), 200 replace/restore/search/
// export (+ETag on writes), 204 forget, 428 missing/unparsable
// If-Match, 409 CAS conflict (body carries current), 404 unknown or
// forgotten entry, 403 scope forbidden, 400 bad request, 500
// anything else. Every error body is the fixed envelope
// {"code","message","current"?:{revision,etag,text}} with static
// messages: request material and store errors are never echoed.
package memoryapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
)

// Server exposes the memory HTTP contract over a memory.Store.
type Server struct {
	store memory.Store
}

// New returns a Server.
func New(store memory.Store) *Server { return &Server{store: store} }

// Handler builds the route mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /memory/{anchor}", s.handleCreate)
	mux.HandleFunc("PUT /memory/{anchor}/{id}", s.handleReplace)
	mux.HandleFunc("DELETE /memory/{anchor}/{id}", s.handleForget)
	mux.HandleFunc("POST /memory/{anchor}/{id}/restore", s.handleRestore)
	mux.HandleFunc("GET /memory/{anchor}", s.handleSearch)
	mux.HandleFunc("GET /memory/{anchor}/export", s.handleExport)
	return mux
}

// --- wire shapes ----------------------------------------------------

type createBody struct {
	ContentType string             `json:"content_type"`
	Text        string             `json:"text"`
	Provenance  *memory.Provenance `json:"provenance"`
}

type replaceBody struct {
	ContentType *string            `json:"content_type"`
	Text        *string            `json:"text"`
	Provenance  *memory.Provenance `json:"provenance"`
}

type entriesEnvelope struct {
	Entries []memory.Entry `json:"entries"`
}

// errorEnvelope is the fixed error body shape.
type errorEnvelope struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Current *currentView `json:"current,omitempty"`
}

// currentView is the conflict/428 "current" view: just enough to
// rebase a write (the full entry is retrievable via the API).
type currentView struct {
	Revision uint64 `json:"revision"`
	ETag     string `json:"etag"`
	Text     string `json:"text"`
}

func currentOf(e memory.Entry) *currentView {
	return &currentView{Revision: e.Revision, ETag: e.ETag, Text: e.Text}
}

// --- plumbing --------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func badRequest(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, errorEnvelope{Code: "bad_request", Message: message})
}

// parseAnchorPath parses the {anchor} path segment or renders 400.
func parseAnchorPath(w http.ResponseWriter, r *http.Request) (memory.Anchor, bool) {
	anchor, err := memory.ParseAnchor(r.PathValue("anchor"))
	if err != nil {
		badRequest(w, "invalid anchor")
		return memory.Anchor{}, false
	}
	return anchor, true
}

// requireIfMatch parses the If-Match header or renders 428
// (Precondition Required): this API is compare-and-swap, the caller
// must name the current revision.
func requireIfMatch(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	revision, ok := memory.ParseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		writeJSON(w, http.StatusPreconditionRequired, errorEnvelope{
			Code:    "precondition_required",
			Message: "If-Match header with the current revision is required",
		})
		return 0, false
	}
	return revision, true
}

// writeStoreError maps a store error onto the fixed envelope. The
// default branch is static on purpose: store errors may carry
// request material, so they are never echoed.
func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case memory.IsConflict(err):
		c := memory.ConflictOf(err)
		writeJSON(w, http.StatusConflict, errorEnvelope{
			Code:    "conflict",
			Message: "revision conflict",
			Current: currentOf(c.Current),
		})
	case errors.Is(err, memory.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorEnvelope{Code: "not_found", Message: "entry not found"})
	case errors.Is(err, memory.ErrForbidden):
		writeJSON(w, http.StatusForbidden, errorEnvelope{Code: "forbidden", Message: "access forbidden"})
	default:
		writeJSON(w, http.StatusInternalServerError, errorEnvelope{Code: "internal", Message: "internal error"})
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst); err != nil {
		badRequest(w, "malformed JSON body")
		return false
	}
	return true
}

// --- handlers --------------------------------------------------------

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	anchor, ok := parseAnchorPath(w, r)
	if !ok {
		return
	}
	var body createBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ContentType == "" || body.Text == "" {
		badRequest(w, "content_type and text are required")
		return
	}
	key, err := memory.ParseIdempotencyKey(r.Header.Get("Idempotency-Key"))
	if err != nil {
		badRequest(w, "invalid Idempotency-Key")
		return
	}
	var prov memory.Provenance
	if body.Provenance != nil {
		prov = *body.Provenance
	}
	entry, err := s.store.Create(r.Context(), memory.NewEntry{
		Anchor:         anchor,
		ContentType:    body.ContentType,
		Text:           body.Text,
		Provenance:     prov,
		IdempotencyKey: key,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.Header().Set("ETag", entry.ETag)
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) handleReplace(w http.ResponseWriter, r *http.Request) {
	if _, ok := parseAnchorPath(w, r); !ok {
		return
	}
	revision, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	var body replaceBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ContentType == nil && body.Text == nil && body.Provenance == nil {
		badRequest(w, "patch must set content_type, text or provenance")
		return
	}
	entry, err := s.store.Replace(r.Context(), r.PathValue("id"), revision, memory.Patch{
		ContentType: body.ContentType,
		Text:        body.Text,
		Provenance:  body.Provenance,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.Header().Set("ETag", entry.ETag)
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	if _, ok := parseAnchorPath(w, r); !ok {
		return
	}
	revision, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	if err := s.store.Forget(r.Context(), r.PathValue("id"), revision); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if _, ok := parseAnchorPath(w, r); !ok {
		return
	}
	revision, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	entry, err := s.store.Restore(r.Context(), r.PathValue("id"), revision)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.Header().Set("ETag", entry.ETag)
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	anchor, ok := parseAnchorPath(w, r)
	if !ok {
		return
	}
	entries, err := s.store.Search(r.Context(), memory.SearchQuery{
		Anchor: anchor,
		Query:  r.URL.Query().Get("q"),
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if entries == nil {
		entries = []memory.Entry{}
	}
	writeJSON(w, http.StatusOK, entriesEnvelope{Entries: entries})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	anchor, ok := parseAnchorPath(w, r)
	if !ok {
		return
	}
	entries, err := s.store.Export(r.Context(), anchor)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if entries == nil {
		entries = []memory.Entry{}
	}
	writeJSON(w, http.StatusOK, entriesEnvelope{Entries: entries})
}
