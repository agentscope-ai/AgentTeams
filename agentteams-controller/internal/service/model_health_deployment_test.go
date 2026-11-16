package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/modelhealth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// This opt-in test runs only on the disposable internal Docker network described
// in docs/model-preflight.md. All endpoints/accounts/keys below are test-only.
func TestModelHealthRealHigressTuwunel(t *testing.T) {
	if os.Getenv("AGENTTEAMS_PREFLIGHT_DEPLOYMENT_TEST") != "1" {
		t.Skip("requires isolated Higress/Tuwunel deployment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var mode atomic.Int32
	var upstreamCalls atomic.Int32
	var authenticatedUpstream atomic.Bool
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer dummy-upstream-key" {
			authenticatedUpstream.Store(false)
		}
		if r.Header.Get("Authorization") == "Bearer dummy-upstream-key" {
			authenticatedUpstream.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 0:
			w.Write([]byte(`{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
		case 1:
			w.Write([]byte(`{"choices":[]}`))
		default:
			w.Write([]byte(`{"error":{"message":"quota dummy-private-body"}}`))
		}
	}))
	listener, err := net.Listen("tcp", ":18877")
	if err != nil {
		t.Fatal(err)
	}
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()
	httpClient := &http.Client{Timeout: 15 * time.Second}
	gw := gateway.NewHigressClient(gateway.Config{ConsoleURL: "http://preflight-infra:8001", AdminUser: "admin", AdminPassword: "admin", DataPlaneURL: "http://preflight-infra:8080"}, httpClient)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(gw.Healthy(ctx))
	must(gw.EnsureServiceSource(ctx, "preflight-upstream", "preflight-probe.test", 18877, "http"))
	// Higress service discovery is eventually consistent.
	time.Sleep(2 * time.Second)
	must(gw.EnsureAIProvider(ctx, gateway.AIProviderRequest{Name: "preflight-provider", Type: "openai", Tokens: []string{"dummy-upstream-key"}, Protocol: "openai/v1", Raw: map[string]interface{}{"agentteamsMode": true, "openaiCustomUrl": "http://preflight-probe.test/v1", "openaiCustomServiceName": "preflight-upstream.dns", "openaiCustomServicePort": 18877}}))
	must(gw.EnsureAIRoute(ctx, gateway.AIRouteRequest{Name: "preflight-route", PathPrefix: "/v1", Provider: "preflight-provider"}))
	_, err = gw.EnsureConsumer(ctx, gateway.ConsumerRequest{Name: "preflight-manager", CredentialKey: "dummy-gateway-key"})
	must(err)
	must(gw.AuthorizeAIRoutes(ctx, "preflight-manager", ""))
	cfg := modelhealth.Config{URL: "http://preflight-infra:8080/v1", Key: "dummy-gateway-key", Model: "fixture-model", Protocol: "chat"}
	deadline := time.Now().Add(100 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		last = modelhealth.Probe(ctx, httpClient, cfg)
		if last == "ok" && authenticatedUpstream.Load() {
			denied := cfg
			denied.Key = "invalid-dummy-key"
			if modelhealth.Probe(ctx, httpClient, denied) == "authentication" {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if last != "ok" || !authenticatedUpstream.Load() {
		t.Fatalf("gateway did not converge: %s upstream_calls=%d", last, upstreamCalls.Load())
	}
	t.Logf("real Higress success: upstream_calls=%d", upstreamCalls.Load())
	cfg.Key = "invalid-dummy-key"
	if got := modelhealth.Probe(ctx, httpClient, cfg); got != "authentication" {
		t.Fatalf("Higress key-auth result %s", got)
	}
	cfg.Key = "dummy-gateway-key"
	mx := matrix.NewTuwunelClient(matrix.Config{ServerURL: "http://preflight-infra:6167", Domain: "preflight.test", RegistrationToken: "preflight-dummy-registration"}, httpClient)
	admin, err := mx.EnsureUser(ctx, matrix.EnsureUserRequest{Username: "preflight-admin", Password: "dummy-admin-password-123"})
	must(err)
	manager, err := mx.EnsureUser(ctx, matrix.EnsureUserRequest{Username: "preflight-manager", Password: "dummy-manager-password-123"})
	must(err)
	room, err := mx.CreateRoom(ctx, matrix.CreateRoomRequest{Name: "Preflight isolated test DM", CreatorToken: admin.AccessToken, Invite: []string{manager.UserID}, IsDirect: true})
	must(err)
	must(mx.JoinRoom(ctx, room.RoomID, manager.AccessToken))
	p := &Provisioner{matrix: mx, adminUser: "preflight-admin", matrixDomain: "preflight.test"}
	store := ossfake.NewMemory()
	d := NewDeployer(DeployerConfig{OSS: store})
	must(store.PutObject(ctx, "agents/manager/runtime/runtime.yaml", []byte(`{"desired":{"model":{"model":"fixture-model","gatewayUrl":"http://preflight-infra:8080/v1"}}}`)))
	sends := 0
	loseAck := true
	send := func(ctx context.Context, room, txn, body string) error {
		sends++
		if err := p.SendModelHealthNotice(ctx, room, manager.AccessToken, txn, body); err != nil {
			return err
		}
		if loseAck {
			loseAck = false
			return errors.New("simulated lost acknowledgement")
		}
		return nil
	}
	run := func() {
		t.Helper()
		must(d.CheckManagerModel(ctx, "qwenpaw", "dummy-gateway-key", admin.UserID, room.RoomID, send))
	}
	state := func() modelhealth.State {
		t.Helper()
		b, err := store.GetObject(ctx, "agents/manager/model-health.json")
		must(err)
		if strings.Contains(string(b), "dummy-") {
			t.Fatal("state leaked credentials/body")
		}
		var s modelhealth.State
		must(json.Unmarshal(b, &s))
		return s
	}
	expire := func() {
		s := state()
		s.CheckedAt = time.Time{}
		b, _ := json.Marshal(s)
		must(store.PutObject(ctx, "agents/manager/model-health.json", b))
	}
	countNotices := func() int {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://preflight-infra:6167/_matrix/client/v3/rooms/"+url.PathEscape(room.RoomID)+"/messages?dir=b&limit=100", nil)
		req.Header.Set("Authorization", "Bearer "+admin.AccessToken)
		resp, err := httpClient.Do(req)
		must(err)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("Matrix timeline HTTP %d", resp.StatusCode)
		}
		var timeline struct {
			Chunk []struct {
				Type    string `json:"type"`
				Content struct {
					MsgType string `json:"msgtype"`
					Body    string `json:"body"`
				} `json:"content"`
			} `json:"chunk"`
		}
		must(json.NewDecoder(resp.Body).Decode(&timeline))
		n := 0
		for _, e := range timeline.Chunk {
			if e.Type == "m.room.message" && e.Content.MsgType == "m.notice" {
				n++
				if strings.Contains(e.Content.Body, "dummy-") {
					t.Fatal("notice leaked body or credential")
				}
			}
		}
		return n
	}
	run()
	if state().Status != "ok" || countNotices() != 0 {
		t.Fatal("success generated failure notice")
	}
	mode.Store(1)
	expire()
	run()
	firstTxn := state().Transaction
	if state().Reason != "empty_response" || state().Notified || countNotices() != 1 {
		t.Fatalf("invalid response state %+v", state())
	}
	// A new Deployer represents restart: durable state and stable transaction survive.
	d = NewDeployer(DeployerConfig{OSS: store})
	run()
	run()
	if !state().Notified || state().Transaction != firstTxn || sends != 2 || countNotices() != 1 {
		t.Fatal("real Matrix duplicated retried notice")
	}
	mode.Store(0)
	expire()
	run()
	if state().RecoveredAt == nil || state().Status != "ok" || countNotices() != 1 {
		t.Fatal("recovery missing")
	}
	mode.Store(2)
	expire()
	run()
	if state().Reason != "billing_or_quota" || countNotices() != 2 || state().Transaction == firstTxn {
		t.Fatalf("new failure state %+v", state())
	}
	t.Logf("real Tuwunel timeline: 2 notices, 3 delivery attempts, stable retry, recovered_at recorded; upstream_calls=%d", upstreamCalls.Load())
}
