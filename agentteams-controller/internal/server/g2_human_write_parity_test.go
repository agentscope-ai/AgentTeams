//go:build service_e2e

package server

// G2 human WRITE parity: the same POST/PUT/DELETE sent to the controller and
// through the service returns identical status/body, and the service's
// write-through keeps GET/list byte-identical afterwards. The create path is
// pure fake-client CR creation (no Matrix dependency), so it runs without a
// cluster.

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func TestG2HumanWriteParity(t *testing.T) {
	bin := os.Getenv("AGENTTEAMS_TEST_SERVICE_BIN")
	resDSN, opsDSN := os.Getenv("AGENTTEAMS_TEST_RES_DSN"), os.Getenv("AGENTTEAMS_TEST_OPS_DSN")
	if bin == "" || resDSN == "" || opsDSN == "" {
		t.Skip("service_e2e: set AGENTTEAMS_TEST_SERVICE_BIN / AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN")
	}

	newController := func() *HTTPServer {
		scheme := newProjectTestScheme(t)
		k8s := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&v1beta1.Human{}).Build()
		mw := authpkg.NewMiddleware(&internalStaticAuth{
			token: "public-token",
			id: &authpkg.CallerIdentity{
				Role: authpkg.RoleAdmin, Username: "admin", MatrixUserID: "@admin:matrix.local",
			},
		}, nil, authpkg.NewAuthorizer(), k8s, "default")
		return NewHTTPServer(":0", ServerDeps{
			Client: k8s, Namespace: "default", KubeMode: "embedded",
			AuthMw: mw, InternalToken: "internal-token", ControllerName: "ctl-g2hw",
			DefaultWorkerRuntime: "qwenpaw", DefaultModel: "m-default",
		})
	}
	closedSrv := httptest.NewServer(newController().Mux)
	defer closedSrv.Close()
	enabledSrv := httptest.NewServer(newController().Mux)
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

	db, err := sql.Open("mysql", resDSN)
	if err != nil {
		t.Fatalf("open res: %v", err)
	}
	defer db.Close()
	for _, q := range []string{"DELETE FROM human_deletions", "DELETE FROM humans"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("clean %s: %v", q, err)
		}
	}

	name := fmt.Sprintf("g2-human-w-%d", time.Now().UnixNano())
	createBody := fmt.Sprintf(`{"name":%q,"displayName":"G2 Human","permissionLevel":2,"accessibleTeams":["team-a"]}`, name)

	cs, cb, _ := e2eDo(t, http.MethodPost, closedSrv.URL+"/api/v1/humans", createBody, "public-token")
	es, eb, _ := e2eDo(t, http.MethodPost, base+"/api/v1/humans", createBody, "public-token")
	if cs != es || cb != eb {
		t.Fatalf("human create differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}

	cs, cb, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/humans/"+name, "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodGet, base+"/api/v1/humans/"+name, "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("human get after create differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}
	cs, cb, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/humans", "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodGet, base+"/api/v1/humans", "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("human list after create differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}

	putBody := `{"displayName":"G2 Human 2"}`
	cs, cb, _ = e2eDo(t, http.MethodPut, closedSrv.URL+"/api/v1/humans/"+name, putBody, "public-token")
	es, eb, _ = e2eDo(t, http.MethodPut, base+"/api/v1/humans/"+name, putBody, "public-token")
	if cs != es || cb != eb {
		t.Fatalf("human update differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}
	cs, cb, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/humans/"+name, "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodGet, base+"/api/v1/humans/"+name, "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("human get after update differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}

	cs, _, _ = e2eDo(t, http.MethodDelete, closedSrv.URL+"/api/v1/humans/"+name, "", "public-token")
	es, _, _ = e2eDo(t, http.MethodDelete, base+"/api/v1/humans/"+name, "", "public-token")
	if cs != es {
		t.Fatalf("human delete differs: closed=%d enabled=%d", cs, es)
	}
	cs, _, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/humans/"+name, "", "public-token")
	es, _, _ = e2eDo(t, http.MethodGet, base+"/api/v1/humans/"+name, "", "public-token")
	if cs != es || cs != http.StatusNotFound {
		t.Fatalf("human get after delete: closed=%d enabled=%d, want 404/404", cs, es)
	}
	cs, cb, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/humans", "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodGet, base+"/api/v1/humans", "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("human list after delete differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}
}
