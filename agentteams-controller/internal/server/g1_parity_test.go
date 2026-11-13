//go:build service_e2e

package server

// G1 dual-mode parity (35 号 R1 门): the same five routes must answer the
// same bytes in
//
//	closed mode  = controller public API alone
//	enabled mode = service (3 native + 2 transparent proxy) in front of the
//	               same controller
//
// The test runs two independent controllers (separate fake Kubernetes
// clients, so the same worker name can exist in both) and one real service
// binary. The only accepted differences are documented in test/g1/README.md
// (team/role and RFC3339 time rendering do not occur in these fixtures).
//
// Run with the same env as TestServiceControllerE2E.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func e2eDo(t *testing.T, method, url, body, token string) (int, string, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(payload), resp.Header.Clone()
}

// cleanServiceRows deletes every row the service still lists, so the parity
// comparison sees exactly the fixture worker. Unknown names are proxied to
// the (empty) controller and come back 404, which soft-deletes the service
// row by design.
func cleanServiceRows(t *testing.T, base string) {
	t.Helper()
	code, body, _ := e2eDo(t, http.MethodGet, base+"/api/v1/workers", "", "public-token")
	if code != http.StatusOK {
		t.Fatalf("cleanup list = %d %s", code, body)
	}
	var list struct {
		Workers []struct {
			Name string `json:"name"`
		} `json:"workers"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("cleanup decode: %v", err)
	}
	for _, w := range list.Workers {
		code, _, _ := e2eDo(t, http.MethodDelete, base+"/api/v1/workers/"+w.Name, "", "public-token")
		if code != http.StatusNoContent && code != http.StatusNotFound {
			t.Fatalf("cleanup delete %s = %d", w.Name, code)
		}
	}
}

func TestG1DualModeParity(t *testing.T) {
	bin := os.Getenv("AGENTTEAMS_TEST_SERVICE_BIN")
	resDSN, opsDSN := os.Getenv("AGENTTEAMS_TEST_RES_DSN"), os.Getenv("AGENTTEAMS_TEST_OPS_DSN")
	if bin == "" || resDSN == "" || opsDSN == "" {
		t.Skip("service_e2e: set AGENTTEAMS_TEST_SERVICE_BIN / AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN")
	}

	newController := func() (*HTTPServer, client.Client) {
		scheme := newProjectTestScheme(t)
		k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1beta1.Worker{}).Build()
		mw := authpkg.NewMiddleware(&internalStaticAuth{
			token: "public-token",
			id: &authpkg.CallerIdentity{
				Role: authpkg.RoleAdmin, Username: "admin", MatrixUserID: "@admin:matrix.local",
				Teams: []string{"team-a"},
			},
		}, nil, authpkg.NewAuthorizer(), k8s, "default")
		srv := NewHTTPServer(":0", ServerDeps{
			Client: k8s, Namespace: "default", KubeMode: "embedded",
			AuthMw: mw, InternalToken: "internal-token", ControllerName: "ctl-g1",
			DefaultWorkerRuntime: "qwenpaw", DefaultModel: "m-default",
		})
		return srv, k8s
	}

	closed, _ := newController()
	enabled, enabledK8s := newController()
	closedSrv := httptest.NewServer(closed.Mux)
	defer closedSrv.Close()
	enabledSrv := httptest.NewServer(enabled.Mux)
	defer enabledSrv.Close()

	port := e2eFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	base := "http://" + addr
	var logs strings.Builder
	svc := exec.Command(bin)
	svc.Env = append(os.Environ(),
		"AGENTTEAMS_SERVICE_ENABLED=true",
		"AGENTTEAMS_SERVICE_LISTEN_ADDR="+addr,
		"AGENTTEAMS_SERVICE_RES_DSN="+resDSN,
		"AGENTTEAMS_SERVICE_OPS_DSN="+opsDSN,
		"AGENTTEAMS_SERVICE_CONTROLLER_URL="+enabledSrv.URL,
		"AGENTTEAMS_SERVICE_READ_THROUGH=false",
		"AGENTTEAMS_SERVICE_INTERNAL_TOKEN=internal-token",
		"AGENTTEAMS_SERVICE_LOG_LEVEL=error",
		"AGENTTEAMS_SERVICE_DEFAULT_WORKER_RUNTIME=qwenpaw",
		"AGENTTEAMS_SERVICE_DEFAULT_MODEL=m-default",
		"AGENTTEAMS_SERVICE_OUTBOX_INTERVAL=100ms",
		"AGENTTEAMS_SERVICE_SHUTDOWN_TIMEOUT=3s",
	)
	svc.Stdout, svc.Stderr = &logs, &logs
	if err := svc.Start(); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer func() {
		_ = svc.Process.Kill()
		_, _ = svc.Process.Wait()
	}()

	e2eWait(t, "service healthy", func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	cleanServiceRows(t, base)

	name := fmt.Sprintf("g1-%d", time.Now().UnixNano())
	// env + expose are the fields whose response shape used to diverge
	// (non-admin env, spec-derived exposedPorts). Include them so the parity
	// test covers the 58 §11 I14/I16 fixes.
	createBody := fmt.Sprintf(`{"name":%q,"workerName":"G1 Worker","model":"m1","runtime":"qwenpaw","skills":["s1"],"env":{"FOO":"bar"},"expose":[{"port":8080,"protocol":"http"}]}`, name)

	// 1) POST /api/v1/workers
	cs, cb, _ := e2eDo(t, http.MethodPost, closedSrv.URL+"/api/v1/workers", createBody, "public-token")
	es, eb, _ := e2eDo(t, http.MethodPost, base+"/api/v1/workers", createBody, "public-token")
	if cs != http.StatusCreated || es != http.StatusCreated {
		t.Fatalf("create status closed=%d enabled=%d (%s / %s)", cs, es, cb, eb)
	}
	if cb != eb {
		t.Fatalf("create body differs:\nclosed : %s\nenabled: %s", cb, eb)
	}
	e2eWait(t, "enabled controller provision", func() bool {
		return enabledK8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &v1beta1.Worker{}) == nil
	})

	// 2) GET by name
	cs, cb, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/workers/"+name, "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodGet, base+"/api/v1/workers/"+name, "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("get differs: status %d/%d\nclosed : %s\nenabled: %s", cs, es, cb, eb)
	}

	// 3) GET list (all + fields=spec with ETag)
	cs, cb, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/workers", "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodGet, base+"/api/v1/workers", "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("list differs: status %d/%d\nclosed : %s\nenabled: %s", cs, es, cb, eb)
	}
	cs, cb, ch := e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/workers?fields=spec", "", "public-token")
	es, eb, eh := e2eDo(t, http.MethodGet, base+"/api/v1/workers?fields=spec", "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("spec list differs: status %d/%d\nclosed : %s\nenabled: %s", cs, es, cb, eb)
	}
	if ch.Get("ETag") != eh.Get("ETag") {
		t.Fatalf("spec ETag differs: closed=%q enabled=%q", ch.Get("ETag"), eh.Get("ETag"))
	}
	if ch.Get("Cache-Control") != eh.Get("Cache-Control") {
		t.Fatalf("spec Cache-Control differs: closed=%q enabled=%q", ch.Get("Cache-Control"), eh.Get("Cache-Control"))
	}
	if ch.Get("Vary") != eh.Get("Vary") {
		t.Fatalf("spec Vary differs: closed=%q enabled=%q", ch.Get("Vary"), eh.Get("Vary"))
	}

	// 4) PUT (transparent proxy vs direct)
	// Also change env: the service must adopt the echoed env, and the
	// generation-only sync must not roll the controller CR back to the old
	// value (the audit found the old update event did exactly that).
	putBody := `{"soul":"g1 soul","env":{"FOO":"changed"}}`
	cs, cb, _ = e2eDo(t, http.MethodPut, closedSrv.URL+"/api/v1/workers/"+name, putBody, "public-token")
	es, eb, _ = e2eDo(t, http.MethodPut, base+"/api/v1/workers/"+name, putBody, "public-token")
	if cs != es || cb != eb {
		t.Fatalf("put differs: status %d/%d\nclosed : %s\nenabled: %s", cs, es, cb, eb)
	}
	// The enabled controller CR must keep the new env after the sync event is
	// delivered (no full-spec rollback).
	e2eWait(t, "sync event does not roll back the controller spec", func() bool {
		var w v1beta1.Worker
		if err := enabledK8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &w); err != nil {
			return false
		}
		return w.Spec.Env["FOO"] == "changed"
	})

	// 5) DELETE
	cs, cb, _ = e2eDo(t, http.MethodDelete, closedSrv.URL+"/api/v1/workers/"+name, "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodDelete, base+"/api/v1/workers/"+name, "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("delete differs: status %d/%d body %q/%q", cs, es, cb, eb)
	}
	e2eWait(t, "enabled controller deprovision", func() bool {
		err := enabledK8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &v1beta1.Worker{})
		return err != nil
	})

	if err := svc.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("service exit: %v\n%s", err, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("service did not exit\n%s", logs.String())
	}
}
