package modelhealth

import (
	"context"
	"encoding/json"
	"errors"
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
func TestReportContractRejectsUnknownStaleAndForeignEvidence(t *testing.T) {
	now := time.Now().UTC()
	good := Report{Version: 1, Runtime: "example", Member: "alice", Configuration: strings.Repeat("a", 64), Status: "ok", Reason: "ok", CheckedAt: now}
	encode := func(r Report) []byte { b, _ := json.Marshal(r); return b }
	if Parse(encode(good), "example", "alice", now).Status != "ok" {
		t.Fatal("valid report rejected")
	}
	for name, change := range map[string]func(*Report){"version": func(r *Report) { r.Version = 2 }, "foreign-runtime": func(r *Report) { r.Runtime = "other" }, "foreign-member": func(r *Report) { r.Member = "bob" }, "future": func(r *Report) { r.CheckedAt = now.Add(time.Hour) }, "stale": func(r *Report) { r.CheckedAt = now.Add(-MaxAge - time.Second) }, "secret-reason": func(r *Report) { r.Status = "failed"; r.Reason = "api-key-secret" }, "secret-configuration": func(r *Report) { r.Configuration = "token-secret" }, "false-ok": func(r *Report) { r.Reason = "network" }} {
		t.Run(name, func(t *testing.T) {
			r := good
			change(&r)
			got := Parse(encode(r), "example", "alice", now)
			if got.Status != "unknown" || strings.Contains(string(encode(got)), "secret") {
				t.Fatal(got)
			}
		})
	}
}
func TestFailureNoticeRecoveryAndUnknownAreIndependentOfModelProtocol(t *testing.T) {
	ctx := context.Background()
	m := &memory{}
	cfg := Config{Runtime: "any-harness", Admin: "admin", Room: "room"}
	r := Report{Version: 1, Configuration: strings.Repeat("a", 64), Status: "failed", Reason: "authentication", CheckedAt: time.Now().UTC()}
	fail := true
	txns := []string{}
	send := func(_ context.Context, _ string, txn, body string) error {
		txns = append(txns, txn)
		if m.state().Transaction != txn {
			t.Fatal("notice preceded durable identity")
		}
		if strings.Contains(body, "secret") {
			t.Fatal("secret escaped")
		}
		if fail {
			return errors.New("lost acknowledgement")
		}
		return nil
	}
	run := func() {
		t.Helper()
		if err := Check(ctx, m, "health", cfg, r, send); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if m.state().Notified {
		t.Fatal("unacknowledged notice marked delivered")
	}
	fail = false
	run()
	run()
	if len(txns) != 2 || txns[0] != txns[1] || !m.state().Notified {
		t.Fatal("notice retry was not idempotent")
	}
	r.Status, r.Reason = "ok", "ok"
	run()
	if m.state().RecoveredAt == nil {
		t.Fatal("missing recovery")
	}
	r = Unknown("stale_report")
	run()
	if m.state().Status != "unknown" || len(txns) != 2 {
		t.Fatal("unknown became healthy or sent model failure notice")
	}
	r.Status, r.Reason = "failed", "network"
	run()
	if len(txns) != 3 || txns[2] == txns[0] {
		t.Fatal("new failure episode reused notice")
	}
}
func TestUnreadableHealthStateNeverSends(t *testing.T) {
	for _, m := range []*memory{{data: []byte("broken")}, {fail: true}} {
		called := false
		err := Check(context.Background(), m, "health", Config{}, Report{Status: "failed", Reason: "network"}, func(context.Context, string, string, string) error { called = true; return nil })
		if err == nil || called {
			t.Fatal("unsafe state sent a notice")
		}
	}
}
