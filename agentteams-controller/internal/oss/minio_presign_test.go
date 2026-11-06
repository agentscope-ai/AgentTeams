package oss

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePresignMCScript is a POSIX sh stand-in for the mc CLI (#1475 PR-4,
// #1501): every invocation is recorded (marker file + args log) and the
// canned JSON from FAKE_MC_SHARE_JSON / FAKE_MC_STAT_JSON is replayed.
// `alias set` (the one-time public-alias registration for presign) is a
// no-op success. FAKE_MC_FAIL=1 makes the `share` command fail with the
// S3 object-not-found error, so the alias setup still succeeds and the
// test isolates the `share download` path.
const fakePresignMCScript = `#!/bin/sh
if [ -n "$FAKE_MC_MARKER" ]; then
  echo fake-mc-called > "$FAKE_MC_MARKER"
fi
if [ -n "$FAKE_MC_ARGS_LOG" ]; then
  echo "$*" >> "$FAKE_MC_ARGS_LOG"
fi
case "$1" in
  alias)
    echo "Added fake-alias successfully."
    ;;
  share)
    if [ "$FAKE_MC_FAIL" = "1" ]; then
      echo "error: Object does not exist (fake mc)" >&2
      exit 1
    fi
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

// fakePresignShareJSON is the canned `mc share download --json` output. The
// URL host is the public (caller-reachable) endpoint, NOT the internal one —
// that is the whole point of #1501: the returned URL's host must be the
// origin the caller can reach, and the path is <bucket>/<key> (the alias is
// not part of the HTTP URL).
const fakePresignShareJSON = `{"status":"success","share":"http://fake-pub:9000/agentteams-storage/humans/alice/files/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=300&X-Amz-Signature=fake"}`

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

// newPresignTestClient builds a MinIOClient around the fake mc with a
// configured caller-reachable public endpoint (so PresignGet actually
// signs). The internal Endpoint is empty on purpose: the main alias
// (ensureAlias) is a no-op, so the tests isolate the public-alias presign
// path from internal-alias setup.
func newPresignTestClient(t *testing.T, dir, shareJSON string) *MinIOClient {
	t.Helper()
	return NewMinIOClient(Config{
		MCBinary:       writeFakeMC(t, dir, shareJSON, `{}`),
		Alias:          "agentteams",
		StoragePrefix:  "agentteams/agentteams-storage",
		Bucket:         "agentteams-storage",
		PublicEndpoint: "http://fake-pub:9000",
		PublicAlias:    "agentteams-public",
		AccessKey:      "ak",
		SecretKey:      "sk",
	})
}

func TestPresignGet_ParsesShareURL(t *testing.T) {
	dir := t.TempDir()
	c := newPresignTestClient(t, dir, fakePresignShareJSON)

	url, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	wantURL := "http://fake-pub:9000/agentteams-storage/humans/alice/files/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=300&X-Amz-Signature=fake"
	if url != wantURL {
		t.Fatalf("url = %q, want %q", url, wantURL)
	}

	// The first mc invocation registers the public alias; the second is
	// exactly `share download --expire <ttl> --json <publicAlias>/<bucket>/<key>`
	// — the share path is bound to the public alias, never the main one.
	args, err := os.ReadFile(filepath.Join(dir, "mc-args.log"))
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	wantArgs := "alias set agentteams-public http://fake-pub:9000 ak sk\n" +
		"share download --expire 5m0s --json agentteams-public/agentteams-storage/humans/alice/files/a.txt\n"
	if string(args) != wantArgs {
		t.Fatalf("args = %q, want %q", string(args), wantArgs)
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

// TestPresignGet_UnconfiguredPublicEndpoint is the capability boundary
// (#1501): with no caller-reachable public endpoint the client must NOT
// mint a URL — it returns ErrPresignUnsupported without invoking mc at all
// (a URL signed for the internal endpoint is unusable from the caller).
func TestPresignGet_UnconfiguredPublicEndpoint(t *testing.T) {
	dir := t.TempDir()
	c := NewMinIOClient(Config{
		MCBinary:      writeFakeMC(t, dir, fakePresignShareJSON, `{}`),
		Alias:         "agentteams",
		StoragePrefix: "agentteams/agentteams-storage",
		Bucket:        "agentteams-storage",
		// PublicEndpoint intentionally left empty.
	})

	_, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 5*time.Minute)
	if !errors.Is(err, ErrPresignUnsupported) {
		t.Fatalf("err = %v, want ErrPresignUnsupported", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mc-marker")); err == nil {
		t.Fatal("fake mc must not be invoked when the public endpoint is unconfigured")
	}
}
