package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestListWorkersTeamIndexListCount pins the issue #1475 fix: 5 workers +
// 3 teams cost exactly 2 K8s List calls (one WorkerList + one TeamList)
// instead of the old 1 WorkerList + 5 TeamList, and every returned worker's
// team/role/room enrichment matches the old findTeamMember +
// applyTeamMember path run on the same fake.
func TestListWorkersTeamIndexListCount(t *testing.T) {
	scheme := newServerTestScheme(t)

	solo := &v1beta1.Worker{}
	solo.Name = "solo"
	solo.Namespace = "default"

	lead := &v1beta1.Worker{}
	lead.Name = "alpha-lead"
	lead.Namespace = "default"

	dev := &v1beta1.Worker{}
	dev.Name = "alpha-dev"
	dev.Namespace = "default"
	// The worker's own status must win over team member status
	// (applyTeamMember only fills empty fields).
	dev.Status.RoomID = "room-alpha-dev"
	dev.Status.MatrixUserID = "@alpha-dev:example.com"

	beta := &v1beta1.Worker{}
	beta.Name = "beta-dev"
	beta.Namespace = "default"

	gamma := &v1beta1.Worker{}
	gamma.Name = "gamma-lead"
	gamma.Namespace = "default"

	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev"},
	}

	betaTeam := &v1beta1.Team{}
	betaTeam.Name = "beta-team"
	betaTeam.Namespace = "default"
	betaTeam.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "beta-dev"},
	}
	betaTeam.Status.Members = []v1beta1.TeamMemberStatus{
		{Name: "beta-dev", RoomID: "room-beta-dev", MatrixUserID: "@beta-dev:example.com"},
	}

	gammaTeam := &v1beta1.Team{}
	gammaTeam.Name = "gamma-team"
	gammaTeam.Namespace = "default"
	gammaTeam.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "gamma-lead", Role: "team_leader"},
	}

	fixtures := []*v1beta1.Worker{solo, lead, dev, beta, gamma}

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(solo, lead, dev, beta, gamma, alpha, betaTeam, gammaTeam).
		Build()
	counting := &countingClient{Client: rawClient}
	handler := NewResourceHandler(counting, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rec := httptest.NewRecorder()
	handler.ListWorkers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := counting.listCalls; got != 2 {
		t.Fatalf("List calls = %d, want 2 (one WorkerList + one TeamList)", got)
	}
	var list WorkerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Total != len(fixtures) {
		t.Fatalf("total = %d, want %d: %+v", list.Total, len(fixtures), list.Workers)
	}

	gotByName := map[string]WorkerResponse{}
	for _, got := range list.Workers {
		gotByName[got.Name] = got
	}

	// Equivalence: old logic (findTeamMember + applyTeamMember) on the same
	// fake must produce the same enrichment for every worker.
	for _, w := range fixtures {
		got, ok := gotByName[w.Name]
		if !ok {
			t.Fatalf("worker %s missing from response: %+v", w.Name, list.Workers)
		}
		team, member, found, err := findTeamMember(req.Context(), rawClient, "default", w.Name)
		if err != nil {
			t.Fatalf("findTeamMember(%s): %v", w.Name, err)
		}
		want := workerToResponse(w)
		if found {
			applyTeamMember(&want, team, member)
		}
		if got.Team != want.Team || got.Role != want.Role ||
			got.RoomID != want.RoomID || got.MatrixUserID != want.MatrixUserID {
			t.Errorf("worker %s: enrichment diverges from the findTeamMember path:\n got  team=%q role=%q roomID=%q matrixUserID=%q\n want team=%q role=%q roomID=%q matrixUserID=%q",
				w.Name, got.Team, got.Role, got.RoomID, got.MatrixUserID,
				want.Team, want.Role, want.RoomID, want.MatrixUserID)
		}
	}

	// Golden values pin the contract itself (the comparison above can only
	// track the findTeamMember path). Fields: team, role, roomID, matrixUserID.
	golden := map[string][4]string{
		"solo":       {"", "", "", ""},
		"alpha-lead": {"alpha-team", "team_leader", "", ""},
		"alpha-dev":  {"alpha-team", "worker", "room-alpha-dev", "@alpha-dev:example.com"},
		"beta-dev":   {"beta-team", "worker", "room-beta-dev", "@beta-dev:example.com"},
		"gamma-lead": {"gamma-team", "team_leader", "", ""},
	}
	for name, want := range golden {
		got := gotByName[name]
		if got.Team != want[0] || got.Role != want[1] || got.RoomID != want[2] || got.MatrixUserID != want[3] {
			t.Errorf("worker %s: got team=%q role=%q roomID=%q matrixUserID=%q, want team=%q role=%q roomID=%q matrixUserID=%q",
				name, got.Team, got.Role, got.RoomID, got.MatrixUserID,
				want[0], want[1], want[2], want[3])
		}
	}
}

