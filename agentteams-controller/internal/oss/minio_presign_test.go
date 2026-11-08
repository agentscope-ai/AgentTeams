package oss

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePresignMCScript is a POSIX sh stand-in for the mc CLI on the presign
// path: every invocation is recorded (marker file + args log), `alias set`
// succeeds (the historical one-time alias registration), and ANY other
// command fails loudly. The point of the canary is the S3-only contract:
// a regression of PresignGet (or any other object-path method that shares
// this client fixture) back to mc must fail these tests, not silently
// spawn a process.
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
  *)
    echo "fake mc: the object path must not invoke mc ($1)" >&2
    exit 1
    ;;
esac
`

// writeFakeMC writes the fake mc script into dir, wires the FAKE_MC_*
// environment variables, and returns the script path.
func writeFakeMC(t *testing.T, dir string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "fake-mc")
	if err := os.WriteFile(scriptPath, []byte(fakePresignMCScript), 0o755); err != nil {
		t.Fatalf("write fake mc: %v", err)
	}
	t.Setenv("FAKE_MC_MARKER", filepath.Join(dir, "mc-marker"))
	t.Setenv("FAKE_MC_ARGS_LOG", filepath.Join(dir, "mc-args.log"))
	return scriptPath
}

// assertMCNeverCalled fails the test when the fake mc was invoked — the
// object path (presign included) must not spawn mc.
func assertMCNeverCalled(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "mc-marker")); err == nil {
		if args, aerr := os.ReadFile(filepath.Join(dir, "mc-args.log")); aerr == nil {
			t.Fatalf("fake mc was invoked on the object path: %q", strings.TrimSpace(string(args)))
		}
		t.Fatal("fake mc was invoked on the object path")
	}
}

// newPublicPresignOrigin stands in for the caller-reachable public
// storage endpoint. minio-go resolves the SigV4 region with a bucket
// location lookup against the origin at presign time, so the origin must
// be dialable; the stub answers that one-time lookup and nothing else.
func newPublicPresignOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
	}))
}

// newPresignTestClient builds a MinIOClient around the fake mc whose
// caller-reachable public endpoint is the given live origin (so
// PresignGet actually signs). The internal Endpoint is empty on purpose:
// the main alias (ensureAlias) is a no-op, so the tests isolate the
// presign path from internal-alias setup.
func newPresignTestClient(t *testing.T, dir, publicEndpoint string) *MinIOClient {
	t.Helper()
	return NewMinIOClient(Config{
		MCBinary:       writeFakeMC(t, dir),
		Alias:          "agentteams",
		StoragePrefix:  "agentteams/agentteams-storage",
		Bucket:         "agentteams-storage",
		PublicEndpoint: publicEndpoint,
		PublicAlias:    "agentteams-public",
		AccessKey:      "ak",
		SecretKey:      "sk",
	})
}

// TestPresignGet_SignsThePublicOrigin pins the S3-SDK contract that
// replaced `mc share download`: the URL's host is the caller-reachable
// public endpoint, the path is /<bucket>/<key> (the alias is not part of
// the HTTP URL), the signature is SigV4, and no mc process is involved.
func TestPresignGet_SignsThePublicOrigin(t *testing.T) {
	public := newPublicPresignOrigin(t)
	defer public.Close()
	dir := t.TempDir()
	c := newPresignTestClient(t, dir, public.URL)

	got, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	u, perr := url.Parse(got)
	if perr != nil {
		t.Fatalf("parse presigned URL %q: %v", got, perr)
	}
	wantOrigin, _ := url.Parse(public.URL)
	if u.Scheme != wantOrigin.Scheme || u.Host != wantOrigin.Host {
		t.Fatalf("presigned URL origin = %s://%s, want the caller-reachable public origin %s://%s",
			u.Scheme, u.Host, wantOrigin.Scheme, wantOrigin.Host)
	}
	if u.Path != "/agentteams-storage/humans/alice/files/a.txt" {
		t.Fatalf("presigned URL path = %q, want /<bucket>/<key>", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		t.Fatalf("X-Amz-Algorithm = %q, want AWS4-HMAC-SHA256", q.Get("X-Amz-Algorithm"))
	}
	if q.Get("X-Amz-Expires") != "300" {
		t.Fatalf("X-Amz-Expires = %q, want 300", q.Get("X-Amz-Expires"))
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Fatal("presigned URL carries no X-Amz-Signature")
	}
	if !strings.Contains(q.Get("X-Amz-Credential"), "ak/") {
		t.Fatalf("X-Amz-Credential = %q, want the configured access key", q.Get("X-Amz-Credential"))
	}
	assertMCNeverCalled(t, dir)
}

// TestPresignGet_TTLBindsTheExpiry pins that the ttl the caller passes is
// the expiry the provider enforces (mc used --expire <ttl.String()>; the
// SDK takes the duration directly).
func TestPresignGet_TTLBindsTheExpiry(t *testing.T) {
	public := newPublicPresignOrigin(t)
	defer public.Close()
	dir := t.TempDir()
	c := newPresignTestClient(t, dir, public.URL)

	got, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 90*time.Second)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	u, perr := url.Parse(got)
	if perr != nil {
		t.Fatalf("parse presigned URL %q: %v", got, perr)
	}
	if q := u.Query().Get("X-Amz-Expires"); q != "90" {
		t.Fatalf("X-Amz-Expires = %q, want 90", q)
	}
	assertMCNeverCalled(t, dir)
}

// TestPresignGet_NonPositiveTTL pins the validation boundary: a
// non-positive ttl is rejected before anything is signed, and no mc (and
// no storage) is touched.
func TestPresignGet_NonPositiveTTL(t *testing.T) {
	dir := t.TempDir()
	c := NewMinIOClient(Config{
		MCBinary:       writeFakeMC(t, dir),
		Alias:          "agentteams",
		StoragePrefix:  "agentteams/agentteams-storage",
		Bucket:         "agentteams-storage",
		PublicEndpoint: "http://127.0.0.1:1",
		AccessKey:      "ak",
		SecretKey:      "sk",
	})

	if _, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 0); err == nil {
		t.Fatal("PresignGet(ttl=0): want error, got nil")
	}
	assertMCNeverCalled(t, dir)
}

// TestPresignGet_UnconfiguredPublicEndpoint is the capability boundary
// (#1501): with no caller-reachable public endpoint the client must NOT
// mint a URL — it returns ErrPresignUnsupported without touching storage
// or mc (a URL signed for the internal endpoint is unusable from the
// caller).
func TestPresignGet_UnconfiguredPublicEndpoint(t *testing.T) {
	dir := t.TempDir()
	c := NewMinIOClient(Config{
		MCBinary:      writeFakeMC(t, dir),
		Alias:         "agentteams",
		StoragePrefix: "agentteams/agentteams-storage",
		Bucket:        "agentteams-storage",
		// PublicEndpoint intentionally left empty.
		AccessKey:     "ak",
		SecretKey:     "sk",
	})

	_, err := c.PresignGet(t.Context(), "humans/alice/files/a.txt", 5*time.Minute)
	if !errors.Is(err, ErrPresignUnsupported) {
		t.Fatalf("err = %v, want ErrPresignUnsupported", err)
	}
	assertMCNeverCalled(t, dir)
}

// TestPresignGet_SignsCurrentIdentityForDynamicCredentials pins that a
// dynamic-credential client signs each issuance with the identity
// resolved at that moment — the mc path did this through per-call
// MC_HOST_* env vars, the SDK path through newPublicSDKClient.
func TestPresignGet_SignsCurrentIdentityForDynamicCredentials(t *testing.T) {
	public := newPublicPresignOrigin(t)
	defer public.Close()

	creds := &uploadCredentials{}
	c := NewMinIOClient(Config{
		Endpoint:       "http://internal.invalid:9000",
		PublicEndpoint: public.URL,
		AccessKey:      "static",
		SecretKey:      "static",
		Bucket:         "bucket",
	}).WithCredentialSource(creds)
	key := "humans/alice/files/a.txt"

	first, err := c.PresignGet(t.Context(), key, time.Minute)
	if err != nil {
		t.Fatalf("presign 1: %v", err)
	}
	second, err := c.PresignGet(t.Context(), key, time.Minute)
	if err != nil {
		t.Fatalf("presign 2: %v", err)
	}
	if u1, _ := url.Parse(first); !strings.Contains(u1.Query().Get("X-Amz-Credential"), "temporary1/") {
		t.Fatalf("issuance 1 X-Amz-Credential = %q, want the first temporary identity", u1.Query().Get("X-Amz-Credential"))
	}
	if u2, _ := url.Parse(second); !strings.Contains(u2.Query().Get("X-Amz-Credential"), "temporary2/") {
		t.Fatalf("issuance 2 X-Amz-Credential = %q, want the rotated temporary identity", u2.Query().Get("X-Amz-Credential"))
	}
	if creds.calls < 2 {
		t.Fatalf("credentials resolved %d times, want one per issuance", creds.calls)
	}
}
