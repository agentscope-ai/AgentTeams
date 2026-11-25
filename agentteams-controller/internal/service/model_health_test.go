package service

import (
	"context"
	"encoding/json"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/modelhealth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagerHealthConsumesSameReportForEveryHarness(t *testing.T) {
	for _, runtime := range []string{"openclaw", "qwenpaw", "hermes", "deepseek-harness", "openhuman", "codex", "opencode", "qoder", "claudecode"} {
		t.Run(runtime, func(t *testing.T) {
			ctx := context.Background()
			store := ossfake.NewMemory()
			d := NewDeployer(DeployerConfig{OSS: store})
			data, _ := json.Marshal(modelhealth.Report{Version: 1, Runtime: runtime, Member: "manager", Status: "ok", Reason: "ok", CheckedAt: time.Now().UTC()})
			store.PutObject(ctx, "agents/manager/runtime/health.json", data)
			if err := d.CheckManagerHealth(ctx, runtime, "admin", "room", nil); err != nil {
				t.Fatal(err)
			}
			saved, _ := store.GetObject(ctx, "agents/manager/model-health.json")
			if !strings.Contains(string(saved), `"status":"ok"`) {
				t.Fatal(string(saved))
			}
			store.DeleteObject(ctx, "agents/manager/runtime/health.json")
			if err := d.CheckManagerHealth(ctx, runtime, "admin", "room", nil); err != nil {
				t.Fatal(err)
			}
			saved, _ = store.GetObject(ctx, "agents/manager/model-health.json")
			if !strings.Contains(string(saved), `"status":"unknown"`) {
				t.Fatal("old success survived missing Harness report")
			}
		})
	}
}

// TestModelHealthGatewayMatrixHTTP exercises both production HTTP clients. The
// fixture accepts the first notice but loses acknowledgement; restart retries
// exactly the same Matrix transaction, then suppresses further delivery.
func TestModelHealthGatewayMatrixHTTP(t *testing.T) {
	ctx := context.Background()
	txns := []string{}
	wrongAdmin := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/chat/completions":
			if r.Header.Get("Authorization") != "Bearer gateway-dummy" {
				t.Error("wrong gateway auth")
			}
			w.Write([]byte(`{"error":{"message":"insufficient quota raw-secret"}}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			admin := "@admin:test"
			if wrongAdmin {
				admin = "@stranger:test"
			}
			w.Write([]byte(`{"chunk":[{"state_key":"` + admin + `","content":{"membership":"join"}},{"state_key":"@manager:test","content":{"membership":"join"}}]}`))
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			if r.Header.Get("Authorization") != "Bearer matrix-dummy" || !strings.Contains(r.URL.Path, "!admin:test") {
				t.Error("wrong DM or Matrix auth")
			}
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["msgtype"] != "m.notice" || strings.Contains(body["body"], "secret") {
				t.Error("unsafe notice")
			}
			txns = append(txns, r.URL.Path)
			if len(txns) == 1 {
				w.WriteHeader(503)
				w.Write([]byte(`raw-secret`))
				return
			}
			w.Write([]byte(`{"event_id":"$accepted"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	store := ossfake.NewMemory()
	d := NewDeployer(DeployerConfig{OSS: store})
	store.PutObject(ctx, "agents/manager/runtime/runtime.yaml", []byte(`{"desired":{"model":{"model":"configured","gatewayUrl":"`+srv.URL+`/v1"}}}`))
	p := &Provisioner{matrix: matrix.NewTuwunelClient(matrix.Config{ServerURL: srv.URL}, srv.Client()), adminUser: "admin", matrixDomain: "test"}
	report, _ := json.Marshal(modelhealth.Report{Version: 1, Runtime: "qwenpaw", Member: "manager", Status: "failed", Reason: "billing_or_quota", CheckedAt: time.Now().UTC()})
	store.PutObject(ctx, "agents/manager/runtime/health.json", report)
	send := func(ctx context.Context, room, txn, body string) error {
		return p.SendModelHealthNotice(ctx, room, "matrix-dummy", txn, body)
	}
	for i := 0; i < 3; i++ {
		if err := d.CheckManagerHealth(ctx, "qwenpaw", p.ModelHealthAdmin(), "!admin:test", send); err != nil {
			t.Fatal(err)
		}
	}
	if len(txns) != 2 || txns[0] != txns[1] {
		t.Fatalf("unsafe retry %v", txns)
	}
	data, _ := store.GetObject(ctx, "agents/manager/model-health.json")
	if strings.Contains(string(data), "secret") || !strings.Contains(string(data), `"notified":true`) {
		t.Fatalf("unsafe state %s", data)
	}
	wrongAdmin = true
	if err := p.SendModelHealthNotice(ctx, "!admin:test", "matrix-dummy", "another-txn", "body"); err == nil || len(txns) != 2 {
		t.Fatal("sent to room without authorized admin")
	}
}
