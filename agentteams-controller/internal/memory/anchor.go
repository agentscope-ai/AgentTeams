// Package memory defines the platform memory contract for #1412:
// anchors, entries, the Store interface, domain errors and
// idempotency.
//
// The package is a behaviour contract, not a service: backends
// (internal/memory/sqlite, the PowerContext adapters in
// internal/memory/pc) and the HTTP layer (internal/memory/memoryapi)
// are all tested against the contract suite in
// internal/memory/contract.
package memory

import (
	"encoding/json"
	"errors"
	"strings"
)

// Kind is a memory anchor kind. Only these three kinds exist; there is
// no global memory scope, and "shared" collaboration material is not a
// kind.
type Kind string

const (
	KindHuman Kind = "human"
	KindTeam  Kind = "team"
	KindAgent Kind = "agent"
)

// Anchor is the stable key of a memory scope: kind plus stable id.
//
// Anchors are keyed by stable identity (human id, team id, agent id),
// never by display name or instance name: renaming never moves memory.
// Name-to-id resolution happens in the gateway (W1), before an anchor
// is used.
type Anchor struct {
	Kind Kind
	ID   string
}

// ErrInvalidAnchor is returned by ParseAnchor for malformed anchors
// (unknown kind, missing id, wrong shape). The message is static: it
// never echoes the input.
var ErrInvalidAnchor = errors.New("memory: invalid anchor: expected <kind>:<id> with kind one of human, team, agent")

// ParseAnchor parses "human:alice" into an Anchor. The kind must be one
// of the three known kinds (lowercase). The id must be non-empty and is
// taken verbatim after the first colon (it may itself contain ':', e.g.
// a Matrix user id).
func ParseAnchor(s string) (Anchor, error) {
	kind, id, ok := strings.Cut(s, ":")
	if !ok || id == "" || !Kind(kind).valid() {
		return Anchor{}, ErrInvalidAnchor
	}
	return Anchor{Kind: Kind(kind), ID: id}, nil
}

// Valid reports whether the anchor has a known kind and a non-empty id.
func (a Anchor) Valid() bool { return a.ID != "" && a.Kind.valid() }

func (k Kind) valid() bool {
	switch k {
	case KindHuman, KindTeam, KindAgent:
		return true
	}
	return false
}

// String renders the canonical "kind:id" form.
func (a Anchor) String() string { return string(a.Kind) + ":" + a.ID }

// MarshalJSON renders an anchor as its canonical string form.
func (a Anchor) MarshalJSON() ([]byte, error) {
	if !a.Valid() {
		return nil, ErrInvalidAnchor
	}
	return json.Marshal(a.String())
}

// UnmarshalJSON parses the canonical "kind:id" string form.
func (a *Anchor) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return ErrInvalidAnchor
	}
	anchor, err := ParseAnchor(s)
	if err != nil {
		return ErrInvalidAnchor
	}
	*a = anchor
	return nil
}
