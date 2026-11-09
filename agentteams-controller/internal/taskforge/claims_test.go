package taskforge

import (
	"errors"
	"testing"
	"time"
)

// goldenClaims / goldenSecret are the fixed input pair of task 26 §3.3.
var goldenClaims = Claims{
	ActorType:  ActorWorker,
	ActorID:    "w-1",
	OnBehalfOf: "",
	TeamID:     "sysdev",
	RequestID:  "req-1",
	TraceID:    "trace-1",
	TokenJTI:   "jti-1",
	IssuedAt:   1700000000,
}

const goldenSecret = "0123456789abcdef"

// goldenSignature was precomputed by the orchestrator (task 26 §3.3) — it
// is contract, not derived here. A mismatch means either the golden value
// or Canonical/Sign changed; both sides of the contract must be reviewed.
const goldenSignature = "ad393637eab80894b8b093b9f3871c80538c97ba401819378dfc81ad3c380323"

func TestClaimsCanonicalGolden(t *testing.T) {
	want := "v1\nworker\nw-1\n\nsysdev\nreq-1\ntrace-1\njti-1\n1700000000"
	if got := goldenClaims.Canonical(); got != want {
		t.Fatalf("Canonical() = %q, want %q", got, want)
	}
}

func TestClaimsSignGolden(t *testing.T) {
	if got := Sign(goldenSecret, goldenClaims); got != goldenSignature {
		t.Fatalf("Sign() = %q, want %q", got, goldenSignature)
	}
}

func TestClaimsVerifyGolden(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if err := Verify(goldenSecret, goldenClaims, goldenSignature, now); err != nil {
		t.Fatalf("Verify(golden) = %v, want nil", err)
	}
}

func TestClaimsVerifyRejectsTamperedField(t *testing.T) {
	now := time.Unix(1700000000, 0)
	sig := Sign(goldenSecret, goldenClaims) // signature of the ORIGINAL claims
	cases := []struct {
		name   string
		mutate func(*Claims)
	}{
		{"actor type", func(c *Claims) { c.ActorType = ActorHuman }},
		{"actor id", func(c *Claims) { c.ActorID = "w-2" }},
		{"on behalf of", func(c *Claims) { c.OnBehalfOf = "h-1" }},
		{"team id", func(c *Claims) { c.TeamID = "other" }},
		{"request id", func(c *Claims) { c.RequestID = "req-2" }},
		{"trace id", func(c *Claims) { c.TraceID = "trace-2" }},
		{"token jti", func(c *Claims) { c.TokenJTI = "jti-2" }},
		{"issued at", func(c *Claims) { c.IssuedAt = 1700000001 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := goldenClaims
			tc.mutate(&c)
			if err := Verify(goldenSecret, c, sig, now); !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("Verify(tampered %s) = %v, want ErrInvalidSignature", tc.name, err)
			}
		})
	}
}

func TestClaimsVerifyRejectsWrongSecret(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if err := Verify("ffffffffffffffff", goldenClaims, goldenSignature, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("Verify(wrong secret) = %v, want ErrInvalidSignature", err)
	}
}

func TestClaimsVerifyRejectsExpired(t *testing.T) {
	issued := time.Unix(goldenClaims.IssuedAt, 0)
	sig := Sign(goldenSecret, goldenClaims)

	// 61s in either direction is outside the window.
	if err := Verify(goldenSecret, goldenClaims, sig, issued.Add(61*time.Second)); !errors.Is(err, ErrClaimsExpired) {
		t.Fatalf("future skew 61s: %v, want ErrClaimsExpired", err)
	}
	if err := Verify(goldenSecret, goldenClaims, sig, issued.Add(-61*time.Second)); !errors.Is(err, ErrClaimsExpired) {
		t.Fatalf("past skew 61s: %v, want ErrClaimsExpired", err)
	}
	// Exactly 60s is still inside the window.
	if err := Verify(goldenSecret, goldenClaims, sig, issued.Add(60*time.Second)); err != nil {
		t.Fatalf("60s boundary: %v, want nil", err)
	}
}

func TestClaimsCanonicalEmptyFieldsKeepPlaceholders(t *testing.T) {
	// 9 fixed segments: v1, actor_type, then 6 empty segments, issued_at.
	c := Claims{ActorType: ActorSystem, IssuedAt: 0}
	want := "v1\nsystem\n\n\n\n\n\n\n0"
	if got := c.Canonical(); got != want {
		t.Fatalf("Canonical() = %q, want %q", got, want)
	}
}
