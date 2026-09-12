package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newSkillsRig builds a template tree:
//
//	base/worker-agent/skills/{file-sync,find-skills}
//	base/copaw-worker-agent/skills/{file-sync,task-progress}
//	base/team-leader-agent/skills/{leader-briefing}
//	(no hermes dir; a stray non-dir file in worker-agent/skills)
//
// and a shared OSS store:
//
//	agents/global/skills/shared-kb/SKILL.md   (directory entry → listed)
//	agents/global/skills/file-sync/SKILL.md   (collides with builtin → builtin wins)
//	agents/global/skills/notes.txt            (bare file → skipped)
//	agents/global/skills/.hidden/SKILL.md     (dot-entry → skipped)
func newSkillsRig(t *testing.T) (*SkillsHandler, *mcLikeOSS, string) {
	t.Helper()
	base := t.TempDir()
	writeSkill(t, filepath.Join(base, "worker-agent", "skills"), "file-sync", "Sync files with centralized storage.")
	writeSkill(t, filepath.Join(base, "worker-agent", "skills"), "find-skills", "Discover skills from the open ecosystem.")
	if err := os.WriteFile(filepath.Join(base, "worker-agent", "skills", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(base, "copaw-worker-agent", "skills"), "file-sync", "Sync files with centralized storage (copaw).")
	writeSkill(t, filepath.Join(base, "copaw-worker-agent", "skills"), "task-progress", "Report task progress.")
	writeSkill(t, filepath.Join(base, "team-leader-agent", "skills"), "leader-briefing", "Brief team members.")

	store := ossfake.NewMemory()
	mustPut := func(key string) {
		t.Helper()
		if err := store.PutObject(context.Background(), key, []byte("---\nname: x\n---\n")); err != nil {
			t.Fatal(err)
		}
	}
	mustPut(globalSkillsPrefix + "shared-kb/SKILL.md")
	mustPut(globalSkillsPrefix + "file-sync/SKILL.md")
	mustPut(globalSkillsPrefix + "notes.txt")
	mustPut(globalSkillsPrefix + ".hidden/SKILL.md")
	// Team-skill layer: market-team has a team-only skill and a builtin
	// name-collision (builtin must win); biz-team has its own.
	mustPut("teams/market-team/skills/team-kb/SKILL.md")
	mustPut("teams/market-team/skills/file-sync/SKILL.md")
	mustPut("teams/biz-team/skills/biz-only/SKILL.md")

	// Team CRs for the ?team= existence check (unknown-team → 404).
	teams := []*v1beta1.Team{
		{ObjectMeta: metav1.ObjectMeta{Name: "market-team", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "biz-team", Namespace: "default"}},
	}
	objs := make([]runtime.Object, 0, len(teams))
	for _, tm := range teams {
		objs = append(objs, tm)
	}
	k8s := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithRuntimeObjects(objs...).Build()

	fakeOSS := &mcLikeOSS{Memory: store}
	dir := filepath.Join(base, "worker-agent")
	return NewSkillsHandler(dir, fakeOSS, k8s, "default"), fakeOSS, base
}

func getSkills(t *testing.T, h *SkillsHandler) *httptest.ResponseRecorder {
	t.Helper()
	req := withCaller(httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil),
		&authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListSkills(rec, req)
	return rec
}

func decodeSkills(t *testing.T, rec *httptest.ResponseRecorder) []SkillInfo {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp SkillListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Skills
}

func skillByName(t *testing.T, skills []SkillInfo, name string) SkillInfo {
	t.Helper()
	for _, s := range skills {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("skill %q not in catalog: %v", name, skills)
	return SkillInfo{}
}

func getSkillsAs(t *testing.T, h *SkillsHandler, caller *authpkg.CallerIdentity, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := withCaller(httptest.NewRequest(http.MethodGet, "/api/v1/skills"+query, nil), caller)
	rec := httptest.NewRecorder()
	h.ListSkills(rec, req)
	return rec
}

var (
	skAdmin   = &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
	skL2      = &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"market-team"}}
	skL2Empty = &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "nobody"}
	skLeader  = &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "market-lead", Team: "market-team"}
	skManager = &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}
	skWorker  = &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "market-dev", Team: "market-team"}
)

