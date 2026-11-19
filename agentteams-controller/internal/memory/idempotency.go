package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// MaxIdempotencyKeyLen bounds the Idempotency-Key header.
const MaxIdempotencyKeyLen = 128

// ErrIdempotencyKeyTooLong is returned for a key over
// MaxIdempotencyKeyLen.
var ErrIdempotencyKeyTooLong = errors.New("memory: idempotency key too long (max 128 bytes)")

// ErrIdempotencyKeyBadChars is returned for a key containing a
// control character or whitespace. Messages are static: the key is
// never echoed back.
var ErrIdempotencyKeyBadChars = errors.New("memory: idempotency key contains invalid characters")

// IdempotencyKey makes Create replayable: the same key with the same
// payload returns the first created entry; the same key with a
// different payload is a conflict (see Store). Keys are scoped to the
// anchor (NewEntry.ScopedKey): the same header value under a
// different anchor names an independent key, so a cross-anchor
// request can never read or collide with another anchor's
// idempotency record.
type IdempotencyKey string

// IsSet reports whether the key is non-empty.
func (k IdempotencyKey) IsSet() bool { return k != "" }

// ParseIdempotencyKey validates a raw Idempotency-Key header value.
// An empty value is valid and means "no key".
func ParseIdempotencyKey(raw string) (IdempotencyKey, error) {
	k := IdempotencyKey(strings.TrimSpace(raw))
	if k == "" {
		return "", nil
	}
	if len(k) > MaxIdempotencyKeyLen {
		return "", ErrIdempotencyKeyTooLong
	}
	for _, r := range string(k) {
		switch r {
		case '\x00', '\n', '\r', '\t', ' ':
			return "", ErrIdempotencyKeyBadChars
		}
	}
	return k, nil
}

// ScopedKey renders the anchor-bound storage form of the entry's
// IdempotencyKey: "<anchor-string>\x00<key>". Backends must store and
// look up idempotency records under this form (and nothing else), so
// that the key is bound to the anchor by construction: the same
// header value under two anchors names two independent records, and a
// cross-anchor create can never surface another anchor's entry — not
// on replay, not on conflict. An unset key renders as "".
func (in NewEntry) ScopedKey() string {
	if !in.IdempotencyKey.IsSet() {
		return ""
	}
	return in.Anchor.String() + "\x00" + string(in.IdempotencyKey)
}

// PayloadHash fingerprints the idempotent payload of NewEntry:
// anchor + content_type + text (provenance is not part of it —
// replaying a create must not re-attribute the entry).
func PayloadHash(in NewEntry) string {
	sum := sha256.Sum256([]byte(in.Anchor.String() + "\x00" + in.ContentType + "\x00" + in.Text))
	return hex.EncodeToString(sum[:])
}
