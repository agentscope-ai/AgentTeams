package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"mime/multipart"
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
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/skillscan"
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
func newSkillsRig(t *testing.T, scanner skillscan.SkillScanner) (*SkillsHandler, *mcLikeOSS, string) {
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
	return NewSkillsHandler(dir, "", fakeOSS, k8s, "default", scanner), fakeOSS, base
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
	skL2      = &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}}
	skL2Empty = &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "nobody"}
	skLeader  = &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "market-lead", Team: "market-team"}
	skManager = &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}
	skWorker  = &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "market-dev", Team: "market-team"}
)

func TestSkills_TeamScopeAdmin(t *testing.T) {
	h, _, _ := newSkillsRig(t, nil)

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
	h, _, _ := newSkillsRig(t, nil)

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
	h, _, _ := newSkillsRig(t, nil)

	skills := decodeSkills(t, getSkillsAs(t, h, skLeader, "?team=market-team"))
	if s := skillByName(t, skills, "team-kb"); s.Source != "team" {
		t.Errorf("team-kb source = %q, want team", s.Source)
	}
	if rec := getSkillsAs(t, h, skLeader, "?team=biz-team"); rec.Code != http.StatusNotFound {
		t.Errorf("leader other team: status = %d, want 404", rec.Code)
	}
}

func TestSkills_TeamScopeForbiddenRoles(t *testing.T) {
	h, _, _ := newSkillsRig(t, nil)
	// Manager does not participate in team-skill paths; worker never.
	if rec := getSkillsAs(t, h, skManager, "?team=market-team"); rec.Code != http.StatusForbidden {
		t.Errorf("manager: status = %d, want 403", rec.Code)
	}
	if rec := getSkillsAs(t, h, skWorker, "?team=market-team"); rec.Code != http.StatusForbidden {
		t.Errorf("worker: status = %d, want 403", rec.Code)
	}
}

// verdictScanner is a fake upload-scan backend for handler tests.
type verdictScanner struct {
	verdict skillscan.SkillScanVerdict
	err     error
}

func (v verdictScanner) ScanSkill(_ context.Context, _ string, _ map[string][]byte) (skillscan.SkillScanVerdict, error) {
	return v.verdict, v.err
}

