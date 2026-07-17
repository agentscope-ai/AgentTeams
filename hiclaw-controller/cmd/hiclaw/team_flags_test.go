package main

import (
	"encoding/json"
	"testing"
)

func TestBuildCreateTeamRequestAdvancedFlags(t *testing.T) {
	req, err := buildCreateTeamRequest(createTeamOptions{
		name:       "alpha",
		leaderName: "alpha-lead",
		leaderModel: "claude-sonnet-4-6",
		workers:    "dev,qa",
		teamAdmin:  "alice",
		teamAdminMatrixID: "@alice:matrix.local",
		peerMentions: "false",
		workerModels: "qwen3.6-plus,qwen3.6-plus",
		workerSkills: "github-operations:file-sync,task-progress",
		workerMcpServers: "github:filesystem",
		teamChannelPolicy: `{"groupAllowExtra":["@extra:matrix.local"]}`,
		leaderMcpServers: "github",
		modelProvider: "dashscope",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	admin := got["admin"].(map[string]interface{})
	if admin["name"] != "alice" || admin["matrixUserId"] != "@alice:matrix.local" {
		t.Fatalf("admin: %#v", admin)
	}
	if got["peerMentions"] != false {
		t.Fatalf("peerMentions: %#v", got["peerMentions"])
	}
	if got["modelProvider"] != "dashscope" {
		t.Fatalf("modelProvider: %#v", got["modelProvider"])
	}

	workers := got["workers"].([]interface{})
	if len(workers) != 2 {
		t.Fatalf("workers len=%d", len(workers))
	}
	dev := workers[0].(map[string]interface{})
	if dev["name"] != "dev" || dev["model"] != "qwen3.6-plus" {
		t.Fatalf("dev worker: %#v", dev)
	}
	skills := dev["skills"].([]interface{})
	if len(skills) != 1 || skills[0] != "github-operations" {
		t.Fatalf("dev skills: %#v", dev["skills"])
	}
	mcp := dev["mcpServers"].([]interface{})
	if len(mcp) != 1 || mcp[0].(map[string]interface{})["name"] != "github" {
		t.Fatalf("dev mcp: %#v", dev["mcpServers"])
	}

	leader := got["leader"].(map[string]interface{})
	leaderMcp := leader["mcpServers"].([]interface{})
	if len(leaderMcp) != 1 || leaderMcp[0].(map[string]interface{})["name"] != "github" {
		t.Fatalf("leader mcp: %#v", leader["mcpServers"])
	}
}

func TestBuildCreateTeamRequestWorkerRuntimes(t *testing.T) {
	req, err := buildCreateTeamRequest(createTeamOptions{
		name:           "alpha",
		leaderName:     "alpha-lead",
		workers:        "dev,qa",
		workerRuntimes: "copaw,hermes",
	})
	if err != nil {
		t.Fatal(err)
	}
	workers := req["workers"].([]interface{})
	if len(workers) != 2 {
		t.Fatalf("workers len=%d", len(workers))
	}
	dev := workers[0].(map[string]interface{})
	qa := workers[1].(map[string]interface{})
	if dev["runtime"] != "copaw" || qa["runtime"] != "hermes" {
		t.Fatalf("runtimes: dev=%#v qa=%#v", dev["runtime"], qa["runtime"])
	}
}

func TestBuildCreateTeamRequestWorkerChannelPolicies(t *testing.T) {
	req, err := buildCreateTeamRequest(createTeamOptions{
		name:       "alpha",
		leaderName: "alpha-lead",
		workers:    "dev",
		workerChannelPolicies: `{"groupAllowExtra":["@dev-only:matrix.local"]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	workers := req["workers"].([]interface{})
	dev := workers[0].(map[string]interface{})
	policy := dev["channelPolicy"].(map[string]interface{})
	extra := policy["groupAllowExtra"].([]interface{})
	if len(extra) != 1 || extra[0] != "@dev-only:matrix.local" {
		t.Fatalf("worker policy: %#v", policy)
	}
}
