package service

import (
	"strings"
	"testing"
)

func TestRoomAliasLocalpartSanitizesDisplayNames(t *testing.T) {
	if got := roomAliasLocalpart("team", "alpha"); got != "agentteams-team-alpha" {
		t.Fatalf("valid alias changed: %q", got)
	}
	got := roomAliasLocalpart("team", "Verify Team 5")
	if strings.ContainsAny(got, " ABCDEFGHIJKLMNOPQRSTUVWXYZ") || !strings.HasPrefix(got, "agentteams-team-") {
		t.Fatalf("invalid alias %q", got)
	}
	if got != roomAliasLocalpart("team", "Verify Team 5") {
		t.Fatal("alias is not deterministic")
	}
	if got == roomAliasLocalpart("team", "verify-team-5") {
		t.Fatal("sanitized alias collides with a different name")
	}
}