// skillZip renders an in-memory skill zip: one top-level directory named
// `name` holding SKILL.md (frontmatter name = name) plus the extra files.
func skillZip(name string, extra map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(name + "/SKILL.md")
	_, _ = w.Write([]byte("---\nname: " + name + "\n---\n"))
	for path, content := range extra {
		w, _ := zw.Create(name + "/" + path)
		_, _ = w.Write([]byte(content))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func postSkill(t *testing.T, h *SkillsHandler, caller *authpkg.CallerIdentity, scope, team string, zipData []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("scope", scope)
	if team != "" {
		_ = mw.WriteField("team", team)
	}
	fw, _ := mw.CreateFormFile("file", "skill.zip")
	_, _ = fw.Write(zipData)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req = withCaller(req, caller)
	rec := httptest.NewRecorder()
	h.UploadSkill(rec, req)
	return rec
}

var passScanner = verdictScanner{verdict: skillscan.SkillScanVerdict{Status: "pass"}}

func TestSkills_UploadAdmin200(t *testing.T) {
	h, store, _ := newSkillsRig(t, passScanner)

	rec := postSkill(t, h, skAdmin, "team", "market-team", skillZip("team-tool", map[string]string{"scripts/run.sh": "#!/bin/sh\necho hi\n"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp SkillUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Team != "market-team" || resp.Name != "team-tool" || resp.Scope != "team" || resp.Files != 2 {
		t.Errorf("response = %+v", resp)
	}
	if resp.Scan == nil || resp.Scan.Status != "pass" {
		t.Errorf("scan = %+v, want pass", resp.Scan)
	}
	// The files land under the team layer, readable back through the fake.
	data, err := store.Memory.GetObject(context.Background(), "teams/market-team/skills/team-tool/SKILL.md")
	if err != nil {
		t.Fatalf("SKILL.md not stored: %v", err)
	}
	if !strings.Contains(string(data), "name: team-tool") {
		t.Errorf("SKILL.md = %q", data)
	}
	if err := store.Memory.Stat(context.Background(), "teams/market-team/skills/team-tool/scripts/run.sh"); err != nil {
		t.Errorf("scripts/run.sh not stored: %v", err)
	}
	// The new skill appears in the team-scope catalog.
	skills := decodeSkills(t, getSkillsAs(t, h, skAdmin, "?team=market-team"))
	if s := skillByName(t, skills, "team-tool"); s.Source != "team" {
		t.Errorf("team-tool source = %q, want team", s.Source)
	}
}

func TestSkills_UploadDeploymentScope(t *testing.T) {
	h, store, _ := newSkillsRig(t, passScanner)
	zip := skillZip("global-tool", nil)

	// admin scope=deployment → 200, object lands under agents/global/skills/.
	rec := postSkill(t, h, skAdmin, "deployment", "", zip)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin deployment: status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp SkillUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Scope != "deployment" || resp.Team != "" || resp.Name != "global-tool" {
		t.Errorf("response = %+v", resp)
	}
	if err := store.Memory.Stat(context.Background(), globalSkillsPrefix+"global-tool/SKILL.md"); err != nil {
		t.Errorf("global-tool not under the deployment prefix: %v", err)
	}
	// It shows up in the no-param (L1) catalog as source "shared".
	skills := decodeSkills(t, getSkills(t, h))
	if s := skillByName(t, skills, "global-tool"); s.Source != "shared" {
		t.Errorf("global-tool source = %q, want shared", s.Source)
	}

	// L2 scope=deployment → 403 (deployment-wide writes are admin only).
	if rec := postSkill(t, h, skL2, "deployment", "", zip); rec.Code != http.StatusForbidden {
		t.Errorf("L2 deployment: status = %d, want 403", rec.Code)
	}
}

func TestSkills_UploadScopeMatrix(t *testing.T) {
	h, _, _ := newSkillsRig(t, passScanner)
	zip := skillZip("team-tool", nil)

	// L2 own team: 200.
	if rec := postSkill(t, h, skL2, "team", "market-team", zip); rec.Code != http.StatusOK {
		t.Errorf("L2 own team: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// L2 cross-team: 404 (indistinguishable from unknown, W8 anti-probing).
	if rec := postSkill(t, h, skL2, "team", "biz-team", zip); rec.Code != http.StatusNotFound {
		t.Errorf("L2 cross-team: status = %d, want 404", rec.Code)
	}
	if rec := postSkill(t, h, skL2, "team", "no-such", zip); rec.Code != http.StatusNotFound {
		t.Errorf("L2 unknown team: status = %d, want 404", rec.Code)
	}
	// Leader: 403 — leaders read the catalog (assign surface) but never
	// publish (pinned at the authorizer AND re-checked in the handler).
	if rec := postSkill(t, h, skLeader, "team", "market-team", zip); rec.Code != http.StatusForbidden {
		t.Errorf("leader: status = %d, want 403", rec.Code)
	}
	// Manager / worker: 403 (authorizer denies; the handler re-checks).
	if rec := postSkill(t, h, skManager, "team", "market-team", zip); rec.Code != http.StatusForbidden {
		t.Errorf("manager: status = %d, want 403", rec.Code)
	}
	if rec := postSkill(t, h, skWorker, "team", "market-team", zip); rec.Code != http.StatusForbidden {
		t.Errorf("worker: status = %d, want 403", rec.Code)
	}
}

func TestSkills_UploadStructureValidation(t *testing.T) {
	h, _, _ := newSkillsRig(t, passScanner)

	// A zip missing SKILL.md (only a readme in the root dir).
	noSkillMD := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("team-tool/readme.md")
		_, _ = w.Write([]byte("x"))
		_ = zw.Close()
		return buf.Bytes()
	}
	// Frontmatter name != directory name.
	nameMismatch := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("team-tool/SKILL.md")
		_, _ = w.Write([]byte("---\nname: other-name\n---\n"))
		_ = zw.Close()
		return buf.Bytes()
	}
	// No frontmatter at all.
	noFrontmatter := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("team-tool/SKILL.md")
		_, _ = w.Write([]byte("# just a heading\n"))
		_ = zw.Close()
		return buf.Bytes()
	}
	// Two top-level directories.
	twoRoots := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("team-tool/SKILL.md")
		_, _ = w.Write([]byte("---\nname: team-tool\n---\n"))
		w, _ = zw.Create("other/SKILL.md")
		_, _ = w.Write([]byte("---\nname: other\n---\n"))
		_ = zw.Close()
		return buf.Bytes()
	}
	// Zip-slip: a ../ entry.
	slip := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("team-tool/SKILL.md")
		_, _ = w.Write([]byte("---\nname: team-tool\n---\n"))
		w, _ = zw.Create("team-tool/../evil.md")
		_, _ = w.Write([]byte("y"))
		_ = zw.Close()
		return buf.Bytes()
	}
	// A symlink entry.
	symlink := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("team-tool/SKILL.md")
		_, _ = w.Write([]byte("---\nname: team-tool\n---\n"))
		hdr := &zip.FileHeader{Name: "team-tool/link", Method: zip.Deflate}
		hdr.SetMode(fs.ModeSymlink | 0o755)
		lw, _ := zw.CreateHeader(hdr)
		_, _ = lw.Write([]byte("SKILL.md")) // link target payload
		_ = zw.Close()
		return buf.Bytes()
	}
	// A file at the zip root (no top-level directory).
	bareRoot := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("SKILL.md")
		_, _ = w.Write([]byte("---\nname: team-tool\n---\n"))
		_ = zw.Close()
		return buf.Bytes()
	}
	// Bad directory name (uppercase).
	badName := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("Team_Tool/SKILL.md")
		_, _ = w.Write([]byte("---\nname: Team_Tool\n---\n"))
		_ = zw.Close()
		return buf.Bytes()
	}

	cases := []struct {
		name string
		zip  []byte
	}{
		{"missing SKILL.md", noSkillMD()},
		{"frontmatter name mismatch", nameMismatch()},
		{"no frontmatter", noFrontmatter()},
		{"two top-level dirs", twoRoots()},
		{"zip-slip dotdot", slip()},
		{"symlink entry", symlink()},
		{"bare root file", bareRoot()},
		{"bad dir name uppercase", badName()},
	}
	for _, tc := range cases {
		if rec := postSkill(t, h, skAdmin, "team", "market-team", tc.zip); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, rec.Code, rec.Body.String())
		}
	}

	// scope=team without the team field.
	if rec := postSkill(t, h, skAdmin, "team", "", skillZip("team-tool", nil)); rec.Code != http.StatusBadRequest {
		t.Errorf("missing team: status = %d, want 400", rec.Code)
	}
	// Unknown scope.
	if rec := postSkill(t, h, skAdmin, "bogus", "", skillZip("team-tool", nil)); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown scope: status = %d, want 400", rec.Code)
	}
}

