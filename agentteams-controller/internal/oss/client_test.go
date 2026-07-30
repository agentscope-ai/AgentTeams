package oss

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMinIOClient_FullPath(t *testing.T) {
	c := NewMinIOClient(Config{
		StoragePrefix: "agentteams/agentteams-storage",
	})

	got := c.fullPath("agents/worker-1/openclaw.json")
	want := "agentteams/agentteams-storage/agents/worker-1/openclaw.json"
	if got != want {
		t.Errorf("fullPath = %q, want %q", got, want)
	}
}

func TestMinIOClient_FullPathNoLeadingSlash(t *testing.T) {
	c := NewMinIOClient(Config{
		StoragePrefix: "agentteams/agentteams-storage",
	})

	got := c.fullPath("/agents/worker-1/file.txt")
	want := "agentteams/agentteams-storage/agents/worker-1/file.txt"
	if got != want {
		t.Errorf("fullPath with leading slash = %q, want %q", got, want)
	}
}

func TestMinIOClient_PutObjectUsesCp(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	mcPath := filepath.Join(dir, "mc")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$MC_ARGS_FILE\"\n"
	if err := os.WriteFile(mcPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MC_ARGS_FILE", argsPath)

	c := NewMinIOClient(Config{
		MCBinary:      mcPath,
		StoragePrefix: "agentteams/agentteams-storage",
	})
	if err := c.PutObject(t.Context(), "agents/worker-1/.agentteams-keep", []byte("")); err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(args); !strings.HasPrefix(got, "cp ") ||
		!strings.HasSuffix(got, " agentteams/agentteams-storage/agents/worker-1/.agentteams-keep\n") {
		t.Fatalf("mc args = %q, want cp <tmp> agentteams/agentteams-storage/agents/worker-1/.agentteams-keep", args)
	}
}

func TestMinIOAdminClient_BuildWorkerPolicy(t *testing.T) {
	c := NewMinIOAdminClient(Config{Bucket: "agentteams-storage"})

	policy := c.buildWorkerPolicy("worker-1", "agentteams-storage", "team-dev", false)

	if policy.Version != "2012-10-17" {
		t.Errorf("Version = %q", policy.Version)
	}
	if len(policy.Statement) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(policy.Statement))
	}

	locationStmt := policy.Statement[0]
	if !stringSliceContains(locationStmt.Action, "s3:GetBucketLocation") {
		t.Errorf("expected bucket location action in bucket statement: %v", locationStmt.Action)
	}
	if len(locationStmt.Condition) != 0 {
		t.Errorf("bucket location statement must not have prefix condition: %v", locationStmt.Condition)
	}

	// Verify team prefix is included in list conditions
	listStmt := policy.Statement[1]
	if !stringSliceContains(listStmt.Action, "s3:ListBucket") {
		t.Errorf("expected list action in list statement: %v", listStmt.Action)
	}
	condition := listStmt.Condition["StringLike"].(map[string]interface{})
	prefixes := condition["s3:prefix"].([]string)
	hasAgentsParent := false
	hasTeam := false
	hasWorkerDir := false
	hasWorkerConfig := false
	hasSharedDir := false
	hasTeamsParent := false
	hasTeamDir := false
	for _, p := range prefixes {
		if p == "agents/" {
			hasAgentsParent = true
		}
		if p == "teams/team-dev" || p == "teams/team-dev/*" {
			hasTeam = true
		}
		if p == "agents/worker-1/" {
			hasWorkerDir = true
		}
		if p == "agents/worker-1/openclaw.json" {
			hasWorkerConfig = true
		}
		if p == "shared/" {
			hasSharedDir = true
		}
		if p == "teams/" {
			hasTeamsParent = true
		}
		if p == "teams/team-dev/" {
			hasTeamDir = true
		}
	}
	if !hasAgentsParent {
		t.Errorf("expected agents parent prefix in list conditions: %v", prefixes)
	}
	if !hasTeam {
		t.Errorf("expected team prefix in list conditions: %v", prefixes)
	}
	if !hasWorkerDir {
		t.Errorf("expected worker directory prefix in list conditions: %v", prefixes)
	}
	if !hasWorkerConfig {
		t.Errorf("expected worker config prefix in list conditions: %v", prefixes)
	}
	if !hasSharedDir {
		t.Errorf("expected shared directory prefix in list conditions: %v", prefixes)
	}
	if !hasTeamsParent {
		t.Errorf("expected teams parent prefix in list conditions: %v", prefixes)
	}
	if !hasTeamDir {
		t.Errorf("expected team directory prefix in list conditions: %v", prefixes)
	}

	// Verify team resource in RW statement
	rwStmt := policy.Statement[2]
	hasTeamResource := false
	hasTeamExactResource := false
	hasWorkerDirResource := false
	hasWorkerExactResource := false
	hasSharedDirResource := false
	hasSharedExactResource := false
	hasTeamDirResource := false
	for _, r := range rwStmt.Resource {
		if r == "arn:aws:s3:::agentteams-storage/teams/team-dev/*" {
			hasTeamResource = true
		}
		if r == "arn:aws:s3:::agentteams-storage/teams/team-dev" {
			hasTeamExactResource = true
		}
		if r == "arn:aws:s3:::agentteams-storage/agents/worker-1" {
			hasWorkerExactResource = true
		}
		if r == "arn:aws:s3:::agentteams-storage/agents/worker-1/" {
			hasWorkerDirResource = true
		}
		if r == "arn:aws:s3:::agentteams-storage/shared" {
			hasSharedExactResource = true
		}
		if r == "arn:aws:s3:::agentteams-storage/shared/" {
			hasSharedDirResource = true
		}
		if r == "arn:aws:s3:::agentteams-storage/teams/team-dev/" {
			hasTeamDirResource = true
		}
	}
	if !hasTeamResource {
		t.Errorf("expected team resource in RW statement: %v", rwStmt.Resource)
	}
	if !hasTeamExactResource {
		t.Errorf("expected exact team resource in RW statement: %v", rwStmt.Resource)
	}
	if !hasWorkerExactResource {
		t.Errorf("expected exact worker resource in RW statement: %v", rwStmt.Resource)
	}
	if !hasWorkerDirResource {
		t.Errorf("expected worker directory resource in RW statement: %v", rwStmt.Resource)
	}
	if !hasSharedExactResource {
		t.Errorf("expected exact shared resource in RW statement: %v", rwStmt.Resource)
	}
	if !hasSharedDirResource {
		t.Errorf("expected shared directory resource in RW statement: %v", rwStmt.Resource)
	}
	if !hasTeamDirResource {
		t.Errorf("expected team directory resource in RW statement: %v", rwStmt.Resource)
	}
}

