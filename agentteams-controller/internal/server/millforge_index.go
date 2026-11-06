// The millforge index proxy serves a read-only view of the Millforge
// release index (releases/channels.json) to the module-update UI. The
// index lives in a git repository on a public raw-content host (github
// or gitcode); an intranet deployment cannot reach those hosts from the
// browser, so the controller fetches the file and re-serves it.
//
// The handler is a re-wrapping proxy, not a pass-through: the upstream
// body is fetched with a 1 MiB cap, parsed as a JSON object, and its
// envelope fields validated (schema must be 1, channel must be
// non-empty, modules must be a JSON object). Only the validated fields
// are re-emitted, plus the request parameters (source, repo) and a
// fetched_at stamp. The whitelist IS the surface — any extra top-level
// field the upstream file gains is dropped, so a hostile or drifted
// upstream file cannot widen what the UI sees.
//
// The upstream host is allowlisted (raw.githubusercontent.com and
// raw.gitcode.com) and the composed URL is re-checked against the
// allowlist BEFORE anything is dialed: the source/repo/ref/channel
// parameters are regex-pinned, and the check is the last line of
// defense so a crafted parameter can never point the fetch at another
// host. Redirects off the original host are refused (CheckRedirect).
// Upstream faults are uniform and echo-free: a 404 maps to 404, every
// other non-200 maps to 502, a body over the cap is 502, malformed or
// off-schema JSON is 502 — in all cases the upstream body is discarded,
// never forwarded (it is an open upstream namespace; same discipline as
// the worker plugins proxy).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

const (
	// millforgeIndexTimeout bounds each upstream channels.json fetch.
	millforgeIndexTimeout = 10 * time.Second

	// millforgeIndexMaxBody caps the upstream channels.json body. The
	// read is capped at max+1 bytes so the (max+1)th byte is the
	// evidence for a 502; a silent truncation would hand the UI a broken
	// index as success.
	millforgeIndexMaxBody = 1 << 20

	// Defaults for the module-update UI's own channel feed.
	millforgeIndexDefaultRepo    = "LUOSENGWA/millforge"
	millforgeIndexDefaultRef     = "main"
	millforgeIndexDefaultChannel = "beta"
)

