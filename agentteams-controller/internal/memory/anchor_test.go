package memory

import (
	"errors"
	"encoding/json"
	"testing"
)

func TestParseAnchor(t *testing.T) {
	valid := map[string]Anchor{
		"human:alice":  {Kind: KindHuman, ID: "alice"},
		"team:t-001":   {Kind: KindTeam, ID: "t-001"},
		"agent:@a:m":   {Kind: KindAgent, ID: "@a:m"},   // id may contain ':'
		"human:a:b:c":  {Kind: KindHuman, ID: "a:b:c"}, // cut at the first colon
	}
	for in, want := range valid {
		got, err := ParseAnchor(in)
		if err != nil {
			t.Fatalf("ParseAnchor(%q) error: %v", in, err)
		}
		if got != want || !got.Valid() {
			t.Fatalf("ParseAnchor(%q) = %+v, want %+v", in, got, want)
		}
		if got.String() != in {
			t.Fatalf("String() = %q, want %q", got.String(), in)
		}
	}

	invalid := []string{
		"robot:x", // unknown kind
		"alice",   // no colon
		"human:",  // empty id
		"",        // empty
		"Team:t1", // kind is lowercase-only
		"human",   // no id
	}
	for _, in := range invalid {
		if got, err := ParseAnchor(in); err != ErrInvalidAnchor {
			t.Fatalf("ParseAnchor(%q) = %+v, want ErrInvalidAnchor", in, got)
		} else if got.Valid() {
			t.Fatalf("ParseAnchor(%q) returned a valid anchor", in)
		}
	}
}

func TestAnchorJSON(t *testing.T) {
	a, err := ParseAnchor("team:t-001")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"team:t-001"` {
		t.Fatalf("marshal = %s, want the canonical string form", b)
	}
	var back Anchor
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != a {
		t.Fatalf("round-trip = %+v, want %+v", back, a)
	}

	// invalid JSON / invalid anchor → ErrInvalidAnchor
	if err := json.Unmarshal([]byte(`"nope"`), &back); err != ErrInvalidAnchor {
		t.Fatalf("unmarshal bad anchor: err = %v, want ErrInvalidAnchor", err)
	}
	if err := json.Unmarshal([]byte(`42`), &back); err != ErrInvalidAnchor {
		t.Fatalf("unmarshal non-string: err = %v, want ErrInvalidAnchor", err)
	}

	// an invalid anchor cannot be marshaled; encoding/json wraps the
	// Marshaler error, so compare with errors.Is.
	if _, err := json.Marshal(Anchor{Kind: KindHuman}); !errors.Is(err, ErrInvalidAnchor) {
		t.Fatalf("marshal invalid anchor: err = %v, want ErrInvalidAnchor", err)
	}
}
