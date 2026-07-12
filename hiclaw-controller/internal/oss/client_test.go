package oss

import (
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
	for _, p := range prefixes {
		if p == "teams/team-dev" || p == "teams/team-dev/*" {
			hasTeam = true
			break
		}
	}
	if !hasTeam {
		t.Errorf("expected team prefix in list conditions: %v", prefixes)
	}

	// Verify team resource in RW statement
	rwStmt := policy.Statement[1]
	hasTeamResource := false
	for _, r := range rwStmt.Resource {
		if r == "arn:aws:s3:::agentteams-storage/teams/team-dev/*" {
			hasTeamResource = true
			break
		}
	}
	if !hasTeamResource {
		t.Errorf("expected team resource in RW statement: %v", rwStmt.Resource)
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
	for _, p := range prefixes {
		if p == "manager" || p == "manager/*" {
			hasManager = true
			break
		}
	}
	if !hasManager {
		t.Errorf("expected manager prefix in list conditions: %v", prefixes)
	}

	// Verify manager resource in RW statement
	rwStmt := policy.Statement[1]
	hasManagerResource := false
	for _, r := range rwStmt.Resource {
		if r == "arn:aws:s3:::agentteams-storage/manager/*" {
			hasManagerResource = true
			break
		}
	}
	if !hasManagerResource {
		t.Errorf("expected manager resource in RW statement: %v", rwStmt.Resource)
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
