package service

import (
	"context"
	"strings"
	"testing"
)

// distinguishingInterviewText is a substring that only appears in the normal
// (multi-user) welcome variant's 4-question interview step. Its presence or
// absence is what distinguishes the two render paths in these tests.
const distinguishingInterviewText = "Ask them: a) What would they like to call you?"

// TestRenderManagerWelcomeBody_NormalContainsInterview locks in existing
// behavior: the default (non-solo) welcome body still asks the 4-question
// identity interview, unchanged.
func TestRenderManagerWelcomeBody_NormalContainsInterview(t *testing.T) {
	body := renderManagerWelcomeBody("en", "America/New_York")

	if !strings.Contains(body, distinguishingInterviewText) {
		t.Fatalf("normal welcome body missing 4-question interview text; body=%q", body)
	}
	if !strings.Contains(body, "en") || !strings.Contains(body, "America/New_York") {
		t.Fatalf("normal welcome body missing language/timezone substitution; body=%q", body)
	}
}

// TestRenderManagerWelcomeBodySolo_NoInterview verifies the solo-mode
// variant omits the 4-question interview entirely and instead contains
// solo-specific guidance text.
func TestRenderManagerWelcomeBodySolo_NoInterview(t *testing.T) {
	body := renderManagerWelcomeBodySolo("en", "America/New_York")

	if strings.Contains(body, distinguishingInterviewText) {
		t.Fatalf("solo welcome body must not contain the 4-question interview text; body=%q", body)
	}
	if !strings.Contains(body, "solo mode") {
		t.Fatalf("solo welcome body missing solo-mode framing; body=%q", body)
	}
	if !strings.Contains(body, "without conducting an interview") {
		t.Fatalf("solo welcome body missing explicit no-interview instruction; body=%q", body)
	}
	if !strings.Contains(body, "en") || !strings.Contains(body, "America/New_York") {
		t.Fatalf("solo welcome body missing language/timezone substitution; body=%q", body)
	}
}

// TestSendManagerWelcomeMessage_SoloOperatorSelectsSoloVariant verifies the
// dispatch in SendManagerWelcomeMessage picks the solo variant only when
// ManagerWelcomeRequest.SoloOperator is true.
func TestSendManagerWelcomeMessage_SoloOperatorSelectsSoloVariant(t *testing.T) {
	fake := newFakeTeamMatrix()
	p := &Provisioner{matrix: fake}
	ctx := context.Background()

	if err := p.SendManagerWelcomeMessage(ctx, ManagerWelcomeRequest{
		RoomID:       "!room:test",
		Language:     "en",
		Timezone:     "America/New_York",
		SoloOperator: true,
	}); err != nil {
		t.Fatalf("SendManagerWelcomeMessage (solo): %v", err)
	}
	if strings.Contains(fake.lastAdminBody, distinguishingInterviewText) {
		t.Fatalf("solo dispatch sent a body containing the 4-question interview; body=%q", fake.lastAdminBody)
	}

	if err := p.SendManagerWelcomeMessage(ctx, ManagerWelcomeRequest{
		RoomID:       "!room:test",
		Language:     "en",
		Timezone:     "America/New_York",
		SoloOperator: false,
	}); err != nil {
		t.Fatalf("SendManagerWelcomeMessage (normal): %v", err)
	}
	if !strings.Contains(fake.lastAdminBody, distinguishingInterviewText) {
		t.Fatalf("normal dispatch expected the 4-question interview text; body=%q", fake.lastAdminBody)
	}
}