// TestExtractSkillZip_StandardDirectoryEntriesAccepted is the maintainer's
// reproduction: standard archive tools (zip -r, Python zipfile) emit
// directory entries ("my-skill/", "my-skill/scripts/") with a trailing
// slash. Before the fix the trailing slash produced an empty final path
// component that was rejected as an unsafe zip entry, so every standard
// archive was rejected.
func TestExtractSkillZip_StandardDirectoryEntriesAccepted(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, dir := range []string{"my-skill/", "my-skill/scripts/"} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: dir, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte{})
	}
	w, _ := zw.Create("my-skill/SKILL.md")
	_, _ = w.Write([]byte("---\nname: my-skill\n---\n"))
	w, _ = zw.Create("my-skill/scripts/run.sh")
	_, _ = w.Write([]byte("echo hi\n"))
	_ = zw.Close()

	name, files, err := extractSkillZip(buf.Bytes())
	if err != nil {
		t.Fatalf("standard archive with directory entries must be accepted, got: %v", err)
	}
	if name != "my-skill" {
		t.Fatalf("name = %q, want my-skill", name)
	}
	if len(files) != 2 {
		t.Fatalf("files = %v, want SKILL.md + scripts/run.sh", files)
	}
	if _, ok := files["SKILL.md"]; !ok {
		t.Errorf("missing SKILL.md in %v", files)
	}
	if _, ok := files["scripts/run.sh"]; !ok {
		t.Errorf("missing scripts/run.sh in %v", files)
	}
}

// TestExtractSkillZip_TraversalStillRejectedWithDirEntries: accepting
// directory entries must not loosen the traversal validation.
func TestExtractSkillZip_TraversalStillRejectedWithDirEntries(t *testing.T) {
	cases := map[string][]string{
		"dotdot dir":  {"my-skill/", "../evil.sh"},
		"dotdot deep": {"my-skill/", "my-skill/../../evil"},
		"empty mid":   {"my-skill/", "my-skill//evil"},
		"second top":  {"my-skill/", "other/SKILL.md"},
	}
	for tc, entries := range cases {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, e := range entries {
			w, _ := zw.Create(e)
			_, _ = w.Write([]byte("x"))
		}
		_ = zw.Close()
		if _, _, err := extractSkillZip(buf.Bytes()); err == nil {
			t.Errorf("%s: want error", tc)
		}
	}
}