func TestMinIOAdminClient_BuildWorkerPolicyNoTeam(t *testing.T) {
	c := NewMinIOAdminClient(Config{Bucket: "agentteams-storage"})

	policy := c.buildWorkerPolicy("worker-solo", "agentteams-storage", "", false)

	rwStmt := policy.Statement[2]
	for _, r := range rwStmt.Resource {
		if r == "arn:aws:s3:::agentteams-storage/teams/*" {
			t.Error("solo worker should not have team resource")
		}
		if r == "arn:aws:s3:::agentteams-storage/manager/*" {
			t.Error("non-manager worker should not have manager resource")
		}
	}
}

func TestMinIOAdminClient_BuildManagerPolicy(t *testing.T) {
	c := NewMinIOAdminClient(Config{Bucket: "agentteams-storage"})

	policy := c.buildWorkerPolicy("default", "agentteams-storage", "", true)

	if len(policy.Statement) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(policy.Statement))
	}

	// Verify manager prefix in list conditions
	listStmt := policy.Statement[1]
	condition := listStmt.Condition["StringLike"].(map[string]interface{})
	prefixes := condition["s3:prefix"].([]string)
	hasManager := false
	hasManagerDir := false
	for _, p := range prefixes {
		if p == "manager" || p == "manager/*" {
			hasManager = true
		}
		if p == "manager/" {
			hasManagerDir = true
		}
	}
	if !hasManager {
		t.Errorf("expected manager prefix in list conditions: %v", prefixes)
	}
	if !hasManagerDir {
		t.Errorf("expected manager directory prefix in list conditions: %v", prefixes)
	}

	// Verify manager resource in RW statement
	rwStmt := policy.Statement[2]
	hasManagerResource := false
	hasManagerDirResource := false
	for _, r := range rwStmt.Resource {
		if r == "arn:aws:s3:::agentteams-storage/manager/*" {
			hasManagerResource = true
		}
		if r == "arn:aws:s3:::agentteams-storage/manager/" {
			hasManagerDirResource = true
		}
	}
	if !hasManagerResource {
		t.Errorf("expected manager resource in RW statement: %v", rwStmt.Resource)
	}
	if !hasManagerDirResource {
		t.Errorf("expected manager directory resource in RW statement: %v", rwStmt.Resource)
	}
}

func TestMinIOAdminClient_EnsurePolicyDetachesBeforeReplace(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	mcPath := filepath.Join(dir, "mc")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$MC_ARGS_FILE"
case "$*" in
  "admin policy detach "*|"admin policy remove "*) exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(mcPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MC_ARGS_FILE", argsPath)

	c := NewMinIOAdminClient(Config{
		MCBinary: mcPath,
		Bucket:   "agentteams-storage",
	})
	if err := c.EnsurePolicy(t.Context(), PolicyRequest{WorkerName: "worker-1"}); err != nil {
		t.Fatalf("EnsurePolicy failed: %v", err)
	}

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("mc calls = %v, want detach/remove/create/attach", lines)
	}
	wantPrefixes := []string{
		"admin policy detach agentteams worker-worker-1 --user worker-1",
		"admin policy remove agentteams worker-worker-1",
		"admin policy create agentteams worker-worker-1 ",
		"admin policy attach agentteams worker-worker-1 --user worker-1",
	}
	for i, want := range wantPrefixes {
		if !strings.HasPrefix(lines[i], want) {
			t.Fatalf("mc call %d = %q, want prefix %q", i, lines[i], want)
		}
	}
}

func TestNewMinIOClient_Defaults(t *testing.T) {
	c := NewMinIOClient(Config{})
	if c.config.MCBinary != "mc" {
		t.Errorf("MCBinary = %q, want mc", c.config.MCBinary)
	}
	if c.config.Alias != "agentteams" {
		t.Errorf("Alias = %q, want agentteams", c.config.Alias)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
