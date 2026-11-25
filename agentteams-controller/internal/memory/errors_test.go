package memory

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrConflict(t *testing.T) {
	c := &ErrConflict{Current: Entry{
		ID:       "e1",
		Text:     "token=super-secret",
		Revision: 7,
	}}

	if !IsConflict(c) {
		t.Fatal("IsConflict(ErrConflict) = false, want true")
	}
	if got := ConflictOf(c); got == nil || got.Current.Revision != 7 {
		t.Fatalf("ConflictOf = %+v, want the same conflict", got)
	}

	// unwraps to the sentinel
	if !errors.Is(c, ErrConflictType) {
		t.Fatal("errors.Is(conflict, ErrConflictType) = false, want true")
	}
	// but is not confused with the other sentinels
	if errors.Is(c, ErrNotFound) || errors.Is(c, ErrForbidden) {
		t.Fatal("conflict must not match the other sentinels")
	}

	// wrapped conflicts are still found
	wrapped := fmt.Errorf("op failed: %w", c)
	if !IsConflict(wrapped) || ConflictOf(wrapped) == nil {
		t.Fatal("conflict detection lost through wrapping")
	}
	if !errors.Is(wrapped, ErrConflictType) {
		t.Fatal("sentinel not reachable through wrapping")
	}

	// the error string carries the revision but never the text
	msg := c.Error()
	if !strings.Contains(msg, "7") {
		t.Fatalf("error message %q must name the current revision", msg)
	}
	if strings.Contains(msg, "super-secret") {
		t.Fatalf("error message leaks entry text: %q", msg)
	}
	if strings.Contains(ErrNotFound.Error(), "e1") || strings.Contains(ErrForbidden.Error(), "e1") {
		t.Fatal("sentinel messages must be static")
	}
}
