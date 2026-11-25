package memory

import (
	"strings"
	"testing"
)

func TestParseIdempotencyKey(t *testing.T) {
	// empty is valid: it means "no key"
	k, err := ParseIdempotencyKey("")
	if err != nil || k.IsSet() {
		t.Fatalf("empty key: %v, %v", k, err)
	}
	k, err = ParseIdempotencyKey("   ")
	if err != nil || k.IsSet() {
		t.Fatalf("blank key: %v, %v", k, err)
	}

	k, err = ParseIdempotencyKey(" req-123 ")
	if err != nil || k != "req-123" {
		t.Fatalf("trimmed key: %q, %v", k, err)
	}

	if _, err := ParseIdempotencyKey(strings.Repeat("x", MaxIdempotencyKeyLen)); err != nil {
		t.Fatalf("max-length key: %v", err)
	}
	if _, err := ParseIdempotencyKey(strings.Repeat("x", MaxIdempotencyKeyLen+1)); err != ErrIdempotencyKeyTooLong {
		t.Fatalf("over-long key: err = %v, want ErrIdempotencyKeyTooLong", err)
	}

	for _, bad := range []string{"a b", "a\tb", "a\nb", "a\rb", "a\x00b"} {
		if _, err := ParseIdempotencyKey(bad); err != ErrIdempotencyKeyBadChars {
			t.Fatalf("bad key %q: err = %v, want ErrIdempotencyKeyBadChars", bad, err)
		}
	}
	// the sentinel's message is static: no key material in it.
	msg := ErrIdempotencyKeyBadChars.Error()
	for _, bad := range []string{"a b", "a\tb", "a\nb", "a\rb", "a\x00b"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("bad-chars error echoes key material: %q", msg)
		}
	}
}

func TestPayloadHash(t *testing.T) {
	base := NewEntry{
		Anchor:      Anchor{Kind: KindTeam, ID: "t1"},
		ContentType: "fact",
		Text:        "hello",
	}
	h1 := PayloadHash(base)
	h2 := PayloadHash(NewEntry{
		Anchor:      base.Anchor,
		ContentType: base.ContentType,
		Text:        base.Text,
		Provenance:  Provenance{Actor: "someone"}, // provenance is NOT part of the hash
	})
	if h1 != h2 {
		t.Fatal("payload hash must ignore provenance")
	}

	if h1 == PayloadHash(NewEntry{Anchor: base.Anchor, ContentType: base.ContentType, Text: "hellO"}) {
		t.Fatal("payload hash must distinguish text")
	}
	if h1 == PayloadHash(NewEntry{Anchor: base.Anchor, ContentType: "note", Text: base.Text}) {
		t.Fatal("payload hash must distinguish content_type")
	}
	if h1 == PayloadHash(NewEntry{
		Anchor:      Anchor{Kind: KindHuman, ID: "t1"},
		ContentType: base.ContentType,
		Text:        base.Text,
	}) {
		t.Fatal("payload hash must distinguish the anchor")
	}
}

func TestParseIfMatch(t *testing.T) {
	for in, want := range map[string]uint64{
		`"3"`:   3,
		`3`:     3,
		`W/"3"`: 3,
		` "4" `: 4,
	} {
		got, ok := ParseIfMatch(in)
		if !ok || got != want {
			t.Fatalf("ParseIfMatch(%q) = %d, %v; want %d, true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "*", `"*"`, "abc", `""`, "-1", "1.5"} {
		if got, ok := ParseIfMatch(in); ok {
			t.Fatalf("ParseIfMatch(%q) = %d, want rejected", in, got)
		}
	}
}