func TestSkills_TeamScopeAdmin(t *testing.T) {
	h, _, _ := newSkillsRig(t)

	// Own (any) team: builtin + team layer, no shared half.
	rec := getSkillsAs(t, h, skAdmin, "?team=market-team")
	skills := decodeSkills(t, rec)
	teamKB := skillByName(t, skills, "team-kb")
	if teamKB.Source != "team" {
		t.Errorf("team-kb source = %q, want team", teamKB.Source)
	}
	// Builtin name wins over a same-named team skill.
	if s := skillByName(t, skills, "file-sync"); s.Source != "builtin" {
		t.Errorf("file-sync source = %q, want builtin (team entry shadowed)", s.Source)
	}
	// The deployment shared half is NOT part of the team view.
	for _, s := range skills {
		if s.Name == "shared-kb" {
			t.Error("shared-kb leaked into the team view")
		}
		if s.Name == "biz-only" {
			t.Error("biz-team skill leaked into market-team view")
		}
	}

	// Unknown team → 404.
	if rec := getSkillsAs(t, h, skAdmin, "?team=no-such-team"); rec.Code != http.StatusNotFound {
		t.Errorf("admin unknown team: status = %d, want 404", rec.Code)
	}
}

func TestSkills_TeamScopeL2(t *testing.T) {
	h, _, _ := newSkillsRig(t)

	// Own team: 200 with the team layer.
	skills := decodeSkills(t, getSkillsAs(t, h, skL2, "?team=market-team"))
	if s := skillByName(t, skills, "team-kb"); s.Source != "team" {
		t.Errorf("team-kb source = %q, want team", s.Source)
	}
	// Builtin half still present for the team view.
	_ = skillByName(t, skills, "file-sync")

	// Cross-team → 404 (indistinguishable from unknown, W8 anti-probing).
	if rec := getSkillsAs(t, h, skL2, "?team=biz-team"); rec.Code != http.StatusNotFound {
		t.Errorf("cross-team: status = %d, want 404", rec.Code)
	}
	if rec := getSkillsAs(t, h, skL2, "?team=no-such-team"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown team: status = %d, want 404", rec.Code)
	}
	// Empty accessibleTeams → 404 for any team.
	if rec := getSkillsAs(t, h, skL2Empty, "?team=market-team"); rec.Code != http.StatusNotFound {
		t.Errorf("empty teams: status = %d, want 404", rec.Code)
	}
	// No-param stays #1211's L1-only contract.
	if rec := getSkillsAs(t, h, skL2, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("L2 no-param: status = %d, want 400", rec.Code)
	}
}

func TestSkills_TeamScopeLeader(t *testing.T) {
	h, _, _ := newSkillsRig(t)

	skills := decodeSkills(t, getSkillsAs(t, h, skLeader, "?team=market-team"))
	if s := skillByName(t, skills, "team-kb"); s.Source != "team" {
		t.Errorf("team-kb source = %q, want team", s.Source)
	}
	if rec := getSkillsAs(t, h, skLeader, "?team=biz-team"); rec.Code != http.StatusNotFound {
		t.Errorf("leader other team: status = %d, want 404", rec.Code)
	}
}

func TestSkills_TeamScopeForbiddenRoles(t *testing.T) {
	h, _, _ := newSkillsRig(t)
	// Manager does not participate in team-skill paths; worker never.
	if rec := getSkillsAs(t, h, skManager, "?team=market-team"); rec.Code != http.StatusForbidden {
		t.Errorf("manager: status = %d, want 403", rec.Code)
	}
	if rec := getSkillsAs(t, h, skWorker, "?team=market-team"); rec.Code != http.StatusForbidden {
		t.Errorf("worker: status = %d, want 403", rec.Code)
	}
}

func TestSkills_TeamScopeListingFailureDegrades(t *testing.T) {
	h, fakeOSS, _ := newSkillsRig(t)
	fakeOSS.failList = true

	// Storage down: the team half degrades to an empty set; the builtin
	// half (local disk) is unaffected and the request still succeeds.
	rec := getSkillsAs(t, h, skAdmin, "?team=market-team")
	skills := decodeSkills(t, rec)
	for _, s := range skills {
		if s.Name == "team-kb" {
			t.Error("team-kb present despite listing failure")
		}
	}
	_ = skillByName(t, skills, "file-sync") // builtin survives
}

