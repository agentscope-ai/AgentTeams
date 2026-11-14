//go:build service_e2e

package server

// Cross-process acceptance for the R1 service slice: a REAL
// agentteams-service binary against the REAL controller HTTP surface backed
// by a fake Kubernetes client. No cluster is needed, but the full contract
// is exercised end to end:
//
//	service create → outbox dispatcher → controller provision → Worker CR
//	controller status pusher → service /internal/v1/workers/status → list
//	service delete → controller deprovision → CR gone
//
// Run:
//
//	AGENTTEAMS_TEST_SERVICE_BIN=/path/to/agentteams-service \
//	AGENTTEAMS_TEST_RES_DSN=... AGENTTEAMS_TEST_OPS_DSN=... \
//	go test -tags service_e2e -count=1 ./internal/server/ -run TestServiceControllerE2E
//
// Skipped without the three env vars, so normal CI is unaffected.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func e2eFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func e2eWait(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func e2eGET(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServiceControllerE2E(t *testing.T) {
	bin := os.Getenv("AGENTTEAMS_TEST_SERVICE_BIN")
	resDSN, opsDSN := os.Getenv("AGENTTEAMS_TEST_RES_DSN"), os.Getenv("AGENTTEAMS_TEST_OPS_DSN")
	if bin == "" || resDSN == "" || opsDSN == "" {
		t.Skip("service_e2e: set AGENTTEAMS_TEST_SERVICE_BIN / AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN")
	}

	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1beta1.Worker{}).Build()
	mw := authpkg.NewMiddleware(&internalStaticAuth{
		token: "public-token",
		id: &authpkg.CallerIdentity{
			Role: authpkg.RoleAdmin, Username: "admin", MatrixUserID: "@admin:matrix.local",
			Teams: []string{"team-a"},
		},
	}, nil, authpkg.NewAuthorizer(), k8s, "default")
	ctrl := NewHTTPServer(":0", ServerDeps{
		Client: k8s, Namespace: "default", KubeMode: "embedded",
		AuthMw: mw, InternalToken: "internal-token", ControllerName: "ctl-e2e",
		DefaultWorkerRuntime: "qwenpaw", DefaultModel: "m-default",
	})
	ctrlSrv := httptest.NewServer(ctrl.Mux)
	defer ctrlSrv.Close()

	port := e2eFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var logs strings.Builder
	svc := exec.Command(bin)
	svc.Env = append(os.Environ(),
		"AGENTTEAMS_SERVICE_ENABLED=true",
		"AGENTTEAMS_SERVICE_LISTEN_ADDR="+addr,
		"AGENTTEAMS_SERVICE_RES_DSN="+resDSN,
		"AGENTTEAMS_SERVICE_OPS_DSN="+opsDSN,
		"AGENTTEAMS_SERVICE_CONTROLLER_URL="+ctrlSrv.URL,
		"AGENTTEAMS_SERVICE_READ_THROUGH=false",
		"AGENTTEAMS_SERVICE_INTERNAL_TOKEN=internal-token",
		"AGENTTEAMS_SERVICE_LOG_LEVEL=error",
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

	base := "http://" + addr
	e2eWait(t, "service healthy", func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	name := fmt.Sprintf("cross-%d", time.Now().UnixNano())
	body := `{"name":"` + name + `","model":"m1","runtime":"qwenpaw"}`
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/workers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer public-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	createBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, createBody)
	}

	// Dispatcher → controller provision → Worker CR exists.
	var worker v1beta1.Worker
	e2eWait(t, "controller provisioned the Worker CR", func() bool {
		err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &worker)
		return err == nil
	})
	if worker.Spec.Model != "m1" || worker.Spec.Runtime != "qwenpaw" {
		t.Fatalf("provisioned spec = %+v", worker.Spec)
	}
	if worker.Annotations[provisionGenerationAnnotation] != "1" {
		t.Fatalf("provision generation annotation = %q", worker.Annotations[provisionGenerationAnnotation])
	}

	// Simulate the reconciler reporting Running.
	worker.Status.Phase = "Running"
	worker.Status.ContainerState = "running"
	worker.Status.MatrixUserID = "@" + name + ":matrix.local"
	worker.Status.RoomID = "!room:matrix.local"
	worker.Status.AgentStatus = "idle"
	if err := k8s.Status().Update(context.Background(), &worker); err != nil {
		t.Fatalf("update worker status: %v", err)
	}

	// Controller status pusher → service → list shows Running.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pusher := NewStatusPusher(k8s, "default", base, "internal-token")
	pusher.Interval = 100 * time.Millisecond
	go func() { _ = pusher.Start(ctx) }()

	e2eWait(t, "service row reflects Running", func() bool {
		code, listBody := e2eGET(t, base+"/api/v1/workers/"+name, "public-token")
		return code == http.StatusOK && strings.Contains(listBody, `"phase":"Running"`)
	})

	// Service DELETE → controller deprovision → CR gone; service row 404.
	delReq, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/workers/"+name, nil)
	delReq.Header.Set("Authorization", "Bearer public-token")
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", delResp.StatusCode)
	}
	e2eWait(t, "controller deprovisioned the Worker CR", func() bool {
		err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &v1beta1.Worker{})
		return errors.IsNotFound(err)
	})
	e2eWait(t, "service row soft-deleted", func() bool {
		code, _ := e2eGET(t, base+"/api/v1/workers/"+name, "public-token")
		return code == http.StatusNotFound
	})

	// Graceful shutdown.
	if err := svc.Process.Signal(syscall.SIGTERM); err != nil {
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
