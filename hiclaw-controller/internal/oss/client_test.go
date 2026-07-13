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
	if len(policy.Statement) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(policy.Statement))
	}

	// Verify team prefix is included in list conditions
	listStmt := policy.Statement[0]
	condition := listStmt.Condition["StringLike"].(map[string]interface{})
	prefixes := condition["s3:prefix"].([]string)
	hasTeam := false
	hasWorkerDir := false
	hasSharedDir := false
	hasTeamDir := false
	for _, p := range prefixes {
		if p == "teams/team-dev" || p == "teams/team-dev/*" {
			hasTeam = true
		}
		if p == "agents/worker-1/" {
			hasWorkerDir = true
		}
		if p == "shared/" {
			hasSharedDir = true
		}
		if p == "teams/team-dev/" {
			hasTeamDir = true
		}
	}
	if !hasTeam {
		t.Errorf("expected team prefix in list conditions: %v", prefixes)
	}
	if !hasWorkerDir {
		t.Errorf("expected worker directory prefix in list conditions: %v", prefixes)
	}
	if !hasSharedDir {
		t.Errorf("expected shared directory prefix in list conditions: %v", prefixes)
	}
	if !hasTeamDir {
		t.Errorf("expected team directory prefix in list conditions: %v", prefixes)
	}

	// Verify team resource in RW statement
	rwStmt := policy.Statement[1]
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

	rwStmt := policy.Statement[1]
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

	if len(policy.Statement) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(policy.Statement))
	}

	// Verify manager prefix in list conditions
	listStmt := policy.Statement[0]
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
	rwStmt := policy.Statement[1]
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

func TestNewMinIOClient_Defaults(t *testing.T) {
	c := NewMinIOClient(Config{})
	if c.config.MCBinary != "mc" {
		t.Errorf("MCBinary = %q, want mc", c.config.MCBinary)
	}
	if c.config.Alias != "hiclaw" {
		t.Errorf("Alias = %q, want hiclaw", c.config.Alias)
	}
}