// TestSkillsCatalogGolden covers the builtin half (per-runtime availability
// derived from service.BuiltinAgentDir) and the shared half (directory
// entries under agents/global/skills/ only).
func TestSkillsCatalogGolden(t *testing.T) {
	h, _, _ := newSkillsRig(t)
	skills := decodeSkills(t, getSkills(t, h))

	wantNames := []string{"file-sync", "find-skills", "leader-briefing", "shared-kb", "task-progress"}
	var gotNames []string
	for _, s := range skills {
		gotNames = append(gotNames, s.Name)
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("names = %v, want %v", gotNames, wantNames)
	}

	// builtin provided by two templates → union of both templates' runtimes
	// (default worker template serves every runtime except copaw/hermes;
	// copaw template serves copaw; hermes dir absent so hermes is missing)
	fs := skillByName(t, skills, "file-sync")
	if fs.Source != "builtin" {
		t.Errorf("file-sync source = %q, want builtin", fs.Source)
	}
	wantAgents := []string{"copaw-worker-agent", "worker-agent"}
	if !reflect.DeepEqual(fs.Agents, wantAgents) {
		t.Errorf("file-sync agents = %v, want %v", fs.Agents, wantAgents)
	}
	wantRuntimes := []string{"copaw", "deepseek-harness", "openclaw", "openhuman", "qwenpaw"}
	if !reflect.DeepEqual(fs.Runtimes, wantRuntimes) {
		t.Errorf("file-sync runtimes = %v, want %v", fs.Runtimes, wantRuntimes)
	}

	fsk := skillByName(t, skills, "find-skills")
	if want := []string{"deepseek-harness", "openclaw", "openhuman", "qwenpaw"}; !reflect.DeepEqual(fsk.Runtimes, want) {
		t.Errorf("find-skills runtimes = %v, want %v", fsk.Runtimes, want)
	}

	tp := skillByName(t, skills, "task-progress")
	if want := []string{"copaw"}; !reflect.DeepEqual(tp.Runtimes, want) {
		t.Errorf("task-progress runtimes = %v, want %v", tp.Runtimes, want)
	}

	// leader template serves every runtime (leader role exists on all runtimes)
	lb := skillByName(t, skills, "leader-briefing")
	if want := append([]string{}, service.AllWorkerRuntimes...); !reflect.DeepEqual(lb.Runtimes, sortedCopy(want)) {
		t.Errorf("leader-briefing runtimes = %v, want %v", lb.Runtimes, sortedCopy(want))
	}
	if len(lb.Agents) != 1 || lb.Agents[0] != "team-leader-agent" {
		t.Errorf("leader-briefing agents = %v, want [team-leader-agent]", lb.Agents)
	}

	// shared half: directory entry only; builtin name wins on collision
	sk := skillByName(t, skills, "shared-kb")
	if sk.Source != "shared" {
		t.Errorf("shared-kb source = %q, want shared", sk.Source)
	}
	if !reflect.DeepEqual(sk.Runtimes, sortedCopy(append([]string{}, service.AllWorkerRuntimes...))) {
		t.Errorf("shared-kb runtimes = %v, want all runtimes", sk.Runtimes)
	}
}

// TestSkillsCatalogMappingConsistency pins the catalog's template→runtime
// derivation to service.BuiltinAgentDir: for every (role, runtime) pair the
// catalog must credit exactly the template the Deployer would seed from.
func TestSkillsCatalogMappingConsistency(t *testing.T) {
	h, _, _ := newSkillsRig(t)
	templates := h.builtinTemplates()
	byRuntime := map[string]map[string]bool{} // runtime → set of template dirs
	for _, tmpl := range templates {
		for _, rt := range tmpl.runtimes {
			if byRuntime[rt] == nil {
				byRuntime[rt] = map[string]bool{}
			}
			byRuntime[rt][tmpl.dir] = true
		}
	}
	for _, role := range []string{"worker", "team_leader"} {
		for _, rt := range service.AllWorkerRuntimes {
			want := service.BuiltinAgentDir(h.workerAgentDir, role, rt)
			if !byRuntime[rt][want] {
				t.Errorf("(role=%s, runtime=%s): BuiltinAgentDir = %s, but catalog does not credit it for %s",
					role, rt, want, rt)
			}
		}
	}
}

