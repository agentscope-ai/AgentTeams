package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/module"
)

func testLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	return lg, &buf
}

func okHealth(ctx context.Context) module.Health {
	return module.Health{Status: module.StatusOK}
}

func TestHealthz(t *testing.T) {
	lg, _ := testLogger(t)
	srv := New(lg)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v (%s)", err, rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestHealthzMethodNotAllowed(t *testing.T) {
	srv := New(nil)
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestReadyzAllOK(t *testing.T) {
	srv := New(nil)
	srv.AddReadyCheck("db", okHealth)
	srv.AddReadyCheck("modules", okHealth)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestReadyzDegraded(t *testing.T) {
	srv := New(nil)
	srv.AddReadyCheck("db", okHealth)
	srv.AddReadyCheck("modules", func(ctx context.Context) module.Health {
		return module.Health{Status: module.StatusDegraded, Detail: "m1(degraded)"}
	})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degraded-only is not down)", rec.Code)
	}
	var body readyOut
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v (%s)", err, rec.Body.String())
	}
	if body.Status != "degraded" {
		t.Fatalf("status = %q", body.Status)
	}
	if body.Checks["modules"].Status != "degraded" || body.Checks["modules"].Detail != "m1(degraded)" {
		t.Fatalf("checks = %v", body.Checks)
	}
	if body.Checks["db"].Status != "ok" {
		t.Fatalf("checks = %v", body.Checks)
	}
}

func TestReadyzDown(t *testing.T) {
	srv := New(nil)
	srv.AddReadyCheck("db", func(ctx context.Context) module.Health {
		return module.Health{Status: module.StatusDown, Detail: "res: ping failed"}
	})
	srv.AddReadyCheck("modules", okHealth)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body readyOut
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v (%s)", err, rec.Body.String())
	}
	if body.Status != "degraded" {
		t.Fatalf("status = %q, want degraded (readyz db check down)", body.Status)
	}
	if body.Checks["db"].Status != "down" || body.Checks["db"].Detail != "res: ping failed" {
		t.Fatalf("checks = %v", body.Checks)
	}
}

func TestReadyzNoChecks(t *testing.T) {
	srv := New(nil)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestPanicRecovered(t *testing.T) {
	lg, _ := testLogger(t)
	srv := New(lg)
	srv.mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestRequestIDPassthrough(t *testing.T) {
	srv := New(nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "client-set-id")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got != "client-set-id" {
		t.Fatalf("X-Request-Id = %q, want passthrough", got)
	}
}

func TestRequestIDGenerated(t *testing.T) {
	srv := New(nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got == "" {
		t.Fatal("X-Request-Id not generated")
	}
}

func TestAccessLog(t *testing.T) {
	lg, buf := testLogger(t)
	srv := New(lg)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "access-log-req")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	out := buf.String()
	for _, want := range []string{
		`method=GET`,
		`path=/healthz`,
		`status=200`,
		`request_id=access-log-req`,
		`duration_ms=`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("access log missing %q: %s", want, out)
		}
	}
}

func TestAccessLogCapturesHandlerStatus(t *testing.T) {
	lg, buf := testLogger(t)
	srv := New(lg)
	srv.mux.HandleFunc("/teapot", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	req := httptest.NewRequest(http.MethodGet, "/teapot", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if !strings.Contains(buf.String(), `status=418`) {
		t.Fatalf("access log missing 418: %s", buf.String())
	}
}

func TestRequestBodyReadableByHandler(t *testing.T) {
	srv := New(nil)
	srv.mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("hello"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