// listFixtureClient is a client.Client whose List returns the fixtures in
// exactly the given slice order. The controller-runtime fake's list order
// is unspecified per call, so the first-hit assertion below needs a
// deterministic list source. Only List is functional — ListWorkers must not
// touch any other method.
type listFixtureClient struct {
	client.Client
	workers   []v1beta1.Worker
	teams     []v1beta1.Team
	listCalls int
}

func (c *listFixtureClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	c.listCalls++
	switch l := list.(type) {
	case *v1beta1.WorkerList:
		l.Items = make([]v1beta1.Worker, len(c.workers))
		copy(l.Items, c.workers)
	case *v1beta1.TeamList:
		l.Items = make([]v1beta1.Team, len(c.teams))
		copy(l.Items, c.teams)
	default:
		return fmt.Errorf("listFixtureClient: unsupported list type %T", list)
	}
	return nil
}

// TestListWorkersTeamIndexFirstHitWins pins the preserved first-hit
// semantics: when a worker is referenced by more than one team, the first
// team IN LIST ORDER wins (findTeamMember's behavior). The fixture lists
// zeta-team before alpha-team on purpose — alphabetically alpha-team would
// come first, so resolving shared-dev to zeta-team proves list order, not
// name order, decides.
func TestListWorkersTeamIndexFirstHitWins(t *testing.T) {
	shared := v1beta1.Worker{}
	shared.Name = "shared-dev"
	shared.Namespace = "default"

	standalone := v1beta1.Worker{}
	standalone.Name = "solo-dev"
	standalone.Namespace = "default"

	// Slice order is the list order: zeta-team first, alpha-team second.
	zeta := v1beta1.Team{}
	zeta.Name = "zeta-team"
	zeta.Namespace = "default"
	zeta.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "shared-dev", Role: "worker"},
	}
	alpha := v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "shared-dev", Role: "team_leader"},
		{Name: "solo-dev"},
	}

	newClient := func() *listFixtureClient {
		return &listFixtureClient{
			workers: []v1beta1.Worker{shared, standalone},
			teams:   []v1beta1.Team{zeta, alpha},
		}
	}

	counting := newClient()
	handler := NewResourceHandler(counting, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rec := httptest.NewRecorder()
	handler.ListWorkers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := counting.listCalls; got != 2 {
		t.Fatalf("List calls = %d, want 2 (one WorkerList + one TeamList)", got)
	}
	var list WorkerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Total != 2 {
		t.Fatalf("total = %d, want 2: %+v", list.Total, list.Workers)
	}
	for _, got := range list.Workers {
		if got.Name == "shared-dev" && (got.Team != "zeta-team" || got.Role != "worker") {
			t.Fatalf("shared-dev: team=%q role=%q, want zeta-team/worker — the first team in list order must win, not the alphabetically first", got.Team, got.Role)
		}
	}

	// Equivalence against the old logic on the same deterministic fixture.
	old := newClient()
	for _, got := range list.Workers {
		team, member, ok, err := findTeamMember(req.Context(), old, "default", got.Name)
		if err != nil {
			t.Fatalf("findTeamMember(%s): %v", got.Name, err)
		}
		if !ok {
			t.Fatalf("findTeamMember(%s): not found, but response carries team=%q", got.Name, got.Team)
		}
		var want WorkerResponse
		applyTeamMember(&want, team, member)
		if got.Team != want.Team || got.Role != want.Role ||
			got.RoomID != want.RoomID || got.MatrixUserID != want.MatrixUserID {
			t.Errorf("worker %s: got team=%q role=%q roomID=%q matrixUserID=%q, want team=%q role=%q roomID=%q matrixUserID=%q",
				got.Name, got.Team, got.Role, got.RoomID, got.MatrixUserID,
				want.Team, want.Role, want.RoomID, want.MatrixUserID)
		}
	}
}

// TestListWorkersTeamIndexSkipsTeamListWhenNoWorkers pins the call pattern
// for an empty worker list: with no workers there is nothing to index, so
// only the WorkerList round-trip happens — the old per-worker lookups were
// never reached either.
func TestListWorkersTeamIndexSkipsTeamListWhenNoWorkers(t *testing.T) {
	scheme := newServerTestScheme(t)
	counting := &countingClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	handler := NewResourceHandler(counting, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rec := httptest.NewRecorder()
	handler.ListWorkers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := counting.listCalls; got != 1 {
		t.Fatalf("List calls = %d, want 1 (WorkerList only — the team lookup was also skipped by the old code)", got)
	}
	var list WorkerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Total != 0 {
		t.Fatalf("total = %d, want 0", list.Total)
	}
}