var (
	// millforgeChannels are the release channels the index proxy may
	// serve. Each channel has its own releases/<channel>/channels.json;
	// the artifact layout remains releases/<module>/<version>/<name>.
	millforgeChannels = map[string]bool{
		"alpha":  true,
		"beta":   true,
		"stable": true,
	}

	// millforgeRepoRE pins repo to `org/name`; each part is only
	// alphanumerics, dot, underscore, dash — no host metacharacters can
	// survive into the composed URL.
	millforgeRepoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

	// millforgeRefRE pins ref and channel names to a path-ish token.
	millforgeRefRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// millforgeIndexAllowedHosts is the production allowlist of raw-content
// hosts the index proxy may dial. GetIndex re-checks the composed URL
// against this map before issuing the request; a URL landing on any
// other host is refused. Tests retarget the fetch at an httptest server
// in the client transport, leaving this map untouched.
var millforgeIndexAllowedHosts = map[string]bool{
	"raw.githubusercontent.com": true,
	"raw.gitcode.com":           true,
}

// newMillforgeIndexClient builds the production upstream client: a 10s
// deadline per fetch plus a CheckRedirect that refuses any redirect off
// the original host (the raw-content hosts serve the file directly; a
// cross-host hop would smuggle the fetch past the allowlist).
func newMillforgeIndexClient() *http.Client {
	return &http.Client{
		Timeout: millforgeIndexTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// millforgeIndexClient is the shared production upstream client; NewMillforgeIndexHandler
// wires it into the handler. Tests inject their own client via the
// handler's client field.
var millforgeIndexClient = newMillforgeIndexClient()

// MillforgeIndexHandler serves GET /api/v1/millforge/index (see the file
// header for the contract).
type MillforgeIndexHandler struct {
	client *http.Client
}

// NewMillforgeIndexHandler wires the production upstream client. Tests
// may replace client with one whose transport retargets the fetch at an
// httptest.Server.
func NewMillforgeIndexHandler() *MillforgeIndexHandler {
	return &MillforgeIndexHandler{client: millforgeIndexClient}
}

// millforgeIndexURL composes the raw-content URL for the index file.
// source must already be validated to github|gitcode.
func millforgeIndexURL(source, repo, ref, channel string) string {
	if source == "gitcode" {
		return "https://raw.gitcode.com/" + repo + "/raw/" + ref + "/releases/" + channel + "/channels.json"
	}
	return "https://raw.githubusercontent.com/" + repo + "/" + ref + "/releases/" + channel + "/channels.json"
}

// millforgeIndexResponse is the exact response envelope: the validated
// upstream fields plus the request parameters and a fetch stamp.
type millforgeIndexResponse struct {
	Schema    int             `json:"schema"`
	Channel   string          `json:"channel"`
	Modules   json.RawMessage `json:"modules"`
	Source    string          `json:"source"`
	Repo      string          `json:"repo"`
	FetchedAt string          `json:"fetched_at"`
}

// GetIndex handles GET /api/v1/millforge/index.
func (h *MillforgeIndexHandler) GetIndex(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	source := q.Get("source")
	if source != "github" && source != "gitcode" {
		httputil.WriteError(w, http.StatusBadRequest, "millforge index: source must be github or gitcode")
		return
	}

	repo := q.Get("repo")
	if repo == "" {
		repo = millforgeIndexDefaultRepo
	}
	if !millforgeRepoRE.MatchString(repo) {
		httputil.WriteError(w, http.StatusBadRequest, "millforge index: invalid repo (expected org/name)")
		return
	}

	ref := q.Get("ref")
	if ref == "" {
		ref = millforgeIndexDefaultRef
	}
	if !millforgeRefRE.MatchString(ref) {
		httputil.WriteError(w, http.StatusBadRequest, "millforge index: invalid ref")
		return
	}

	channel := q.Get("channel")
	if channel == "" {
		channel = millforgeIndexDefaultChannel
	}
	if !millforgeRefRE.MatchString(channel) || !millforgeChannels[channel] {
		httputil.WriteError(w, http.StatusBadRequest, "millforge index: channel must be alpha, beta or stable")
		return
	}

	doc, status, message := h.fetchIndex(r.Context(), source, repo, ref, channel)
	if doc == nil {
		httputil.WriteError(w, status, message)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(millforgeIndexResponse{
		Schema:    doc.Schema,
		Channel:   doc.Channel,
		Modules:   doc.Modules,
		Source:    source,
		Repo:      repo,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// millforgeIndexDoc is the validated upstream index (schema/channel/modules).
type millforgeIndexDoc struct {
	Schema  int
	Channel string
	Modules json.RawMessage
}

// fetchIndex performs the validated fetch shared by the read route and the
// install routes (which re-read the index to pin the artifact checksum).
func (h *MillforgeIndexHandler) fetchIndex(ctx context.Context, source, repo, ref, channel string) (*millforgeIndexDoc, int, string) {
	if !millforgeChannels[channel] {
		return nil, http.StatusBadRequest, "millforge index: channel must be alpha, beta or stable"
	}
	rawURL := millforgeIndexURL(source, repo, ref, channel)

	u, err := url.Parse(rawURL)
	if err != nil || !millforgeIndexAllowedHosts[u.Host] {
		return nil, http.StatusBadGateway, "millforge index: upstream host not allowed"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, http.StatusBadGateway, "millforge index: upstream fetch failed"
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, http.StatusBadGateway, "millforge index: upstream fetch failed"
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusNotFound {
			status = http.StatusNotFound
		}
		return nil, status, "millforge index: upstream unavailable"
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, millforgeIndexMaxBody+1))
	if err != nil {
		return nil, http.StatusBadGateway, "millforge index: upstream body read failed"
	}
	if len(body) > millforgeIndexMaxBody {
		return nil, http.StatusBadGateway, "millforge index: upstream body too large"
	}

	var upstream struct {
		Schema  *int            `json:"schema"`
		Channel string          `json:"channel"`
		Modules json.RawMessage `json:"modules"`
	}
	if err := json.Unmarshal(body, &upstream); err != nil {
		return nil, http.StatusBadGateway, "millforge index: upstream body is not a JSON object"
	}
	if upstream.Schema == nil || *upstream.Schema != 1 {
		return nil, http.StatusBadGateway, "millforge index: unsupported upstream schema"
	}
	if upstream.Channel == "" {
		return nil, http.StatusBadGateway, "millforge index: upstream channel missing"
	}
	if channel != "" && upstream.Channel != channel {
		return nil, http.StatusNotFound, "millforge index: requested channel not present in the index"
	}
	modules := bytes.TrimSpace(upstream.Modules)
	if len(modules) == 0 || modules[0] != '{' {
		return nil, http.StatusBadGateway, "millforge index: upstream modules is not an object"
	}
	return &millforgeIndexDoc{Schema: *upstream.Schema, Channel: upstream.Channel, Modules: modules}, http.StatusOK, ""
}
