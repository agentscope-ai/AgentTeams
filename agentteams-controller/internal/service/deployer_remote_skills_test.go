package service

import (
	"reflect"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

func TestMapRemoteSkillAuthTypeEmptyUsesAutoDetect(t *testing.T) {
	authType, err := mapRemoteSkillAuthType("")
	if err != nil {
		t.Fatalf("mapRemoteSkillAuthType: %v", err)
	}
	if authType != "" {
		t.Fatalf("authType = %q, want empty auto-detect", authType)
	}
}

func TestRemoteSkillSTSResources(t *testing.T) {
	got := remoteSkillSTSResources([]v1beta1.RemoteSkill{
		{Name: "zeta"},
		{Name: "alpha"},
		{Name: "alpha"},
	})
	want := []string{"skill/alpha", "skill/zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resources = %+v, want %+v", got, want)
	}
}
