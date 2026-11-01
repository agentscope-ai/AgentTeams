// Package pc is the PowerContext backend adapter for the memory
// contract: a minimal HTTP client plus in-memory and HTTP fakes that
// satisfy memory.Store, so the contract suite and the HTTP layer can
// be exercised without a real PC deployment.
//
// Wire protocol (adapter contract, W1 maps the real PC on top of
// it):
//
//	POST   /entries                     create (anchor in body,
//	                                    optional Idempotency-Key header)
//	PUT    /entries/{id}?anchor=        replace (If-Match = strong ETag,
//	                                     anchor required)
//	DELETE /entries/{id}?anchor=        forget (If-Match, anchor required)
//	POST   /entries/{id}/restore?anchor= restore (If-Match, anchor required)
//	GET    /entries?anchor=&q=          search
//	GET    /export?anchor=              export
//
// The anchor is part of the entry key on the wire: a mutation for an
// id that exists only under a different anchor is a 404 from the
// backend, with no content of that entry.
//
// Upstream error mapping (contract-pinned): 412 and 428 both mean
// "CAS did not apply" and map to memory.ErrConflict carrying the
// upstream's current entry; 404 maps to memory.ErrNotFound;
// 401/403 map to memory.ErrForbidden; anything else is a generic
// error. A precondition failure whose body carries no usable current
// entry is a generic (malformed-upstream) error, never a partial
// trust of the body. All error messages are static: no request or
// body material is echoed.
package pc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
)

// Adapter is the PowerContext backend contract. It is intentionally
// method-for-method identical to memory.Store: a *Client satisfies
// both, and so do the fakes in this package.
type Adapter interface {
	Create(context.Context, memory.NewEntry) (memory.Entry, error)
	Replace(context.Context, memory.Anchor, string, uint64, memory.Patch) (memory.Entry, error)
	Forget(context.Context, memory.Anchor, string, uint64) error
	Restore(context.Context, memory.Anchor, string, uint64) (memory.Entry, error)
	Search(context.Context, memory.SearchQuery) ([]memory.Entry, error)
	Export(context.Context, memory.Anchor) ([]memory.Entry, error)
}

var (
	_ memory.Store = (*Client)(nil)
	_ Adapter      = (*Client)(nil)
)

// Client is a minimal PowerContext HTTP client.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Client for baseURL with a 30s-timeout transport.
func NewClient(baseURL string) *Client {
	return NewClientWithHTTP(baseURL, &http.Client{Timeout: 30 * time.Second})
}

// NewClientWithHTTP returns a Client using the given http client
// (tests inject an httptest transport).
func NewClientWithHTTP(baseURL string, hc *http.Client) *Client {
	return &Client{baseURL: baseURL, http: hc}
}

// --- wire types -----------------------------------------------------

type createRequest struct {
	Anchor      memory.Anchor     `json:"anchor"`
	ContentType string            `json:"content_type"`
	Text        string            `json:"text"`
	Provenance  memory.Provenance `json:"provenance"`
}

type replaceRequest struct {
	ContentType *string            `json:"content_type,omitempty"`
	Text        *string            `json:"text,omitempty"`
	Provenance  *memory.Provenance `json:"provenance,omitempty"`
}

type entriesResponse struct {
	Entries []memory.Entry `json:"entries"`
}

type errorResponse struct {
	Code    string        `json:"code"`
	Message string        `json:"message"`
	Current *memory.Entry `json:"current"`
}

// mapUpstreamError converts an upstream status+body into a domain
// error. Messages are static; the body is only used to extract the
// current entry on precondition failures.
func mapUpstreamError(status int, body []byte) error {
	switch status {
	case http.StatusPreconditionFailed, http.StatusPreconditionRequired:
		var er errorResponse
		if err := json.Unmarshal(body, &er); err != nil || er.Current == nil || er.Current.ID == "" || er.Current.Revision == 0 {
			return fmt.Errorf("pc: malformed precondition failure from upstream (status %d)", status)
		}
		return &memory.ErrConflict{Current: *er.Current}
	case http.StatusNotFound:
		return memory.ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return memory.ErrForbidden
	default:
		return fmt.Errorf("pc: unexpected upstream status %d", status)
	}
}

