//go:build service_e2e

package server

// G2 team WRITE parity: the same POST/PUT/DELETE sent to the controller and
// through the service must return the same status/body, and the service's
// write-through must make GET/list agree afterwards.
//
// The two controllers use separate fake clients (like G1) so the same team
// name can exist in both. Each controller is seeded with the Worker CRs the
// team validator expects.

import (
	"context"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func TestG2TeamWriteParity(t *testing.T) {
	bin := os.Getenv("AGENTTEAMS_TEST_SERVICE_BIN")
	resDSN, opsDSN := os.Getenv("AGENTTEAMS_TEST_RES_DSN"), os.Getenv("AGENTTEAMS_TEST_OPS_DSN")
	if bin == "" || resDSN == "" || opsDSN == "" {
		t.Skip("service_e2e: set AGENTTEAMS_TEST_SERVICE_BIN / AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN")
	}

	newController := func() (*HTTPServer, client.Client) {
		scheme := newProjectTestScheme(t)
		k8s := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&v1beta1.Team{}, &v1beta1.Worker{}).Build()
		for _, worker := range []string{"leader", "w1"} {
			if err := k8s.Create(context.Background(), &v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: worker, Namespace: "default"},
			}); err != nil {
				t.Fatalf("seed worker %s: %v", worker, err)
			}
		}
		mw := authpkg.NewMiddleware(&internalStaticAuth{
			token: "public-token",
			id: &authpkg.CallerIdentity{
				Role: authpkg.RoleAdmin, Username: "admin", MatrixUserID: "@admin:matrix.local",
			},
		}, nil, authpkg.NewAuthorizer(), k8s, "default")
		srv := NewHTTPServer(":0", ServerDeps{
			Client: k8s, Namespace: "default", KubeMode: "embedded",
			AuthMw: mw, InternalToken: "internal-token", ControllerName: "ctl-g2w",
			DefaultWorkerRuntime: "qwenpaw", DefaultModel: "m-default",
		})
		return srv, k8s
	}
	closed, _ := newController()
	enabled, _ := newController()
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

	if err := cleanTeamRows(resDSN); err != nil {
		t.Fatalf("clean teams: %v", err)
	}

	name := fmt.Sprintf("g2-write-%d", time.Now().UnixNano())
	createBody := fmt.Sprintf(`{"name":%q,"teamName":"G2 Write","workerMembers":[{"name":"leader","role":"team_leader"},{"name":"w1","role":"worker"}]}`, name)

	cs, cb, _ := e2eDo(t, http.MethodPost, closedSrv.URL+"/api/v1/teams", createBody, "public-token")
	es, eb, _ := e2eDo(t, http.MethodPost, base+"/api/v1/teams", createBody, "public-token")
	if cs != es || cb != eb {
		t.Fatalf("write create differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}

	cs, cb, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/teams/"+name, "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodGet, base+"/api/v1/teams/"+name, "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("get after create differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}

	putBody := `{"teamName":"G2 Write 2"}`
	cs, cb, _ = e2eDo(t, http.MethodPut, closedSrv.URL+"/api/v1/teams/"+name, putBody, "public-token")
	es, eb, _ = e2eDo(t, http.MethodPut, base+"/api/v1/teams/"+name, putBody, "public-token")
	if cs != es || cb != eb {
		t.Fatalf("write update differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}
	cs, cb, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/teams/"+name, "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodGet, base+"/api/v1/teams/"+name, "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("get after update differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}

	cs, _, _ = e2eDo(t, http.MethodDelete, closedSrv.URL+"/api/v1/teams/"+name, "", "public-token")
	es, _, _ = e2eDo(t, http.MethodDelete, base+"/api/v1/teams/"+name, "", "public-token")
	if cs != es {
		t.Fatalf("delete status differs: closed=%d enabled=%d", cs, es)
	}

	cs, _, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/teams/"+name, "", "public-token")
	es, _, _ = e2eDo(t, http.MethodGet, base+"/api/v1/teams/"+name, "", "public-token")
	if cs != es || cs != http.StatusNotFound {
		t.Fatalf("get after delete: closed=%d enabled=%d, want 404/404", cs, es)
	}

	cs, cb, _ = e2eDo(t, http.MethodGet, closedSrv.URL+"/api/v1/teams", "", "public-token")
	es, eb, _ = e2eDo(t, http.MethodGet, base+"/api/v1/teams", "", "public-token")
	if cs != es || cb != eb {
		t.Fatalf("list after delete differs: status %d/%d closed=%s enabled=%s", cs, es, cb, eb)
	}
}

// cleanTeamRows starts the write-parity test from an empty res.teams state.
func cleanTeamRows(resDSN string) error {
	db, err := sql.Open("mysql", resDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, q := range []string{"DELETE FROM team_deletions", "DELETE FROM team_projections", "DELETE FROM teams"} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}