func TestSkills_UploadZipBombRejected(t *testing.T) {
	h, _, _ := newSkillsRig(t, passScanner)
	// A tiny zip that decompresses past the 64 MB limit.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("team-tool/SKILL.md")
	_, _ = w.Write(bytes.Repeat([]byte("a"), maxSkillUploadBytes+1))
	_ = zw.Close()
	if rec := postSkill(t, h, skAdmin, "team", "market-team", buf.Bytes()); rec.Code != http.StatusBadRequest {
		t.Errorf("zip bomb: status = %d, want 400", rec.Code)
	}
}

func TestSkills_UploadScanGate(t *testing.T) {
	hBlock, storeBlock, _ := newSkillsRig(t, verdictScanner{verdict: skillscan.SkillScanVerdict{
		Status:   "block",
		Findings: []skillscan.SkillUploadFinding{{RuleID: "malicious.exec", Severity: "CRITICAL", File: "scripts/run.sh", Title: "exec call"}},
	}})
	hFail, storeFail, _ := newSkillsRig(t, verdictScanner{err: errors.New("exec timeout")})
	hWarn, _, _ := newSkillsRig(t, verdictScanner{verdict: skillscan.SkillScanVerdict{
		Status:   "warn",
		Findings: []skillscan.SkillUploadFinding{{RuleID: "style.lint", Severity: "MEDIUM", Title: "style"}},
	}})
	zip := skillZip("team-tool", nil)

	// block → 422, nothing written.
	rec := postSkill(t, hBlock, skAdmin, "team", "market-team", zip)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("block: status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "malicious.exec") {
		t.Errorf("block response should name the finding: %s", rec.Body.String())
	}
	if err := storeBlock.Memory.Stat(context.Background(), "teams/market-team/skills/team-tool/SKILL.md"); err == nil {
		t.Error("blocked upload wrote files")
	}

	// unavailable → best-effort: 200 + scan.status="skipped" + files written
	// (the mandatory gate is scan ② at assign time).
	rec = postSkill(t, hFail, skAdmin, "team", "market-team", zip)
	if rec.Code != http.StatusOK {
		t.Fatalf("unavailable: status = %d, want 200 (best-effort): %s", rec.Code, rec.Body.String())
	}
	var resp SkillUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Scan == nil || resp.Scan.Status != "skipped" {
		t.Errorf("unavailable scan = %+v, want skipped", resp.Scan)
	}
	if err := storeFail.Memory.Stat(context.Background(), "teams/market-team/skills/team-tool/SKILL.md"); err != nil {
		t.Errorf("best-effort upload did not write: %v", err)
	}

	// warn → allowed, findings surfaced.
	rec = postSkill(t, hWarn, skAdmin, "team", "market-team", zip)
	if rec.Code != http.StatusOK {
		t.Fatalf("warn: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Scan == nil || resp.Scan.Status != "warn" || len(resp.Scan.Findings) != 1 {
		t.Errorf("warn scan not surfaced: %+v", resp.Scan)
	}

	// nil scanner → 200 + skipped (best-effort, nothing to ask).
	_, _, base := newSkillsRig(t, nil)
	k8s := fake.NewClientBuilder().
		WithScheme(newServerTestScheme(t)).
		WithRuntimeObjects(&v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "market-team", Namespace: "default"}}).
		Build()
	hNil := NewSkillsHandler(filepath.Join(base, "worker-agent"), "", &mcLikeOSS{Memory: ossfake.NewMemory()}, k8s, "default", nil)
	if rec := postSkill(t, hNil, skAdmin, "team", "market-team", zip); rec.Code != http.StatusOK {
		t.Errorf("nil scanner: status = %d, want 200 (best-effort)", rec.Code)
	}
}

