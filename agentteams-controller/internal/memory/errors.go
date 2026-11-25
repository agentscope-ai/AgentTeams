package memory

import (
	"errors"
	"fmt"
)

// ErrConflictType is the sentinel that every conflict error wraps; use
// IsConflict to test for one.
var ErrConflictType = errors.New("memory: revision conflict")

// ErrNotFound is returned when the entry does not exist in the scope —
// unknown id, or forgotten (forget is a logical delete).
var ErrNotFound = errors.New("memory: entry not found")

// ErrForbidden is returned when the caller lacks access to the scope.
// In W0a the store never returns it; the W1 gateway does.
var ErrForbidden = errors.New("memory: access to this scope is forbidden")

// ErrConflict is a CAS failure: the write did not happen, Current
// holds the entry's actual state so the caller can rebase onto it.
//
// Error() never renders Current's text or any request material — only
// the static message plus the revision number — so an error string
// can not leak credentials or content.
type ErrConflict struct {
	Current Entry
}

func (e *ErrConflict) Error() string {
	return fmt.Sprintf("%s: current revision %d", ErrConflictType.Error(), e.Current.Revision)
}

// Unwrap exposes ErrConflictType for errors.Is.
func (e *ErrConflict) Unwrap() error { return ErrConflictType }

// IsConflict reports whether err is a *ErrConflict.
func IsConflict(err error) bool {
	var c *ErrConflict
	return errors.As(err, &c)
}

// ConflictOf extracts the *ErrConflict from err, or nil.
func ConflictOf(err error) *ErrConflict {
	var c *ErrConflict
	if errors.As(err, &c) {
		return c
	}
	return nil
}
