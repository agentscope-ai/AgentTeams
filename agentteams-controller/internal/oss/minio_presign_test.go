package oss

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePresignMCScript is a POSIX sh stand-in for the mc CLI (#1475 PR-4): every
// invocation is recorded (marker file + args log) and the canned JSON from
// FAKE_MC_SHARE_JSON / FAKE_MC_STAT_JSON is replayed. FAKE_MC_FAIL=1 makes
// the script fail with the S3 object-not-found error.
const fakePresignMCScript = `#!/bin/sh
if [ -n "$FAKE_MC_MARKER" ]; then
  echo fake-mc-called > "$FAKE_MC_MARKER"
fi
if [ -n "$FAKE_MC_ARGS_LOG" ]; then
  echo "$*" >> "$FAKE_MC_ARGS_LOG"
fi
if [ "$FAKE_MC_FAIL" = "1" ]; then
  echo "error: Object does not exist (fake mc)" >&2
  exit 1
fi
case "$1" in
  share)
    cat "$FAKE_MC_SHARE_JSON"
    ;;
  stat)
    cat "$FAKE_MC_STAT_JSON"
    ;;
  *)
    echo "fake mc: unhandled command: $1" >&2
    exit 1
    ;;
esac
`

const fakePresignShareJSON = `{"status":"success","share":"http://fake-oss:9000/agentteams/agentteams-storage/humans/alice/files/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=300&X-Amz-Signature=fake"}`

// writeFakeMC writes the fake mc script and the canned JSON into dir, wires
// the FAKE_MC_* environment variables, and returns the script path.
func writeFakeMC(t *testing.T, dir, shareJSON, statJSON string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "fake-mc")
	if err := os.WriteFile(scriptPath, []byte(fakePresignMCScript), 0o755); err != nil {
		t.Fatalf("write fake mc: %v", err)
	}
	sharePath := filepath.Join(dir, "share.json")
	if err := os.WriteFile(sharePath, []byte(shareJSON), 0o600); err != nil {
		t.Fatalf("write share json: %v", err)
	}
	statPath := filepath.Join(dir, "stat.json")
	if err := os.WriteFile(statPath, []byte(statJSON), 0o600); err != nil {
		t.Fatalf("write stat json: %v", err)
	}
	t.Setenv("FAKE_MC_SHARE_JSON", sharePath)
	t.Setenv("FAKE_MC_STAT_JSON", statPath)
	t.Setenv("FAKE_MC_MARKER", filepath.Join(dir, "mc-marker"))
	t.Setenv("FAKE_MC_ARGS_LOG", filepath.Join(dir, "mc-args.log"))
	return scriptPath
}

// newPresignTestClient builds a MinIOClient around the fake mc. Endpoint is
// empty on purpose: ensureAlias is then a no-op, so the tests isolate the
// `share download` invocation from alias setup.
func newPresignTestClient(t *testing.T, dir, shareJSON string) *MinIOClient {
	t.Helper()
	return NewMinIOClient(Config{
		MCBinary:      writeFakeMC(t, dir, shareJSON, `{}`),
		Alias:         "agentteams",
		StoragePrefix: "agentteams/agentteams-storage",
		Bucket:        "agentteams-storage",
	})
}

func TestPresignGet_ParsesShareURL(t *testing.T) {
	dir := t.TempDir()
	c := newPresignTestClient(t, dir, fakePresignShareJSON)

	url, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	wantURL := "http://fake-oss:9000/agentteams/agentteams-storage/humans/alice/files/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=300&X-Amz-Signature=fake"
	if url != wantURL {
		t.Fatalf("url = %q, want %q", url, wantURL)
	}

	// The mc invocation must be exactly
	// `share download --expire <ttl> --json <fullPath>`.
	args, err := os.ReadFile(filepath.Join(dir, "mc-args.log"))
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	wantArgs := "share download --expire 5m0s --json agentteams/agentteams-storage/humans/alice/files/a.txt"
	if string(args) != wantArgs+"\n" {
		t.Fatalf("args = %q, want %q", string(args), wantArgs+"\n")
	}
	if _, err := os.Stat(filepath.Join(dir, "mc-marker")); err != nil {
		t.Fatalf("fake mc was not invoked: %v", err)
	}
}

func TestPresignGet_TTLFormattedForMC(t *testing.T) {
	dir := t.TempDir()
	c := newPresignTestClient(t, dir, fakePresignShareJSON)

	if _, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 90*time.Second); err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "mc-args.log"))
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	if !strings.Contains(string(args), "--expire 1m30s") {
		t.Fatalf("args = %q, want --expire 1m30s", string(args))
	}
}

func TestPresignGet_MCFailure(t *testing.T) {
	dir := t.TempDir()
	c := newPresignTestClient(t, dir, fakePresignShareJSON)
	t.Setenv("FAKE_MC_FAIL", "1")

	if _, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 5*time.Minute); err == nil {
		t.Fatal("PresignGet: want error when mc fails, got nil")
	}
}

func TestPresignGet_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	c := newPresignTestClient(t, dir, `not json`)

	_, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "parse mc share download") {
		t.Fatalf("err = %v, want a parse error", err)
	}
}

func TestPresignGet_EmptyShare(t *testing.T) {
	dir := t.TempDir()
	c := newPresignTestClient(t, dir, `{"status":"success"}`)

	_, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "empty share URL") {
		t.Fatalf("err = %v, want an empty-share error", err)
	}
}

func TestPresignGet_NonPositiveTTL(t *testing.T) {
	dir := t.TempDir()
	c := newPresignTestClient(t, dir, fakePresignShareJSON)

	if _, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 0); err == nil {
		t.Fatal("PresignGet(ttl=0): want error, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "mc-marker")); err == nil {
		t.Fatal("fake mc must not be invoked for a non-positive ttl")
	}
}