func TestSkills_UploadReplaceExactCopy(t *testing.T) {
	h, store, _ := newSkillsRig(t, passScanner)

	if rec := postSkill(t, h, skAdmin, "team", "market-team", skillZip("team-tool", map[string]string{"stale/old.sh": "stale"})); rec.Code != http.StatusOK {
		t.Fatalf("v1: %d %s", rec.Code, rec.Body.String())
	}
	// Re-upload without stale/old.sh: exact-copy semantics delete it.
	if rec := postSkill(t, h, skAdmin, "team", "market-team", skillZip("team-tool", nil)); rec.Code != http.StatusOK {
		t.Fatalf("v2: %d %s", rec.Code, rec.Body.String())
	}
	if err := store.Memory.Stat(context.Background(), "teams/market-team/skills/team-tool/SKILL.md"); err != nil {
		t.Errorf("SKILL.md missing after re-upload: %v", err)
	}
	if err := store.Memory.Stat(context.Background(), "teams/market-team/skills/team-tool/stale/old.sh"); err == nil {
		t.Error("stale file survived the re-upload (Remove not applied)")
	}
}

func TestSkills_TeamScopeListingFailureDegrades(t *testing.T) {
	h, fakeOSS, _ := newSkillsRig(t, nil)
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
	h, _, _ := newSkillsRig(t, nil)
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
	h, _, _ := newSkillsRig(t, nil)
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
	h, _, _ := newSkillsRig(t, nil)
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
	allowed := map[string]bool{"name": true, "description": true, "source": true, "version": true, "requirements": true, "updated_at": true, "agents": true, "plugin": true, "runtimes": true}
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
	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), "", failing, nil, "default", nil)

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
	h := NewSkillsHandler("", "", &mcLikeOSS{Memory: store}, nil, "default", nil)
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
		{"l2-human", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}}},
		{"team-leader", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}},
		{"worker", &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-worker-1"}},
		{"manager", &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newSkillsRig(t, nil)
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
		h, _, _ := newSkillsRig(t, nil)
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

	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), "", &mcLikeOSS{Memory: ossfake.NewMemory()}, nil, "default", nil)
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
	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), "", &mcLikeOSS{Memory: fakeOSS}, nil, "default", nil)
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
	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), "", &mcLikeOSS{Memory: ossfake.NewMemory()}, nil, "default", nil)
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

// --- Plugin source (teamharness skill visibility, #1221 addendum) ---

const testPluginYAML = `apiVersion: agentteams.agentteam/v1alpha1
kind: AgentTeamPlugin
metadata:
  name: teamharness
  version: 0.1.0
skills:
  agent:
    - id: mcporter
      path: skills/agent/mcporter
      roles: [leader, worker, manager, remote-member]
  team:
    - id: communication
      path: skills/team/communication
      roles: [leader, worker, manager, remote-member]
`

