package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestMillforgeIndexURL pins the raw-content URL construction for both
// sources. The gitcode layout puts /raw/<ref>/ after the repo, and the
// host is raw.gitcode.com — api.gitcode.com is the API surface and must
// never appear in a constructed URL.
func TestMillforgeIndexURL(t *testing.T) {
	tests := []struct {
		source, repo, ref, want string
	}{
		{"github", "LUOSENGWA/millforge", "main",
			"https://raw.githubusercontent.com/LUOSENGWA/millforge/main/releases/channels.json"},
		{"github", "acme/forge-x", "v1.2.0",
			"https://raw.githubusercontent.com/acme/forge-x/v1.2.0/releases/channels.json"},
		{"gitcode", "LUOSENGWA/millforge", "main",
			"https://raw.gitcode.com/LUOSENGWA/millforge/raw/main/releases/channels.json"},
		{"gitcode", "acme/forge-x", "refs/tags/v1.2.0",
			"https://raw.gitcode.com/acme/forge-x/raw/refs/tags/v1.2.0/releases/channels.json"},
	}
	for _, tt := range tests {
		got := millforgeIndexURL(tt.source, tt.repo, tt.ref)
		if got != tt.want {
			t.Fatalf("%s %s@%s: got %s, want %s", tt.source, tt.repo, tt.ref, got, tt.want)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("%s: unparseable URL %q: %v", tt.source, got, err)
		}
		if tt.source == "gitcode" {
			if u.Host != "raw.gitcode.com" {
				t.Fatalf("gitcode host: got %q, want raw.gitcode.com", u.Host)
			}
			if strings.Contains(got, "api.gitcode.com") {
				t.Fatalf("gitcode URL must not reference the API host: %q", got)
			}
		}
	}
}

// millforgeTestTransport retargets every request at the local httptest
// server while preserving the request path. The URL the handler composed
// (and allowlist-checked) keeps its original https://raw.* host, so the
// pre-dial allowlist gate runs unmodified and nothing leaves the
// loopback interface.
type millforgeTestTransport struct {
	target string
}

func (m millforgeTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = m.target
	clone.Host = m.target
	return http.DefaultTransport.RoundTrip(clone)
}

// newMillforgeIndexTestHandler builds the handler with an injected
// client whose transport retargets the fetch at a fresh httptest.Server.
func newMillforgeIndexTestHandler(t *testing.T, upstream http.HandlerFunc) *MillforgeIndexHandler {
	t.Helper()
	ts := httptest.NewServer(upstream)
	t.Cleanup(ts.Close)
	return &MillforgeIndexHandler{
		client: &http.Client{
			Timeout:   5 * time.Second,
			Transport: millforgeTestTransport{target: ts.Listener.Addr().String()},
		},
	}
}

// TestMillforgeIndexHandler drives GetIndex end to end against a local
// upstream: the valid envelope, the gitcode path layout, and the four
// failure shapes (missing schema, malformed JSON, 404, oversized body).
// The sentinel must never survive into a non-200 response (echo-free
// contract).
func TestMillforgeIndexHandler(t *testing.T) {
	validBody := `{"schema":1,"channel":"beta","modules":{"core":{"version":"1.4.2"},"ui":{"version":"0.9.0"}}}`
	sentinel := "upstream-body-must-never-echo"

	tests := []struct {
		name       string
		query      string
		upstream   http.HandlerFunc
		wantStatus int
	}{
		{
			name:       "valid github index",
			query:      "source=github",
			upstream:   func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, validBody) },
			wantStatus: http.StatusOK,
		},
		{
			name:  "gitcode layout reaches the right path",
			query: "source=gitcode",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/LUOSENGWA/millforge/raw/main/releases/channels.json" {
					t.Errorf("upstream path: got %q, want /LUOSENGWA/millforge/raw/main/releases/channels.json", r.URL.Path)
				}
				io.WriteString(w, validBody)
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing schema",
			query:      "source=github",
			upstream:   func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"channel":"beta","modules":{}}`) },
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "malformed json",
			query:      "source=github",
			upstream:   func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"schema":1,"channel":"beta","modules":{`) },
			wantStatus: http.StatusBadGateway,
		},
		{
			name:  "upstream 404",
			query: "source=github",
			upstream: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, sentinel, http.StatusNotFound)
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:  "oversized body",
			query: "source=github",
			upstream: func(w http.ResponseWriter, _ *http.Request) {
				// 1 MiB + 16 bytes: over the cap, must be 502, never a
				// silent truncation.
				w.Write(make([]byte, millforgeIndexMaxBody+16))
			},
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newMillforgeIndexTestHandler(t, tt.upstream)
			rec := httptest.NewRecorder()
			h.GetIndex(rec, httptest.NewRequest(http.MethodGet, "/api/v1/millforge/index?"+tt.query, nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status=%d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), sentinel) {
				t.Fatal("upstream body echoed into the response")
			}
			if rec.Code == http.StatusOK {
				assertMillforgeEnvelope(t, tt.query, rec.Body.Bytes())
			}
		})
	}
}

// assertMillforgeEnvelope checks a 200 response carries exactly the
// whitelisted envelope: the validated upstream fields, the source/repo
// echo, and an RFC3339 fetch stamp.
func assertMillforgeEnvelope(t *testing.T, query string, body []byte) {
	t.Helper()
	wantSource := "github"
	if strings.Contains(query, "source=gitcode") {
		wantSource = "gitcode"
	}
	var got struct {
		Schema    int             `json:"schema"`
		Channel   string          `json:"channel"`
		Modules   json.RawMessage `json:"modules"`
		Source    string          `json:"source"`
		Repo      string          `json:"repo"`
		FetchedAt string          `json:"fetched_at"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if got.Schema != 1 {
		t.Fatalf("schema: got %d, want 1", got.Schema)
	}
	if got.Channel != "beta" {
		t.Fatalf("channel: got %q, want beta", got.Channel)
	}
	if len(got.Modules) == 0 || got.Modules[0] != '{' {
		t.Fatalf("modules: not a JSON object: %s", got.Modules)
	}
	if got.Source != wantSource {
		t.Fatalf("source: got %q, want %q", got.Source, wantSource)
	}
	if got.Repo != millforgeIndexDefaultRepo {
		t.Fatalf("repo: got %q, want %q", got.Repo, millforgeIndexDefaultRepo)
	}
	if _, err := time.Parse(time.RFC3339, got.FetchedAt); err != nil {
		t.Fatalf("fetched_at %q is not RFC3339: %v", got.FetchedAt, err)
	}
}
