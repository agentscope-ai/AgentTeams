package taskforge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ActorType identifies the kind of principal a claim is issued for. The
// values are fixed by the TaskForge API contract (design 21):
// "human" | "worker" | "manager" | "leader" | "system".
type ActorType string

const (
	ActorHuman   ActorType = "human"
	ActorWorker  ActorType = "worker"
	ActorManager ActorType = "manager"
	ActorLeader  ActorType = "leader"
	ActorSystem  ActorType = "system"
)

// canonicalVersion is the first version of the canonical serialization.
const canonicalVersion = "v1"

// claimsWindow is the freshness window enforced by Verify: a claim whose
// IssuedAt differs from now by more than this (either direction) is
// rejected.
const claimsWindow = 60 * time.Second

// ErrClaimsExpired is returned by Verify when the claim falls outside the
// freshness window.
var ErrClaimsExpired = errors.New("taskforge: claims expired")

// ErrInvalidSignature is returned by Verify when the signature does not
// match.
var ErrInvalidSignature = errors.New("taskforge: invalid claims signature")

// Claims is the identity/trace envelope the controller signs onto every
// request to the TaskForge runner (design 21 §claims). The controller is
// the only party that may issue claims; TaskForge verifies them with the
// shared secret before trusting any of the X-Atf-* headers.
type Claims struct {
	ActorType  ActorType
	ActorID    string
	OnBehalfOf string // empty when absent
	TeamID     string
	RequestID  string
	TraceID    string
	TokenJTI   string
	IssuedAt   int64 // unix seconds
}

// Canonical returns the deterministic signing string:
//
//	v1\n<actor_type>\n<actor_id>\n<on_behalf_of>\n<team_id>\n<request_id>\n<trace_id>\n<token_jti>\n<issued_at>
//
// Fixed 9 segments: an empty field still occupies its segment (it is
// serialized as the empty string), so shifting one field changes the
// whole string.
func (c Claims) Canonical() string {
	return canonicalVersion + "\n" +
		string(c.ActorType) + "\n" +
		c.ActorID + "\n" +
		c.OnBehalfOf + "\n" +
		c.TeamID + "\n" +
		c.RequestID + "\n" +
		c.TraceID + "\n" +
		c.TokenJTI + "\n" +
		strconv.FormatInt(c.IssuedAt, 10)
}

// Sign returns hex(HMAC-SHA256(secret, Canonical())).
func Sign(secret string, c Claims) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(c.Canonical()))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks the signature with a constant-time compare (hmac.Equal,
// never string ==), then rejects claims whose IssuedAt is outside the
// ±60s freshness window.
func Verify(secret string, c Claims, sig string, now time.Time) error {
	expected := Sign(secret, c)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("%w: signature mismatch", ErrInvalidSignature)
	}
	issued := time.Unix(c.IssuedAt, 0)
	delta := now.Sub(issued)
	if delta < 0 {
		delta = -delta
	}
	if delta > claimsWindow {
		return ErrClaimsExpired
	}
	return nil
}