// do performs one JSON request. body is JSON-encoded when non-nil,
// out is JSON-decoded on 2xx with a body. Non-2xx goes through
// mapUpstreamError.
func (c *Client) do(ctx context.Context, method, path string, headers map[string]string, body, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return err
			}
		}
		return nil
	}
	return mapUpstreamError(resp.StatusCode, data)
}

// --- memory.Store ---------------------------------------------------

// Create creates an entry under in.Anchor.
func (c *Client) Create(ctx context.Context, in memory.NewEntry) (memory.Entry, error) {
	if !in.Anchor.Valid() {
		return memory.Entry{}, memory.ErrInvalidAnchor
	}
	var entry memory.Entry
	headers := make(map[string]string)
	if in.IdempotencyKey.IsSet() {
		headers["Idempotency-Key"] = string(in.IdempotencyKey)
	}
	err := c.do(ctx, http.MethodPost, "/entries", headers, createRequest{
		Anchor:      in.Anchor,
		ContentType: in.ContentType,
		Text:        in.Text,
		Provenance:  in.Provenance,
	}, &entry)
	return entry, err
}

// entryPath renders the wire path of a mutation: the anchor is part
// of the entry key and rides the query (path segments are reserved
// for the id).
func entryPath(id string, anchor memory.Anchor, suffix string) string {
	return "/entries/" + url.PathEscape(id) + suffix +
		"?anchor=" + url.QueryEscape(anchor.String())
}

// Replace updates the entry named by (anchor, id), CAS on ifRevision.
func (c *Client) Replace(ctx context.Context, anchor memory.Anchor, id string, ifRevision uint64, patch memory.Patch) (memory.Entry, error) {
	if !anchor.Valid() {
		return memory.Entry{}, memory.ErrInvalidAnchor
	}
	var entry memory.Entry
	err := c.do(ctx, http.MethodPut, entryPath(id, anchor, ""),
		map[string]string{"If-Match": memory.ETagForRevision(ifRevision)},
		replaceRequest{ContentType: patch.ContentType, Text: patch.Text, Provenance: patch.Provenance},
		&entry)
	return entry, err
}

// Forget forgets the entry named by (anchor, id), CAS on ifRevision.
func (c *Client) Forget(ctx context.Context, anchor memory.Anchor, id string, ifRevision uint64) error {
	if !anchor.Valid() {
		return memory.ErrInvalidAnchor
	}
	return c.do(ctx, http.MethodDelete, entryPath(id, anchor, ""),
		map[string]string{"If-Match": memory.ETagForRevision(ifRevision)},
		nil, nil)
}

// Restore revives a forgotten entry named by (anchor, id), CAS on the
// tombstone revision.
func (c *Client) Restore(ctx context.Context, anchor memory.Anchor, id string, ifRevision uint64) (memory.Entry, error) {
	if !anchor.Valid() {
		return memory.Entry{}, memory.ErrInvalidAnchor
	}
	var entry memory.Entry
	err := c.do(ctx, http.MethodPost, entryPath(id, anchor, "/restore"),
		map[string]string{"If-Match": memory.ETagForRevision(ifRevision)},
		nil, &entry)
	return entry, err
}

// Search lists the live matching entries of one anchor.
func (c *Client) Search(ctx context.Context, q memory.SearchQuery) ([]memory.Entry, error) {
	if !q.Anchor.Valid() {
		return nil, memory.ErrInvalidAnchor
	}
	params := url.Values{}
	params.Set("anchor", q.Anchor.String())
	params.Set("q", q.Query)
	var res entriesResponse
	if err := c.do(ctx, http.MethodGet, "/entries?"+params.Encode(), nil, nil, &res); err != nil {
		return nil, err
	}
	return res.Entries, nil
}

// Export lists all live entries of one anchor.
func (c *Client) Export(ctx context.Context, anchor memory.Anchor) ([]memory.Entry, error) {
	if !anchor.Valid() {
		return nil, memory.ErrInvalidAnchor
	}
	params := url.Values{}
	params.Set("anchor", anchor.String())
	var res entriesResponse
	if err := c.do(ctx, http.MethodGet, "/export?"+params.Encode(), nil, nil, &res); err != nil {
		return nil, err
	}
	return res.Entries, nil
}
