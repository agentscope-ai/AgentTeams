// GET /api/v1/audit — the read-only workbench view of the durable audit
// store written by the capability-foundation audit client (#1220 §8,
// #1245). Read-only: no mutations, no deletes, no live tailing in v1.
//
// Scope (same rules as the worker/team read APIs):
//   - admin / manager: full scope; ?team= narrows by the event's target
//     team. Events without a target team (e.g. capability changes on a
//     human) appear only in unscoped queries.
//   - scoped readers (team leader, L2 human): ?team= is mandatory; a
//     cross-team read is hidden as 404 (W8 anti-probing), never 403.
//
// Degradation: a storage failure or a malformed audit object degrades to
// 502 with the standard error envelope — never a partial silent list. A
// day with no object is normal (zero events, not an error).
package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

const (
	auditDefaultLimit = 50
	auditMaxLimit     = 200
)

// AuditHandler serves the read-only audit query endpoint.
type AuditHandler struct {
	q *audit.Query
}

// NewAuditHandler creates the audit query handler over the same storage
// client the writer uses (read-only access).
func NewAuditHandler(sc oss.StorageClient) *AuditHandler {
	return &AuditHandler{q: audit.NewQuery(sc)}
}

// auditEventResponse is one audit event in the list response. Values
// follow the #1220 §8 contract: before/after where defined, never secret
// values (the writer guarantees that on Event; this is a field-for-field
// projection plus the derived kind).
type auditEventResponse struct {
	Ts         string   `json:"ts"`
	Kind       string   `json:"kind"`
	Actor      string   `json:"actor"`
	Target     string   `json:"target,omitempty"`
	TargetTeam string   `json:"targetTeam,omitempty"`
	Action     string   `json:"action"`
	Capability string   `json:"capability,omitempty"`
	Before     []string `json:"before,omitempty"`
	After      []string `json:"after,omitempty"`
	Detail     string   `json:"detail,omitempty"`
}

// auditListResponse is the paged list envelope.
type auditListResponse struct {
	Events []auditEventResponse `json:"events"`
	Cursor string               `json:"cursor,omitempty"`
}

// List implements GET /api/v1/audit.
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	team := q.Get("team")
	if caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleManager {
		// Scoped reader (team leader or L2 human): the team scope is
		// mandatory and enforced here; the middleware cannot resolve it.
		if team == "" {
			httputil.WriteError(w, http.StatusBadRequest,
				"team scope required: pass ?team= (unscoped audit queries are L1 only)")
			return
		}
		if !caller.TeamMatches(team) {
			httputil.WriteError(w, http.StatusNotFound, "audit: not found")
			return
		}
	}

	from, err := parseAuditTime(q.Get("from"))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid from: "+err.Error())
		return
	}
	to, err := parseAuditTime(q.Get("to"))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid to: "+err.Error())
		return
	}

	limit := auditDefaultLimit
	if s := q.Get("limit"); s != "" {
		l, convErr := strconv.Atoi(s)
		if convErr != nil || l < 1 || l > auditMaxLimit {
			httputil.WriteError(w, http.StatusBadRequest,
				fmt.Sprintf("limit must be an integer between 1 and %d", auditMaxLimit))
			return
		}
		limit = l
	}

	var cursor *audit.Cursor
	if c := q.Get("cursor"); c != "" {
		cursor, err = audit.DecodeCursor(c)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid cursor: "+err.Error())
			return
		}
	}

	res, err := h.q.List(r.Context(), audit.QueryOptions{
		Team:   team,
		Kind:   q.Get("kind"),
		From:   from,
		To:     to,
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		switch {
		case errors.Is(err, audit.ErrStorageUnavailable), errors.Is(err, audit.ErrMalformedObject):
			httputil.WriteError(w, http.StatusBadGateway, "audit store unavailable: "+err.Error())
			return
		case errors.Is(err, audit.ErrRangeTooLarge):
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		default:
			httputil.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	out := auditListResponse{Events: make([]auditEventResponse, 0, len(res.Events))}
	for _, ev := range res.Events {
		out.Events = append(out.Events, auditEventResponse{
			Ts:         ev.When.UTC().Format(time.RFC3339Nano),
			Kind:       audit.KindOf(ev.Action),
			Actor:      ev.Who,
			Target:     ev.Target,
			TargetTeam: ev.TargetTeam,
			Action:     ev.Action,
			Capability: ev.Capability,
			Before:     ev.Before,
			After:      ev.After,
			Detail:     ev.Detail,
		})
	}
	if res.NextCursor != nil {
		if enc, encErr := res.NextCursor.Encode(); encErr == nil {
			out.Cursor = enc
		}
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}

// parseAuditTime parses an RFC3339 bound; empty = unbounded.
func parseAuditTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a valid RFC3339 timestamp", s)
	}
	return t, nil
}