// TestSkillsCatalogFieldDiscipline asserts the response schema carries
// identity/availability only — no content, credential, or registry fields
// can sneak in.
func TestSkillsCatalogFieldDiscipline(t *testing.T) {
	h, _, _ := newSkillsRig(t)
	rec := getSkills(t, h)
	var payload struct {
		Skills []map[string]any `json:"skills"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != len(payload.Skills) {
		t.Fatalf("total = %d, entries = %d", payload.Total, len(payload.Skills))
	}
	allowed := map[string]bool{"name": true, "description": true, "source": true, "version": true, "requirements": true, "updated_at": true, "agents": true, "runtimes": true}
	for _, entry := range payload.Skills {
		for k := range entry {
			if !allowed[k] {
				t.Errorf("unexpected field %q in catalog entry %v", k, entry)
			}
		}
	}
}

// TestSkillsCatalogSharedDegradesOnOSSFailure: a listing failure degrades to
// an empty shared half; the catalog still serves builtins with 200.
func TestSkillsCatalogSharedDegradesOnOSSFailure(t *testing.T) {
	base := t.TempDir()
	writeSkill(t, filepath.Join(base, "worker-agent", "skills"), "file-sync", "Sync files.")
	failing := &mcLikeOSS{Memory: ossfake.NewMemory(), failList: true}
	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), failing, nil, "default")

	skills := decodeSkills(t, getSkills(t, h))
	if len(skills) != 1 || skills[0].Name != "file-sync" || skills[0].Source != "builtin" {
		t.Fatalf("skills = %v, want builtin-only [file-sync]", skills)
	}
}

// TestSkillsCatalogNoTemplateDir: an empty workerAgentDir yields no builtins
// but still lists shared skills.
func TestSkillsCatalogNoTemplateDir(t *testing.T) {
	store := ossfake.NewMemory()
	if err := store.PutObject(context.Background(), globalSkillsPrefix+"shared-kb/SKILL.md", []byte("x")); err != nil {
		t.Fatal(err)
	}
	h := NewSkillsHandler("", &mcLikeOSS{Memory: store}, nil, "default")
	skills := decodeSkills(t, getSkills(t, h))
	if len(skills) != 1 || skills[0].Name != "shared-kb" || skills[0].Source != "shared" {
		t.Fatalf("skills = %v, want shared-only [shared-kb]", skills)
	}
}

// TestSkills_NonAdminNoTeam_400 locks the two-layer skill model contract:
// the deployment-level catalog is admin (L1) only. L2 humans, team leaders,
// workers, and the manager are meant to use the team-scoped catalog
// (?team=), which is not available yet — so a team-less request from a
// non-admin is rejected with a self-explanatory 400 (the positive admin
// 200 path is covered by TestSkillsCatalogGolden; per the #1214 discipline
// both sides are asserted).
func TestSkills_NonAdminNoTeam_400(t *testing.T) {
	cases := []struct {
		name   string
		caller *authpkg.CallerIdentity
	}{
		{"l2-human", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"market-team"}}},
		{"team-leader", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}},
		{"worker", &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-worker-1"}},
		{"manager", &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newSkillsRig(t)
			req := withCaller(httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil), tc.caller)
			rec := httptest.NewRecorder()
			h.ListSkills(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "team scope required") {
				t.Fatalf("error not self-explanatory: %s", rec.Body.String())
			}
		})
	}

	t.Run("no-caller", func(t *testing.T) {
		h, _, _ := newSkillsRig(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
		rec := httptest.NewRecorder()
		h.ListSkills(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// writeSkillWithFrontmatter writes a skill dir whose SKILL.md carries the
// given raw frontmatter block (unquoted, caller controls exact YAML).
func writeSkillWithFrontmatter(t *testing.T, dir, name, frontmatter string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSkillsCatalogFrontmatterExtension covers the version + requires
// declarations, following QwenPaw 2.2.x semantics: metadata namespace
// requires win over top-level, a bare list is shorthand for bins, and
// entries that declare nothing keep the fields omitted (omitempty).
func TestSkillsCatalogFrontmatterExtension(t *testing.T) {
	base := t.TempDir()
	skillRoot := filepath.Join(base, "worker-agent", "skills")

	writeSkillWithFrontmatter(t, skillRoot, "full-skill",
		"name: full-skill\n"+
			"description: Declares everything.\n"+
			"version: 1.2.0\n"+
			"metadata:\n"+
			"  qwenpaw:\n"+
			"    requires:\n"+
			"      bins: [ffmpeg, curl]\n"+
			"      env: [API_KEY]\n"+
			"      mcp: [web-search]\n")
	writeSkillWithFrontmatter(t, skillRoot, "bins-only",
		"name: bins-only\n"+
			"description: Top-level bare list.\n"+
			"requires: [git, jq]\n")
	writeSkill(t, skillRoot, "plain-skill", "Declares nothing.")

	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), &mcLikeOSS{Memory: ossfake.NewMemory()}, nil, "default")
	skills := decodeSkills(t, getSkills(t, h))

	full := skillByName(t, skills, "full-skill")
	if full.Version != "1.2.0" {
		t.Errorf("full-skill version = %q, want 1.2.0", full.Version)
	}
	if full.Requirements == nil {
		t.Fatalf("full-skill requirements = nil, want populated")
	}
	if want := []string{"curl", "ffmpeg"}; !reflect.DeepEqual(full.Requirements.RequireBins, want) {
		t.Errorf("require_bins = %v, want %v", full.Requirements.RequireBins, want)
	}
	if want := []string{"API_KEY"}; !reflect.DeepEqual(full.Requirements.RequireEnvs, want) {
		t.Errorf("require_envs = %v, want %v", full.Requirements.RequireEnvs, want)
	}
	if want := []string{"web-search"}; !reflect.DeepEqual(full.Requirements.RequireMcps, want) {
		t.Errorf("require_mcps = %v, want %v", full.Requirements.RequireMcps, want)
	}

	bins := skillByName(t, skills, "bins-only")
	if bins.Requirements == nil {
		t.Fatalf("bins-only requirements = nil, want populated")
	}
	if want := []string{"git", "jq"}; !reflect.DeepEqual(bins.Requirements.RequireBins, want) {
		t.Errorf("require_bins = %v, want %v", bins.Requirements.RequireBins, want)
	}

	plain := skillByName(t, skills, "plain-skill")
	if plain.Version != "" || plain.Requirements != nil {
		t.Errorf("plain-skill = %+v, want version/requirements omitted", plain)
	}
}

// TestSkillsCatalogSharedUpdatedAt pins that shared entries carry the
// listing timestamp (the fake's fixed write clock) and that builtin entries
// never do (updated_at is shared-only metadata).
func TestSkillsCatalogSharedUpdatedAt(t *testing.T) {
	base := t.TempDir()
	skillRoot := filepath.Join(base, "worker-agent", "skills")
	writeSkill(t, skillRoot, "built-in", "A builtin skill.")

	fakeOSS := ossfake.NewMemory()
	if err := fakeOSS.PutObject(context.Background(), "agents/global/skills/team-report/SKILL.md", []byte("---\nname: team-report\n---\n")); err != nil {
		t.Fatal(err)
	}
	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), &mcLikeOSS{Memory: fakeOSS}, nil, "default")
	skills := decodeSkills(t, getSkills(t, h))

	shared := skillByName(t, skills, "team-report")
	want := fakeOSS.LastWriteTime().UTC().Format(time.RFC3339)
	if shared.UpdatedAt != want {
		t.Errorf("shared updated_at = %q, want %q", shared.UpdatedAt, want)
	}

	builtin := skillByName(t, skills, "built-in")
	if builtin.UpdatedAt != "" {
		t.Errorf("builtin updated_at = %q, want omitted", builtin.UpdatedAt)
	}
}

// TestSkillsCatalogRequiresNamespacePrecedence pins the 2.2.x rule: a
// namespace requires block shadows the top-level one.
func TestSkillsCatalogRequiresNamespacePrecedence(t *testing.T) {
	base := t.TempDir()
	skillRoot := filepath.Join(base, "worker-agent", "skills")
	writeSkillWithFrontmatter(t, skillRoot, "ns-skill",
		"name: ns-skill\n"+
			"description: Namespace wins.\n"+
			"requires: [top-level-bin]\n"+
			"metadata:\n"+
			"  openclaw:\n"+
			"    requires:\n"+
			"      mcp: [ns-mcp]\n")
	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), &mcLikeOSS{Memory: ossfake.NewMemory()}, nil, "default")
	skills := decodeSkills(t, getSkills(t, h))

	ns := skillByName(t, skills, "ns-skill")
	if ns.Requirements == nil {
		t.Fatal("ns-skill requirements = nil")
	}
	if len(ns.Requirements.RequireBins) != 0 {
		t.Errorf("require_bins = %v, want empty (namespace shadows top-level)", ns.Requirements.RequireBins)
	}
	if want := []string{"ns-mcp"}; !reflect.DeepEqual(ns.Requirements.RequireMcps, want) {
		t.Errorf("require_mcps = %v, want %v", ns.Requirements.RequireMcps, want)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	// simple insertion sort keeps the test dependency-free
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
