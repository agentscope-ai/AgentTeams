package memory

import (
	"strconv"
	"strings"
	"time"
)

// Provenance records where an entry came from. The platform sets it at
// write time (never the model): the session/event that produced the
// entry plus the acting identities, so "who wrote what, for whom" stays
// auditable.
type Provenance struct {
	SessionID  string `json:"session_id,omitempty"`
	EventID    string `json:"event_id,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Runtime    string `json:"runtime,omitempty"`
	Actor      string `json:"actor,omitempty"`
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
}

// Entry is a memory entry under an anchor.
//
// Revision advances by one on every write (create, replace, forget,
// restore). ETag is the strong ETag of the current revision.
type Entry struct {
	ID          string     `json:"id"`
	Anchor      Anchor     `json:"anchor"`
	ContentType string     `json:"content_type"`
	Text        string     `json:"text"`
	Revision    uint64     `json:"revision"`
	ETag        string     `json:"etag"`
	Provenance  Provenance `json:"provenance"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// NewEntry is the payload for Store.Create.
type NewEntry struct {
	Anchor         Anchor
	ContentType    string
	Text           string
	Provenance     Provenance
	IdempotencyKey IdempotencyKey
}

// Patch is a partial entry update for Store.Replace: nil fields are
// left unchanged.
type Patch struct {
	ContentType *string
	Text        *string
	Provenance  *Provenance
}

// SearchQuery filters the live entries of one anchor. Anchor is
// mandatory: every search is scope-local, there is no global search.
type SearchQuery struct {
	Anchor Anchor
	Query  string // case-insensitive substring match; empty matches all
}

// ETagForRevision renders the strong ETag for a revision, e.g. "1".
func ETagForRevision(revision uint64) string {
	return `"` + strconv.FormatUint(revision, 10) + `"`
}

// ParseIfMatch parses an If-Match header value into the revision it
// names. The quoted form ("3") and the bare number (3) are accepted,
// with an optional W/ prefix. Anything else (including "*") is
// rejected: this API is compare-and-swap, the caller must name the
// current revision.
func ParseIfMatch(value string) (uint64, bool) {
	v := strings.TrimSpace(value)
	if strings.HasPrefix(v, "W/") {
		v = strings.TrimPrefix(v, "W/")
	}
	v = strings.Trim(v, `"`)
	revision, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return revision, true
}