// writePluginTree lays out a minimal plugin dir:
//
//	plugins/teamharness/{plugin.yaml, skills/agent/mcporter, skills/team/communication, skills/team/organization}
//	plugins/no-skills/plugin.yaml          (no skills block)
//
// organization/ is on disk but NOT declared in the manifest — it must not
// appear in the catalog (manifest-driven discovery).
func writePluginTree(t *testing.T, pluginsDir string) {
	t.Helper()
	th := filepath.Join(pluginsDir, "teamharness")
	for _, d := range []string{
		filepath.Join(th, "skills", "agent", "mcporter"),
		filepath.Join(th, "skills", "team", "communication"),
		filepath.Join(th, "skills", "team", "organization"),
		filepath.Join(pluginsDir, "no-skills"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(th, "plugin.yaml"), []byte(testPluginYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkillFile := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(th, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// frontmatter name (runtime identity) differs from the manifest id; no
	// version → the plugin package version must be the fallback.
	writeSkillFile("skills/agent/mcporter/SKILL.md",
		"---\nname: teamharness-mcporter\ndescription: Manage mcporter registries.\n---\n")
	// frontmatter with an explicit version wins over the package version.
	writeSkillFile("skills/team/communication/SKILL.md",
		"---\nname: teamharness-communication\ndescription: Message delivery protocol.\nversion: 1.2.0\n---\n")
	// On disk but NOT declared in the manifest — must not appear.
	writeSkillFile("skills/team/organization/SKILL.md",
		"---\nname: teamharness-organization\ndescription: Unlisted.\n---\n")
	if err := os.WriteFile(filepath.Join(pluginsDir, "no-skills", "plugin.yaml"),
		[]byte("metadata:\n  name: no-skills\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsCatalogPluginSource(t *testing.T) {
	base := t.TempDir()
	pluginsDir := filepath.Join(base, "plugins")
	writePluginTree(t, pluginsDir)
	writeSkill(t, filepath.Join(base, "worker-agent", "skills"), "file-sync", "Sync files with centralized storage.")

	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), pluginsDir, &mcLikeOSS{Memory: ossfake.NewMemory()}, nil, "default", nil)
	skills := decodeSkills(t, getSkills(t, h))

	// 1 builtin + 2 declared plugin skills (unlisted organization excluded).
	if len(skills) != 3 {
		t.Fatalf("want 3 skills, got %d: %+v", len(skills), skills)
	}
	mc := skillByName(t, skills, "teamharness-mcporter")
	if mc.Source != "plugin" || mc.Plugin != "teamharness" {
		t.Errorf("mcporter: source=%q plugin=%q, want plugin/teamharness", mc.Source, mc.Plugin)
	}
	if mc.Version != "0.1.0" {
		t.Errorf("mcporter version = %q, want package version fallback 0.1.0", mc.Version)
	}
	if mc.Description != "Manage mcporter registries." {
		t.Errorf("mcporter description = %q", mc.Description)
	}
	comm := skillByName(t, skills, "teamharness-communication")
	if comm.Version != "1.2.0" {
		t.Errorf("communication version = %q, want frontmatter version 1.2.0", comm.Version)
	}
	// No runtimes/agents/updated_at on plugin entries (availability follows
	// the plugin's deployment).
	if len(comm.Runtimes) != 0 || len(comm.Agents) != 0 || comm.UpdatedAt != "" {
		t.Errorf("communication: plugin entry must not carry runtimes/agents/updated_at: %+v", comm)
	}
	for _, s := range skills {
		if s.Name == "teamharness-organization" {
			t.Fatalf("unlisted skill leaked into the catalog: %+v", s)
		}
	}
}

// TestSkillsCatalogPluginSkillMdMissing pins the reviewer repro: a
// manifest-declared skill whose SKILL.md is missing must be omitted —
// neither under the frontmatter name nor under the manifest-ID fallback —
// per the documented "missing SKILL.md → absence" contract.
func TestSkillsCatalogPluginSkillMdMissing(t *testing.T) {
	base := t.TempDir()
	pluginsDir := filepath.Join(base, "plugins")
	writePluginTree(t, pluginsDir)
	// Reviewer repro: remove skills/agent/mcporter/SKILL.md.
	if err := os.Remove(filepath.Join(pluginsDir, "teamharness", "skills", "agent", "mcporter", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), pluginsDir, &mcLikeOSS{Memory: ossfake.NewMemory()}, nil, "default", nil)
	skills := decodeSkills(t, getSkills(t, h))

	// Only the one plugin skill with a real SKILL.md remains
	// (mcporter omitted, unlisted organization excluded).
	if len(skills) != 1 {
		t.Fatalf("want 1 skill, got %d: %+v", len(skills), skills)
	}
	for _, s := range skills {
		if s.Name == "teamharness-mcporter" || s.Name == "mcporter" {
			t.Fatalf("skill with missing SKILL.md leaked into the catalog: %+v", s)
		}
	}
	if comm := skillByName(t, skills, "teamharness-communication"); comm.Source != "plugin" {
		t.Fatalf("communication must survive: %+v", comm)
	}
}

func TestSkillsCatalogPluginCollisionBuiltinWins(t *testing.T) {
	base := t.TempDir()
	pluginsDir := filepath.Join(base, "plugins")
	writePluginTree(t, pluginsDir)
	// A plugin skill whose frontmatter name equals a builtin skill name.
	writeSkill(t, filepath.Join(base, "worker-agent", "skills"), "teamharness-mcporter", "Builtin wins.")
	writeSkill(t, filepath.Join(base, "worker-agent", "skills"), "file-sync", "Sync files with centralized storage.")

	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), pluginsDir, &mcLikeOSS{Memory: ossfake.NewMemory()}, nil, "default", nil)
	skills := decodeSkills(t, getSkills(t, h))

	mc := skillByName(t, skills, "teamharness-mcporter")
	if mc.Source != "builtin" {
		t.Fatalf("collision: builtin must win, got source=%q", mc.Source)
	}
	if len(skills) != 3 {
		t.Fatalf("want 3 skills (1 builtin-collision + 2 plugin), got %d: %+v", len(skills), skills)
	}
}

func TestSkillsCatalogPluginDirMissing(t *testing.T) {
	base := t.TempDir()
	writeSkill(t, filepath.Join(base, "worker-agent", "skills"), "file-sync", "Sync files with centralized storage.")
	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), filepath.Join(base, "no-such-dir"), &mcLikeOSS{Memory: ossfake.NewMemory()}, nil, "default", nil)
	skills := decodeSkills(t, getSkills(t, h))
	if len(skills) != 1 || skills[0].Name != "file-sync" {
		t.Fatalf("missing plugin dir must degrade to builtin-only, got: %+v", skills)
	}
}

