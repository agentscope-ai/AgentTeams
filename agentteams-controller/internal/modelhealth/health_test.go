package modelhealth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type memory struct {
	data []byte
	fail bool
}

func (m *memory) GetObject(context.Context, string) ([]byte, error) {
	if m.data == nil {
		return nil, os.ErrNotExist
	}
	return m.data, nil
}
func (m *memory) PutObject(_ context.Context, _ string, b []byte) error {
	if m.fail {
		return errors.New("storage secret")
	}
	m.data = append([]byte{}, b...)
	return nil
}
func (m *memory) state() State { var s State; json.Unmarshal(m.data, &s); return s }
func TestFailureLifecycle(t *testing.T) {
	ctx := context.Background()
	body := `{"error":{"message":"quota key-secret unrelated-secret"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer srv.Close()
	cfg := Config{URL: srv.URL, Key: "key-secret", Model: "model-secret", Protocol: "chat", Admin: "@admin:test", Room: "!dm:test"}
	m := &memory{}
	txns := []string{}
	fail := true
	send := func(_ context.Context, room, txn, body string) error {
		txns = append(txns, txn)
		if strings.Contains(body, "secret") {
			t.Fatal("notice leaked secret")
		}
		if m.state().Transaction != txn {
			t.Fatal("send preceded durable state")
		}
		if fail {
			return errors.New("matrix-secret")
		}
		return nil
	}
	run := func() {
		t.Helper()
		if err := Check(ctx, m, "health", cfg, nil, send); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(m.data), "secret") {
			t.Fatal("state leaked secret")
		}
	}
	run()
	if m.state().Notified || m.state().NoticeStatus != "delivery_failed" {
		t.Fatal(m.state())
	}
	fail = false
	run()
	if txns[0] != txns[1] || !m.state().Notified {
		t.Fatal("retry changed transaction")
	}
	run()
	if len(txns) != 2 {
		t.Fatal("restart duplicated notice")
	}
	// Each material identity change must bypass previous deduplication.
	for _, change := range []func(){func() { cfg.Model = "other" }, func() { cfg.Key = "new-secret" }, func() { cfg.Admin = "@new:test" }, func() { cfg.Room = "!other:test" }, func() { cfg.URL = srv.URL + "/v1" }} {
		change()
		before := len(txns)
		run()
		if len(txns) != before+1 {
			t.Fatal("configuration change swallowed")
		}
	}
	s0 := m.state()
	s0.CheckedAt = time.Time{}
	m.data, _ = json.Marshal(s0)
	body = `{"choices":[{"message":{"content":"ok"}}]}`
	run()
	if m.state().Status != "ok" || m.state().RecoveredAt == nil {
		t.Fatal("recovery missing")
	}
	// Expire success cache to simulate the next startup/check interval.
	s := m.state()
	s.CheckedAt = time.Time{}
	m.data, _ = json.Marshal(s)
	body = `{"choices":[]}`
	run()
	if m.state().Reason != "empty_response" || txns[len(txns)-1] == txns[len(txns)-2] {
		t.Fatal("new episode reused old transaction")
	}
}
func TestUnsafeStateNeverSends(t *testing.T) {
	for _, m := range []*memory{{data: []byte("invalid")}, {fail: true}} {
		called := false
		err := Check(context.Background(), m, "health", Config{}, nil, func(context.Context, string, string, string) error { called = true; return nil })
		if err == nil || called || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe state sent or leaked")
		}
	}
}
func TestProbeProtocolsAndErrors(t *testing.T) {
	for _, tc := range []struct {
		protocol, path, body, want string
		code                       int
	}{
		{"chat", "/v1/chat/completions", `{"choices":[{"message":{"content":"ok"}}]}`, "ok", 200},
		{"responses", "/v1/responses", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`, "ok", 200},
		{"anthropic-messages", "/v1/messages", `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`, "ok", 200},
		{"anthropic-messages", "/v1/messages", `{"type":"message","role":"assistant","content":[]}`, "empty_response", 200},
		{"anthropic-messages", "/v1/messages", `{"choices":[{"message":{"content":"ok"}}]}`, "invalid_response", 200},
		{"chat", "/v1/chat/completions", `{"base_resp":{"status_code":1008,"status_msg":"secret"}}`, "billing_or_quota", 200},
		{"chat", "/v1/chat/completions", `{"error":{"message":"unauthorized secret"}}`, "authentication", 200},
		{"chat", "/v1/chat/completions", `{}`, "empty_response", 200},
		{"chat", "/v1/chat/completions", `{"choices":[{"message":{"reasoning_content":"thinking"}}]}`, "empty_response", 200},
		{"chat", "/v1/chat/completions", `html secret`, "invalid_response", 200},
		{"chat", "/v1/chat/completions", `secret`, "authentication", 401},
		{"chat", "/v1/chat/completions", `secret`, "billing_or_quota", 402},
		{"chat", "/v1/chat/completions", `secret`, "model_or_endpoint", 404},
		{"chat", "/v1/chat/completions", `secret`, "rate_limit_or_quota", 429},
		{"chat", "/v1/chat/completions", `secret`, "upstream_unavailable", 503},
	} {
		t.Run(tc.want+tc.protocol, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Header.Get("Authorization") != "Bearer dummy" {
					t.Error("wrong gateway request")
				}
				var p map[string]any
				json.NewDecoder(r.Body).Decode(&p)
				if tc.protocol == "responses" && (p["input"] == nil || p["messages"] != nil) {
					t.Error("wrong Responses request")
				}
				if tc.protocol == "anthropic-messages" && (r.Header.Get("anthropic-version") != "2023-06-01" || p["messages"] == nil || p["model"] != "selected") {
					t.Error("wrong Anthropic Messages request")
				}
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got := Probe(context.Background(), nil, Config{URL: srv.URL + "/v1", Key: "dummy", Model: "selected", Protocol: tc.protocol})
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
func TestTransportAndRedirect(t *testing.T) {
	if transportReason(&net.DNSError{Err: "secret"}) != "dns" || transportReason(context.DeadlineExceeded) != "timeout" {
		t.Fatal("wrong category")
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tls.Close()
	cfg := Config{URL: tls.URL, Key: "dummy", Model: "m", Protocol: "chat"}
	if got := Probe(context.Background(), nil, cfg); got != "tls" {
		t.Fatal(got)
	}
	received := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	cfg.URL = redirect.URL
	if got := Probe(context.Background(), nil, cfg); got != "configuration" || received {
		t.Fatal("followed credential redirect")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(30 * time.Millisecond) }))
	defer slow.Close()
	cfg.URL = slow.URL
	if got := Probe(ctx, nil, cfg); got != "timeout" {
		t.Fatal(got)
	}
}

func TestAcknowledgedNoticeStateWriteFailureRetriesSameTransaction(t *testing.T) {
	ctx := context.Background()
	m := &memory{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	cfg := Config{URL: srv.URL, Key: "dummy", Model: "m", Protocol: "chat", Admin: "admin", Room: "room"}
	var transactions []string
	send := func(_ context.Context, _, txn, _ string) error {
		transactions = append(transactions, txn)
		m.fail = true
		return nil
	}
	if err := Check(ctx, m, "health", cfg, nil, send); err == nil {
		t.Fatal("missed state failure")
	}
	if m.state().Notified || m.state().Transaction == "" {
		t.Fatal("lost pending transaction")
	}
	m.fail = false
	send = func(_ context.Context, _, txn, _ string) error { transactions = append(transactions, txn); return nil }
	if err := Check(ctx, m, "health", cfg, nil, send); err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 2 || transactions[0] != transactions[1] || !m.state().Notified {
		t.Fatal("duplicated acknowledged transaction")
	}
}
