//go:build service_e2e

package server

// G2 human read parity: the controller HumanResponse and the service
// projection render the same bytes for GET /api/v1/humans (full + summary)
// and GET /api/v1/humans/{name}, including the detail-only initialPassword.

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func TestG2HumanReadParity(t *testing.T) {
	bin := os.Getenv("AGENTTEAMS_TEST_SERVICE_BIN")
	resDSN, opsDSN := os.Getenv("AGENTTEAMS_TEST_RES_DSN"), os.Getenv("AGENTTEAMS_TEST_OPS_DSN")
	if bin == "" || resDSN == "" || opsDSN == "" {
		t.Skip("service_e2e: set AGENTTEAMS_TEST_SERVICE_BIN / AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN")
	}

	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Human{}).Build()
	mw := authpkg.NewMiddleware(&internalStaticAuth{
		token: "public-token",
		id: &authpkg.CallerIdentity{
			Role: authpkg.RoleAdmin, Username: "admin", MatrixUserID: "@admin:matrix.local",
		},
	}, nil, authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{
		Client: k8s, Namespace: "default", KubeMode: "embedded",
		AuthMw: mw, InternalToken: "internal-token", ControllerName: "ctl-g2h",
		DefaultWorkerRuntime: "qwenpaw", DefaultModel: "m-default",
	})
	closed := httptest.NewServer(srv.Mux)
	defer closed.Close()

	name := fmt.Sprintf("g2-human-%d", time.Now().UnixNano())
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{v1beta1.LabelSystemAdmin: "true"},
		},
		Spec: v1beta1.HumanSpec{
			DisplayName: "G2 Human", Username: name, Email: "g2@example.com",
			PermissionLevel: 1, AccessibleTeams: []string{"team-a"},
			AccessibleWorkers: []string{"w1"}, Capabilities: []string{"cap-x"},
			Note: "parity",
			ModelAccess: &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
				Role: "worker", Table: "tab-1",
			}}},
		},
	}
	if err := k8s.Create(context.Background(), human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	human.Status = v1beta1.HumanStatus{
		Phase: "Active", MatrixUserID: "@g2:matrix.local",
		InitialPassword: "one-shot", Rooms: []string{"!room:matrix.local"}, Message: "ok",
	}
	if err := k8s.Status().Update(context.Background(), human); err != nil {
		t.Fatalf("status update: %v", err)
	}

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
		"AGENTTEAMS_SERVICE_CONTROLLER_URL="+closed.URL,
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

	raw, err := json.Marshal(human)
	if err != nil {
		t.Fatalf("marshal human: %v", err)
	}
	db, err := sql.Open("mysql", resDSN)
	if err != nil {
		t.Fatalf("open res: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM humans"); err != nil {
		t.Fatalf("clean humans: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO humans (human_uid, username, human_json, updated_at) VALUES (?, ?, ?, ?)",
		"01HUMAN0000000000000000009", name, raw, time.Now().UTC()); err != nil {
		t.Fatalf("insert human: %v", err)
	}

	compare := func(path, label string, compareBody bool) {
		t.Helper()
		cs, cb, _ := e2eDo(t, http.MethodGet, closed.URL+path, "", "public-token")
		es, eb, _ := e2eDo(t, http.MethodGet, base+path, "", "public-token")
		if cs != es {
			t.Fatalf("%s status differs: controller=%d service=%d", label, cs, es)
		}
		if compareBody && cb != eb {
			t.Fatalf("%s body differs: controller=%s service=%s", label, cb, eb)
		}
	}

	compare("/api/v1/humans/"+name, "human get", true)
	compare("/api/v1/humans", "human list", true)
	compare("/api/v1/humans?fields=summary", "human summary", true)
	compare("/api/v1/humans/missing", "human missing", false)
}