func TestSkillsCatalogPluginManifestMalformed(t *testing.T) {
	base := t.TempDir()
	pluginsDir := filepath.Join(base, "plugins")
	th := filepath.Join(pluginsDir, "broken")
	if err := os.MkdirAll(filepath.Join(th, "skills", "agent", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(th, "plugin.yaml"), []byte("skills: [unclosed"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(base, "worker-agent", "skills"), "file-sync", "Sync files with centralized storage.")
	h := NewSkillsHandler(filepath.Join(base, "worker-agent"), pluginsDir, &mcLikeOSS{Memory: ossfake.NewMemory()}, nil, "default", nil)
	skills := decodeSkills(t, getSkills(t, h))
	if len(skills) != 1 || skills[0].Name != "file-sync" {
		t.Fatalf("malformed manifest must degrade to builtin-only, got: %+v", skills)
	}
}

// countingScanner is a fixed-pass scanner with a call counter.
type countingScanner struct{ calls int }

func (c *countingScanner) ScanSkill(_ context.Context, _ string, _ map[string][]byte) (skillscan.SkillScanVerdict, error) {
	c.calls++
	return skillscan.SkillScanVerdict{Status: "pass"}, nil
}

// TestTeamSkillStandardZipToWorkerE2E is the maintainer's requested
// end-to-end case: a standard archive (directory entries + nested
// scripts, the zip -r shape) uploaded through the real handler, then
// assigned to a worker. Materialization follows the production `mc ls`
// listing contract (relative names, non-recursive) so every nested file
// lands under the worker's agent dir, gated by scan ②. The
// fresh-scanner-container half — the Docker archive API 404s an archive
// PUT into a nonexistent scratch dir — is enforced by the skillscan
// embedded tests, whose fake now returns 404 for that case and whose
// probe only writes a verdict when the payload actually landed;
// ensureScratchDir is what makes the first scan on a fresh container
// succeed.
func TestTeamSkillStandardZipToWorkerE2E(t *testing.T) {
	ctx := context.Background()
	// Standard archive shape: directory entries with trailing slashes +
	// a nested script two levels deep.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for _, dir := range []string{"zip-kb/", "zip-kb/scripts/"} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: dir, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte{})
	}
	mustZip := func(name, content string) {
		t.Helper()
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	mustZip("zip-kb/SKILL.md", "---\nname: zip-kb\n---\n")
	mustZip("zip-kb/scripts/run.sh", "echo hi\n")
	mustZip("zip-kb/scripts/lib/helper.py", "x = 1\n")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	// Upload through the real handler into the team layer.
	h, store, _ := newSkillsRig(t, passScanner)
	rec := postSkill(t, h, skAdmin, "team", "market-team", zbuf.Bytes())
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := store.Memory.Stat(ctx, "teams/market-team/skills/zip-kb/scripts/lib/helper.py"); err != nil {
		t.Fatalf("nested file not stored after standard-zip upload: %v", err)
	}

	// Assign: scan ② gates, materialization lists per the production
	// contract and copies every nested file into the worker's agent dir.
	scanner := &countingScanner{}
	deployer := service.NewDeployer(service.DeployerConfig{OSS: store.Memory, SkillScanner: scanner})
	if err := deployer.PushOnDemandSkills(ctx, "alice", "market-team", []string{"zip-kb"}, nil); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if scanner.calls != 1 {
		t.Fatalf("scan ② ran %d times, want 1", scanner.calls)
	}
	for _, key := range []string{
		"agents/alice/skills/zip-kb/SKILL.md",
		"agents/alice/skills/zip-kb/scripts/run.sh",
		"agents/alice/skills/zip-kb/scripts/lib/helper.py",
	} {
		if err := store.Memory.Stat(ctx, key); err != nil {
			t.Errorf("missing %s after materialization: %v", key, err)
		}
	}
}
