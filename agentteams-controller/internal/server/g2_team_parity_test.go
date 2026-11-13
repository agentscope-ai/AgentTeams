//go:build service_e2e

package server

// G2 team read parity: the controller's TeamResponse derivation and the
// service's imported-projection derivation must produce the same bytes for
// GET /api/v1/teams and GET /api/v1/teams/{name}, for an admin and for a
// team-scoped human. The service DB is seeded directly with the same Team
// objects the controller serves.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// multiStaticAuth resolves the two public tokens the parity test uses
// (admin and a team-scoped human) through one controller middleware.
type multiStaticAuth struct {
	ids map[string]*authpkg.CallerIdentity
}

func (a *multiStaticAuth) Authenticate(_ context.Context, token string) (*authpkg.CallerIdentity, error) {
	id, ok := a.ids[token]
	if !ok {
		return nil, errors.New("invalid token")
	}
	return id, nil
}

func TestG2TeamReadParity(t *testing.T) {
	bin := os.Getenv("AGENTTEAMS_TEST_SERVICE_BIN")
	resDSN, opsDSN := os.Getenv("AGENTTEAMS_TEST_RES_DSN"), os.Getenv("AGENTTEAMS_TEST_OPS_DSN")
	if bin == "" || resDSN == "" || opsDSN == "" {
		t.Skip("service_e2e: set AGENTTEAMS_TEST_SERVICE_BIN / AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN")
	}

	suffix := time.Now().UnixNano()
	teamAName := fmt.Sprintf("g2-team-a-%d", suffix)
	teamBName := fmt.Sprintf("g2-team-b-%d", suffix)

	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Team{}, &v1beta1.Worker{}).Build()
	mw := authpkg.NewMiddleware(&multiStaticAuth{ids: map[string]*authpkg.CallerIdentity{
		"admin-token": {
			Role: authpkg.RoleAdmin, Username: "admin", MatrixUserID: "@admin:matrix.local",
			Teams: []string{teamAName},
		},
		"human-token": {
			Role: authpkg.RoleHuman, Username: "human", MatrixUserID: "@human:matrix.local",
			HumanPermissionLevel: 2, Teams: []string{teamAName},
		},
	}}, nil, authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{
		Client: k8s, Namespace: "default", KubeMode: "embedded",
		AuthMw: mw, InternalToken: "internal-token", ControllerName: "ctl-g2",
		DefaultWorkerRuntime: "qwenpaw", DefaultModel: "m-default",
	})
	closed := httptest.NewServer(srv.Mux)
	defer closed.Close()

	createTeam := func(name, teamName, phase string) *v1beta1.Team {
		t.Helper()
		team := &v1beta1.Team{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: v1beta1.TeamSpec{
				TeamName:    teamName,
				Description: "team parity",
				Admin:       &v1beta1.TeamAdminSpec{Name: "admin", MatrixUserID: "@admin:mtx"},
				HumanMembers: []v1beta1.TeamMemberSpec{
					{Name: "h1", MatrixUserID: "@h1:mtx", Role: "coordinator"},
				},
				WorkerMembers: []v1beta1.TeamWorkerRef{
					{Name: "leader", Role: "team_leader"},
					{Name: "w1", Role: "worker"},
				},
				HeartbeatEvery:               "30m",
				SubagentModel:                "mini",
				LlmStreamFirstContentTimeout: "300s",
				LlmMaxQpm:                    "60",
			},
		}
		if err := k8s.Create(context.Background(), team); err != nil {
			t.Fatalf("create team %s: %v", name, err)
		}
		team.Status = v1beta1.TeamStatus{
			Phase: phase, TeamRoomID: "!team:mtx", LeaderDMRoomID: "!dm:mtx",
			LeaderReady: true, ReadyWorkers: 2, TotalWorkers: 3, Message: "ok",
			Members: []v1beta1.TeamMemberStatus{
				{Name: "w1", Role: "worker", ExposedPorts: []v1beta1.ExposedPortStatus{{Port: 8080, Domain: "w1.example"}}},
			},
		}
		if err := k8s.Status().Update(context.Background(), team); err != nil {
			t.Fatalf("status update %s: %v", name, err)
		}
		return team
	}
	teamA := createTeam(teamAName, "G2 Team A", "Active")
	teamB := createTeam(teamBName, "G2 Team B", "Pending")

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
	for _, q := range []string{"DELETE FROM team_projections", "DELETE FROM teams"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("clean %q: %v", q, err)
		}
	}
	now := time.Now().UTC()
	seed := func(uid string, team *v1beta1.Team) {
		t.Helper()
		raw, err := json.Marshal(team)
		if err != nil {
			t.Fatalf("marshal %s: %v", team.Name, err)
		}
		if _, err := db.Exec(
			"INSERT INTO teams (team_uid, team_slug, display_name, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			uid, team.Name, team.Spec.TeamName, team.Status.Phase, now, now); err != nil {
			t.Fatalf("insert team %s: %v", team.Name, err)
		}
		if _, err := db.Exec(
			"INSERT INTO team_projections (team_uid, team_json, updated_at) VALUES (?, ?, ?)",
			uid, raw, now); err != nil {
			t.Fatalf("insert projection %s: %v", team.Name, err)
		}
	}
	seed("01TEAM0000000000000000009", teamA)
	seed("01TEAM0000000000000000010", teamB)

	// compare checks status always and body when asked. Error bodies are not
	// byte-compatible by design (controller {"error":...} vs service
	// envelope), so hidden reads compare status only.
	compare := func(path, token, label string, compareBody bool) {
		t.Helper()
		cs, cb, _ := e2eDo(t, http.MethodGet, closed.URL+path, "", token)
		es, eb, _ := e2eDo(t, http.MethodGet, base+path, "", token)
		if cs != es {
			t.Fatalf("%s status differs: controller=%d service=%d", label, cs, es)
		}
		if compareBody && cb != eb {
			t.Fatalf("%s body differs: controller=%s service=%s", label, cb, eb)
		}
	}

	compare("/api/v1/teams/"+teamA.Name, "admin-token", "admin get A", true)
	compare("/api/v1/teams/"+teamB.Name, "admin-token", "admin get B", true)
	compare("/api/v1/teams/"+teamA.Name, "human-token", "human get A", true)
	compare("/api/v1/teams/"+teamB.Name, "human-token", "human get B hidden", false)
	compare("/api/v1/teams", "admin-token", "admin list", true)
	compare("/api/v1/teams", "human-token", "human list", true)
	compare("/api/v1/teams?fields=summary", "admin-token", "admin summary", true)
	compare("/api/v1/teams?fields=summary", "human-token", "human summary", true)
	// Error statuses must agree even though the bodies use different
	// envelopes (controller {"error"} vs service 11-code envelope).
	compare("/api/v1/teams?fields=bogus", "admin-token", "invalid fields", false)
	compare("/api/v1/teams?fields=summary,all", "admin-token", "combined fields", false)
}
