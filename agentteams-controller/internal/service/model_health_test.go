package service

import (
	"context"
	"encoding/json"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagerHealthEffectiveContract(t *testing.T) {
	for _, runtime := range []string{"qwenpaw", "codex", "openclaw"} {
		t.Run(runtime, func(t *testing.T) {
			path := "/v1/chat/completions"
			body := `{"choices":[{"message":{"content":"ok"}}]}`
			if runtime == "codex" {
				path = "/v1/responses"
				body = `{"status":"completed","output":[{"content":[{"type":"output_text","text":"ok"}]}]}`
			}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != path || r.Header.Get("Authorization") != "Bearer effective-key" {
					t.Errorf("wrong effective route %s", r.URL.Path)
				}
				w.Write([]byte(body))
			}))
			defer srv.Close()
			store := ossfake.NewMemory()
			d := NewDeployer(DeployerConfig{OSS: store})
			ctx := context.Background()
			config := `{"desired":{"model":{"model":"effective-model","gatewayUrl":"` + srv.URL + `"}}}`
			key := "agents/manager/runtime/runtime.yaml"
			if runtime == "openclaw" {
				key = "agents/manager/openclaw.json"
				config = `{"agents":{"defaults":{"model":{"primary":"custom/effective-model"}}},"models":{"providers":{"custom":{"baseUrl":"` + srv.URL + `/v1","apiKey":"effective-key","api":"openai-completions"}}}}`
			}
			store.PutObject(ctx, key, []byte(config))
			if err := d.CheckManagerModel(ctx, runtime, "effective-key", "admin", "room", nil); err != nil {
				t.Fatal(err)
			}
			b, _ := store.GetObject(ctx, "agents/manager/model-health.json")
			if !strings.Contains(string(b), `"status":"ok"`) || calls != 1 {
				t.Fatalf("health %s calls %d", b, calls)
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
	send := func(ctx context.Context, room, txn, body string) error {
		return p.SendModelHealthNotice(ctx, room, "matrix-dummy", txn, body)
	}
	for i := 0; i < 3; i++ {
		if err := d.CheckManagerModel(ctx, "qwenpaw", "gateway-dummy", p.ModelHealthAdmin(), "!admin:test", send); err != nil {
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
