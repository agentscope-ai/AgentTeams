//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
	klog "github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/log"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/modules/resources"
)

// staticReviewer authenticates every bearer token as the given identity:
// the API layer is exercised end-to-end without a controller.
type staticReviewer struct{ id *identity.Identity }

func (s staticReviewer) TokenReview(ctx context.Context, token string) (*identity.Identity, error) {
	return s.id, nil
}

func newAPI(t *testing.T, h *harness, id *identity.Identity) http.Handler {
	t.Helper()
	lg, err := klog.New("error", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	handler := &resources.Handler{
		Store:          h.store,
		Idem:           h.idem,
		Log:            lg,
		DefaultRuntime: "qwenpaw",
	}
	authed := identity.Middleware(identity.NewCache(staticReviewer{id: id}))
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/workers", authed(handler.CreateHTTP()))
	mux.Handle("GET /api/v1/workers", authed(handler.ListHTTP()))
	mux.Handle("GET /api/v1/workers/{name}", authed(handler.GetHTTP()))
	return mux
}

func do(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer token")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAPIWorkersLifecycle(t *testing.T) {
	h := newHarness(t)
	api := newAPI(t, h, &identity.Identity{Role: identity.RoleAdmin, Username: "admin"})

	rec := do(t, api, http.MethodPost, "/api/v1/workers", `{"name":"api-w1","model":"qwen3-coder","runtime":"qwenpaw"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"api-w1"`) ||
		!strings.Contains(rec.Body.String(), `"phase":"Pending"`) {
		t.Fatalf("create body = %s", rec.Body.String())
	}

	rec = do(t, api, http.MethodGet, "/api/v1/workers", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":1`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, api, http.MethodGet, "/api/v1/workers/api-w1", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"api-w1"`) {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, api, http.MethodGet, "/api/v1/workers/missing", "", nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
		t.Fatalf("get missing = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPIValidationAndAuth(t *testing.T) {
	h := newHarness(t)
	admin := newAPI(t, h, &identity.Identity{Role: identity.RoleAdmin, Username: "admin"})

	cases := []struct {
		name string
		body string
		want int
		code string
	}{
		{"bad name", `{"name":"Bad_Name"}`, http.StatusBadRequest, "invalid_name"},
		{"copaw", `{"name":"api-w2","runtime":"copaw"}`, http.StatusBadRequest, "deprecated_runtime"},
		{"dup", `{"name":"api-w3"}`, http.StatusCreated, ""},
		{"dup again", `{"name":"api-w3"}`, http.StatusConflict, "already_exists"},
	}
	for _, tc := range cases {
		rec := do(t, admin, http.MethodPost, "/api/v1/workers", tc.body, nil)
		if rec.Code != tc.want {
			t.Fatalf("%s = %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if tc.code != "" && !strings.Contains(rec.Body.String(), `"`+tc.code+`"`) {
			t.Fatalf("%s body = %s", tc.name, rec.Body.String())
		}
	}

	// A human cannot create (frozen matrix): 403 forbidden.
	human := newAPI(t, h, &identity.Identity{Role: identity.RoleHuman, Username: "h1", Teams: []string{"t1"}})
	rec := do(t, human, http.MethodPost, "/api/v1/workers", `{"name":"api-w4"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("human create = %d %s", rec.Code, rec.Body.String())
	}

	// Missing bearer token: 401 before the handler.
	rec = do(t, admin, http.MethodGet, "/api/v1/workers", "", map[string]string{"Authorization": ""})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d %s", rec.Code, rec.Body.String())
	}
}

// TestAPIRequestBodyLimit bounds memory: a body over the 1 MiB kernel limit
// must be rejected with the normal 400 envelope, not buffered or 500.
func TestAPIRequestBodyLimit(t *testing.T) {
	h := newHarness(t)
	api := newAPI(t, h, &identity.Identity{Role: identity.RoleAdmin, Username: "admin"})
	body := `{"name":"big-body","workerName":"` + strings.Repeat("x", 1<<20) + `"}`
	rec := do(t, api, http.MethodPost, "/api/v1/workers", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatalf("envelope = %s, want invalid_request", rec.Body.String())
	}
	if n := h.count(t, "res.workers"); n != 0 {
		t.Fatalf("workers = %d, want 0", n)
	}
}

// TestAPIFieldTooLong: a value that exceeds its column width is a client
// error (400), never a 500 from the store.
func TestAPIFieldTooLong(t *testing.T) {
	h := newHarness(t)
	api := newAPI(t, h, &identity.Identity{Role: identity.RoleAdmin, Username: "admin"})
	body := fmt.Sprintf(`{"name":"long-field","workerName":%q}`, strings.Repeat("x", 300))
	rec := do(t, api, http.MethodPost, "/api/v1/workers", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long workerName = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "field value too long") {
		t.Fatalf("envelope = %s, want field-too-long message", rec.Body.String())
	}
	if n := h.count(t, "res.workers"); n != 0 {
		t.Fatalf("workers = %d, want 0", n)
	}
}

// TestAPILongClientHeaders pins the audit-column truncation: a long
// X-Request-Id / User-Agent must not make the create transaction fail with a
// 1406 (which would surface as a 500), and the stored row keeps the column
// widths.
func TestAPILongClientHeaders(t *testing.T) {
	h := newHarness(t)
	api := newAPI(t, h, &identity.Identity{Role: identity.RoleAdmin, Username: "admin"})
	rec := do(t, api, http.MethodPost, "/api/v1/workers",
		`{"name":"long-hdr"}`,
		map[string]string{
			"X-Request-Id": strings.Repeat("r", 300),
			"User-Agent":   strings.Repeat("u", 500),
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with long headers = %d %s", rec.Code, rec.Body.String())
	}
	var reqID, client string
	if err := h.res.QueryRow("SELECT request_id, client FROM ops.audit_events WHERE action = 'worker.create' ORDER BY id DESC LIMIT 1").Scan(&reqID, &client); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if len([]rune(reqID)) > 128 || len([]rune(client)) > 191 {
		t.Fatalf("audit columns not truncated: request_id=%d client=%d", len([]rune(reqID)), len([]rune(client)))
	}
	if reqID == "" || client == "" {
		t.Fatalf("audit columns empty: %q / %q", reqID, client)
	}
}

// TestHTTPConcurrentCreateSameName: concurrent duplicate creates return
// exactly one 201 and 409s, never the old reclaim deadlock/500.
func TestHTTPConcurrentCreateSameName(t *testing.T) {
	h := newHarness(t)
	api := newAPI(t, h, &identity.Identity{Role: identity.RoleAdmin, Username: "admin"})
	const n = 10
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(t, api, http.MethodPost, "/api/v1/workers", `{"name":"http-same"}`, nil)
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)

	var created, conflict int
	for code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 || conflict != n-1 {
		t.Fatalf("created=%d conflict=%d, want 1/%d", created, conflict, n-1)
	}
	if got := h.count(t, "res.workers"); got != 1 {
		t.Fatalf("workers = %d, want 1", got)
	}
}

// TestHTTPConcurrentCreates drives 15 creates through the full HTTP stack
// (identity middleware + handler + store) against a real database. It checks
// that the pool, per-request transactions and response encoding hold up
// under concurrency, and that every row is visible afterwards.
func TestHTTPConcurrentCreates(t *testing.T) {
	h := newHarness(t)
	api := newAPI(t, h, &identity.Identity{Role: identity.RoleAdmin, Username: "admin"})

	const n = 15
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"name":"http-conc-%02d","model":"m"}`, i)
			rec := do(t, api, http.MethodPost, "/api/v1/workers", body, nil)
			if rec.Code != http.StatusCreated {
				errs <- fmt.Sprintf("create %d = %d %s", i, rec.Code, rec.Body.String())
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}

	rec := do(t, api, http.MethodGet, "/api/v1/workers", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if out.Total != n {
		t.Fatalf("total = %d, want %d", out.Total, n)
	}
}

func TestAPIListETagAndIdempotency(t *testing.T) {
	h := newHarness(t)
	api := newAPI(t, h, &identity.Identity{Role: identity.RoleAdmin, Username: "admin"})

	if rec := do(t, api, http.MethodPost, "/api/v1/workers", `{"name":"api-w5"}`, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	specURL := "/api/v1/workers?fields=spec"
	first := do(t, api, http.MethodGet, specURL, "", nil)
	if first.Code != http.StatusOK || first.Header().Get("ETag") == "" {
		t.Fatalf("spec list = %d etag=%q", first.Code, first.Header().Get("ETag"))
	}
	second := do(t, api, http.MethodGet, specURL, "", map[string]string{"If-None-Match": first.Header().Get("ETag")})
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 {
		t.Fatalf("304 = %d body=%q", second.Code, second.Body.String())
	}
	status := do(t, api, http.MethodGet, "/api/v1/workers?fields=status", "", nil)
	if status.Header().Get("ETag") != "" {
		t.Fatalf("status list must not carry an ETag")
	}

	// Idempotency-Key: the second call replays the first response byte for byte.
	const body = `{"name":"api-w6","model":"m"}`
	headers := map[string]string{"Idempotency-Key": "idem-1"}
	a := do(t, api, http.MethodPost, "/api/v1/workers", body, headers)
	b := do(t, api, http.MethodPost, "/api/v1/workers", body, headers)
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated {
		t.Fatalf("idem statuses = %d/%d", a.Code, b.Code)
	}
	if a.Body.String() != b.Body.String() {
		t.Fatalf("idem replay is not byte-identical:\n%s\n%s", a.Body.String(), b.Body.String())
	}
	rows, err := h.store.List(context.Background(), resources.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 { // api-w5 + api-w6
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}
