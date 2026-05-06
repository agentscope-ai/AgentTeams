package controller

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

// captureLogger returns a logr.Logger whose Info output is written to buf as
// plain text. funcr is sufficient for testing call presence and key/value
// ordering.
func captureLogger(buf *bytes.Buffer) logr.Logger {
	return funcr.New(func(prefix, args string) {
		if prefix != "" {
			buf.WriteString(prefix)
			buf.WriteByte(' ')
		}
		buf.WriteString(args)
		buf.WriteByte('\n')
	}, funcr.Options{})
}

func TestMergeUserEnv_EmptyUserIsNoOp(t *testing.T) {
	sys := map[string]string{"HICLAW_MATRIX_URL": "http://m"}
	original := map[string]string{"HICLAW_MATRIX_URL": "http://m"}

	buf := &bytes.Buffer{}
	mergeUserEnv(sys, nil, captureLogger(buf), "worker/w1")
	mergeUserEnv(sys, map[string]string{}, captureLogger(buf), "worker/w1")

	if !reflect.DeepEqual(sys, original) {
		t.Errorf("sys mutated unexpectedly: got %v, want %v", sys, original)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output, got %q", buf.String())
	}
}

func TestMergeUserEnv_NonConflictingMerge(t *testing.T) {
	sys := map[string]string{"HICLAW_MATRIX_URL": "http://m"}
	user := map[string]string{"FOO": "bar", "EMPTY": ""}

	buf := &bytes.Buffer{}
	mergeUserEnv(sys, user, captureLogger(buf), "worker/w1")

	want := map[string]string{
		"HICLAW_MATRIX_URL": "http://m",
		"FOO":               "bar",
		"EMPTY":             "",
	}
	if !reflect.DeepEqual(sys, want) {
		t.Errorf("merged sys = %v, want %v", sys, want)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output for non-conflict merge, got %q", buf.String())
	}
}

func TestMergeUserEnv_ConflictKeepsSystemValue(t *testing.T) {
	sys := map[string]string{"HICLAW_MATRIX_URL": "http://sys"}
	user := map[string]string{"HICLAW_MATRIX_URL": "http://user", "FOO": "bar"}

	buf := &bytes.Buffer{}
	mergeUserEnv(sys, user, captureLogger(buf), "worker/w1")

	if got := sys["HICLAW_MATRIX_URL"]; got != "http://sys" {
		t.Errorf("HICLAW_MATRIX_URL = %q, want system value http://sys", got)
	}
	if got := sys["FOO"]; got != "bar" {
		t.Errorf("FOO = %q, want bar", got)
	}
	if buf.Len() == 0 {
		t.Errorf("expected warning log for ignored key, got none")
	}
}

func TestMergeUserEnv_SortedLog(t *testing.T) {
	sys := map[string]string{"A": "1", "B": "2", "C": "3"}
	user := map[string]string{"C": "x", "A": "x", "B": "x"}

	buf := &bytes.Buffer{}
	mergeUserEnv(sys, user, captureLogger(buf), "worker/w1")

	// funcr prints the keys slice; its string form must have A before B before C.
	out := buf.String()
	aIdx := bytes.Index([]byte(out), []byte(`A`))
	bIdx := bytes.Index([]byte(out), []byte(`B`))
	cIdx := bytes.Index([]byte(out), []byte(`C`))
	if aIdx < 0 || bIdx < 0 || cIdx < 0 {
		t.Fatalf("log missing one or more keys: %q", out)
	}
	if !(aIdx < bIdx && bIdx < cIdx) {
		t.Errorf("keys not sorted in log output: A@%d B@%d C@%d, log=%q", aIdx, bIdx, cIdx, out)
	}
}
