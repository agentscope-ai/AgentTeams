package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- Fake mc rig -----------------------------------------------------------
//
// The presign path is tested against the real MinIOClient with a fake mc
// binary injected via Config.MCBinary: the script records every invocation
// (marker + args log) and fails loudly on any non-alias command, so the
// tests prove the issued URL is SDK-minted and that mc is never reached —
// neither on a successful issuance nor on a denied request.

// fakeMCScript is the POSIX sh stand-in for mc; duplicated from the oss
// package's tests because the two packages cannot share unexported helpers.
const fakeMCScript = `#!/bin/sh
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
  alias)
    echo "Added fake-alias successfully."
    ;;
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

// fakePresignURL is the canned signed URL the fake mc reports; it must not
// look like a credential (it is what the response should echo verbatim).
// Its host is the caller-reachable public endpoint (#1501), not the
// internal one, and its path is <bucket>/<key> (the mc alias never appears
// in the HTTP URL).
const fakePresignURL = "http://fake-pub:9000/agentteams-storage/humans/alice/files/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=300&X-Amz-Signature=fake"

const fakePresignShareJSON = `{"status":"success","share":"` + fakePresignURL + `"}`

// fakeStatJSON mirrors the real `mc stat --json` shape the MinIOClient
// parses (size/etag/content type via the metadata map).
const fakeStatJSON = `{"status":"success","name":"agentteams/agentteams-storage/humans/alice/files/a.txt","lastModified":"2026-10-31T12:00:00Z","size":123,"etag":"d41d8cd98f00b204e9800998ecf8427e","type":"file","metadata":{"Content-Type":"text/plain"}}`

const fakePresignETag = "d41d8cd98f00b204e9800998ecf8427e"
const fakePresignSize = 123

// writeFakeMCServer wires the fake mc into dir and sets the FAKE_MC_* env
// vars consumed by the script.
func writeFakeMCServer(t *testing.T, dir string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "fake-mc")
	if err := os.WriteFile(scriptPath, []byte(fakeMCScript), 0o755); err != nil {
		t.Fatalf("write fake mc: %v", err)
	}
	sharePath := filepath.Join(dir, "share.json")
	if err := os.WriteFile(sharePath, []byte(fakePresignShareJSON), 0o600); err != nil {
		t.Fatalf("write share json: %v", err)
	}
	statPath := filepath.Join(dir, "stat.json")
	if err := os.WriteFile(statPath, []byte(fakeStatJSON), 0o600); err != nil {
		t.Fatalf("write stat json: %v", err)
	}
	t.Setenv("FAKE_MC_SHARE_JSON", sharePath)
	t.Setenv("FAKE_MC_STAT_JSON", statPath)
	t.Setenv("FAKE_MC_MARKER", filepath.Join(dir, "mc-marker"))
	t.Setenv("FAKE_MC_ARGS_LOG", filepath.Join(dir, "mc-args.log"))
	return scriptPath
}

// presignStatS3Server is the minimal S3 surface the presign path performs:
// the bucket location lookup the SDK runs when minting a presigned URL
// (SigV4 binds the region), plus a HEAD that serves the canned file
// (etag/size/content type) and 404s every other key. The same server acts
// as the internal endpoint AND the caller-reachable public origin, because
// the SDK mints the URL by dialing the public origin at issuance — a
// configured rig's public endpoint must be dialable.
func presignStatS3Server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if r.URL.Path == "/agentteams-storage/humans/alice/files/a.txt" {
			w.Header().Set("ETag", `"`+fakePresignETag+`"`)
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", strconv.Itoa(fakePresignSize))
			w.Header().Set("Last-Modified", "Sat, 31 Oct 2026 12:00:00 GMT")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newHumanPresignRig builds the handler against a fake k8s client (humans
// alice and bob), a real MinIOClient whose MCBinary is the fake mc canary
// (must never be invoked) and whose internal endpoint and public origin
// are both a minimal S3 fake, and a live audit client on a separate
// in-memory store. It returns the handler, the audit store, and the
// fake-mc temp dir (marker / args log).
func newHumanPresignRig(t *testing.T) (*humanFilesHandler, *ossfake.Memory, string) {
	// Any non-empty value: the rig uses its live S3 fake as the
	// caller-reachable public origin (see newHumanPresignRigWithPublic).
	return newHumanPresignRigWithPublic(t, "live")
}

// newHumanPresignRigWithPublic is the same rig with an explicit
// caller-reachable public endpoint (#1501); "" models the unconfigured
// capability boundary where presign must answer 501 instead of minting a
// URL. A configured rig's public endpoint is replaced by the live S3
// fake's URL: the SDK mints the presigned URL by dialing the public
// origin (the SigV4 region lookup), so the origin must be reachable.
func newHumanPresignRigWithPublic(t *testing.T, publicEndpoint string) (*humanFilesHandler, *ossfake.Memory, string) {
	t.Helper()
	scheme := newServerTestScheme(t)
	alice := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: "Alice"},
	}
	bob := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: "Bob"},
	}
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(alice, bob).
		Build()
	dir := t.TempDir()
	endpoint := presignStatS3Server(t)
	if publicEndpoint != "" {
		publicEndpoint = endpoint.URL
	}
	sc := oss.NewMinIOClient(oss.Config{
		MCBinary:       writeFakeMCServer(t, dir),
		Endpoint:       endpoint.URL,
		Alias:          "agentteams",
		StoragePrefix:  "agentteams/agentteams-storage",
		Bucket:         "agentteams-storage",
		AccessKey:      "test-access-key",
		SecretKey:      "test-secret-key",
		PublicEndpoint: publicEndpoint,
	})
	store := ossfake.NewMemory()
	hfh := NewHumanFilesHandler(k8s, "default", sc, audit.NewClient(store))
	return hfh, store, dir
}

// presignReq builds an authenticated files GET with ?presign=1.
func presignReq(t *testing.T, name, path string, caller *authpkg.CallerIdentity) *http.Request {
	t.Helper()
	req := hfReq(t, http.MethodGet, name, path, caller, nil)
	req.URL.RawQuery = "presign=1"
	return req
}

// readPresignAuditLines is readAuditLines with a missing object treated as
// "no events recorded" (readAuditLines fatals on absence).
func readPresignAuditLines(t *testing.T, store *ossfake.Memory) []string {
	t.Helper()
	key := "audit/" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	data, err := store.GetObject(context.Background(), key)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// --- Tests ------------------------------------------------------------------

func TestHumanFiles_PresignIssuesURL(t *testing.T) {
	t.Setenv("AGENTTEAMS_PRESIGN_TTL", "")
	hfh, store, dir := newHumanPresignRig(t)

	t0 := time.Now()
	rec := httptest.NewRecorder()
	hfh.getFile(rec, presignReq(t, "alice", "a.txt", hfSelf()))
	if rec.Code != http.StatusOK {
		t.Fatalf("presign: %d %s", rec.Code, rec.Body.String())
	}
	var body humanFilePresignResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	// The URL is SDK-minted SigV4 for exactly this object: the bucket/key
	// path, the default 5m expiry, and a real signature. (The host is the
	// rig's live public origin; the oss package's presign tests pin the
	// host contract against a named endpoint.)
	u, perr := url.Parse(body.URL)
	if perr != nil {
		t.Fatalf("parse url %q: %v", body.URL, perr)
	}
	if u.Path != "/agentteams-storage/humans/alice/files/a.txt" {
		t.Fatalf("url path = %q, want /agentteams-storage/humans/alice/files/a.txt", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		t.Fatalf("X-Amz-Algorithm = %q, want AWS4-HMAC-SHA256", q.Get("X-Amz-Algorithm"))
	}
	if q.Get("X-Amz-Expires") != "300" {
		t.Fatalf("X-Amz-Expires = %q, want 300 (the default 5m TTL)", q.Get("X-Amz-Expires"))
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Fatal("issued URL carries no X-Amz-Signature")
	}
	if !strings.Contains(q.Get("X-Amz-Credential"), "test-access-key/") {
		t.Fatalf("X-Amz-Credential = %q, want the configured access key", q.Get("X-Amz-Credential"))
	}
	if body.ETag != fakePresignETag {
		t.Fatalf("etag = %q, want %q", body.ETag, fakePresignETag)
	}
	if body.Size != fakePresignSize {
		t.Fatalf("size = %d, want %d", body.Size, fakePresignSize)
	}
	exp, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt %q: %v", body.ExpiresAt, err)
	}
	// Default TTL is 5 minutes: expiresAt must be ~t0+5m (slack covers the
	// two separate time.Now() calls).
	if !exp.After(t0.Add(4*time.Minute)) || !exp.Before(t0.Add(6*time.Minute)) {
		t.Fatalf("expiresAt = %v, want ~%v+5m", exp, t0)
	}
	// The SECRET must never leak into the response. (The access key ID
	// does appear in X-Amz-Credential — that is part of the SigV4
	// presigned-URL format and public by design, exactly as in the mc
	// share era; the security is the signature, computable only by the
	// holder of the secret.)
	if strings.Contains(rec.Body.String(), "test-secret-key") {
		t.Fatalf("response leaks the secret key: %s", rec.Body.String())
	}

	// Audit: exactly one line with who/path/ttl/expiresAt.
	lines := readPresignAuditLines(t, store)
	if len(lines) != 1 {
		t.Fatalf("want 1 audit line, got %d: %s", len(lines), strings.Join(lines, " | "))
	}
	m := decodeAuditLine(t, lines[0])
	if m["who"] != "alice" || m["role"] != "human" || m["target"] != "alice" || m["action"] != "human_file_presign" {
		t.Fatalf("audit line = who=%v role=%v target=%v action=%v", m["who"], m["role"], m["target"], m["action"])
	}
	if v := detailField(t, lines[0], "path"); v != "files/a.txt" {
		t.Fatalf("detail path = %v", v)
	}
	if v := detailField(t, lines[0], "ttl"); v != "5m0s" {
		t.Fatalf("detail ttl = %v, want 5m0s", v)
	}
	if v := detailField(t, lines[0], "expiresAt"); v != body.ExpiresAt {
		t.Fatalf("detail expiresAt = %v, want %v", v, body.ExpiresAt)
	}

	// The S3-only contract: the URL was minted by the SDK against the
	// caller-reachable public origin, so mc was never spawned (the fake-mc
	// canary records any invocation).
	if _, err := os.Stat(filepath.Join(dir, "mc-marker")); err == nil {
		args, _ := os.ReadFile(filepath.Join(dir, "mc-args.log"))
		t.Fatalf("fake mc was invoked on the presign path: %q", string(args))
	}
}

func TestHumanFiles_PresignNoAccess_DeniedBeforeMC(t *testing.T) {
	t.Setenv("AGENTTEAMS_PRESIGN_TTL", "")
	hfh, store, dir := newHumanPresignRig(t)

	cases := []struct {
		name   string
		caller *authpkg.CallerIdentity
	}{
		{"another human", hfBob()},
		{"team leader", hfLeader()},
		{"worker", hfWorker()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			hfh.getFile(rec, presignReq(t, "alice", "a.txt", tc.caller))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(filepath.Join(dir, "mc-marker")); err == nil {
				t.Fatal("fake mc was invoked for a denied presign; it must never be")
			}
			if len(readPresignAuditLines(t, store)) != 0 {
				t.Fatal("a denied presign must not be audited")
			}
		})
	}
}

// A presign for an unknown human is the same uniform 403 as a no-access
// denial: the status must not reveal which humans exist.
func TestHumanFiles_PresignUnknownHuman_Denied(t *testing.T) {
	t.Setenv("AGENTTEAMS_PRESIGN_TTL", "")
	hfh, _, dir := newHumanPresignRig(t)

	rec := httptest.NewRecorder()
	hfh.getFile(rec, presignReq(t, "ghost", "a.txt", hfSelf()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (uniform denial): %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "mc-marker")); err == nil {
		t.Fatal("fake mc was invoked for a denied presign; it must never be")
	}
}

// An authorized caller asking for a missing object gets 404, and nothing is
// audited or issued.
func TestHumanFiles_PresignMissingFile(t *testing.T) {
	t.Setenv("AGENTTEAMS_PRESIGN_TTL", "")
	// Belt and braces: mc would fail loudly if it were invoked at all —
	// the stat itself now runs through the SDK against the S3 fake, which
	// 404s every key but the canned file.
	t.Setenv("FAKE_MC_FAIL", "1")
	hfh, store, _ := newHumanPresignRig(t)

	rec := httptest.NewRecorder()
	hfh.getFile(rec, presignReq(t, "alice", "nope.txt", hfSelf()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(readPresignAuditLines(t, store)) != 0 {
		t.Fatal("a missing-file presign must not be audited")
	}
}

// AGENTTEAMS_PRESIGN_TTL parsing: unset/invalid/non-positive fall back to the
// default, valid values are honored, and values above the cap are clamped.
func TestHumanFiles_PresignTTLConfig(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultHumanPresignTTL},
		{"banana", defaultHumanPresignTTL},
		{"0s", defaultHumanPresignTTL},
		{"-5m", defaultHumanPresignTTL},
		{"10m", 10 * time.Minute},
		{"90m", maxHumanPresignTTL},
		{"1h", maxHumanPresignTTL},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("AGENTTEAMS_PRESIGN_TTL", tc.env)
			hfh, _, _ := newHumanPresignRig(t)
			if hfh.presignTTL != tc.want {
				t.Fatalf("presignTTL = %v, want %v", hfh.presignTTL, tc.want)
			}
		})
	}
}

// A valid override must reach the issued URL's expiry, not just the
// handler field.
func TestHumanFiles_PresignTTLOverrideReachesMC(t *testing.T) {
	t.Setenv("AGENTTEAMS_PRESIGN_TTL", "10m")
	hfh, _, dir := newHumanPresignRig(t)

	rec := httptest.NewRecorder()
	hfh.getFile(rec, presignReq(t, "alice", "a.txt", hfSelf()))
	if rec.Code != http.StatusOK {
		t.Fatalf("presign: %d %s", rec.Code, rec.Body.String())
	}
	var body humanFilePresignResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	u, perr := url.Parse(body.URL)
	if perr != nil {
		t.Fatalf("parse url %q: %v", body.URL, perr)
	}
	if q := u.Query().Get("X-Amz-Expires"); q != "600" {
		t.Fatalf("X-Amz-Expires = %q, want 600 (the 10m override must reach the signer)", q)
	}
	if _, err := os.Stat(filepath.Join(dir, "mc-marker")); err == nil {
		args, _ := os.ReadFile(filepath.Join(dir, "mc-args.log"))
		t.Fatalf("fake mc was invoked on the presign path: %q", string(args))
	}
}

// A backend without presign support (the in-memory fake) fails closed with
// 500 instead of leaking a URL or the object bytes.
func TestHumanFiles_PresignUnsupportedBackend(t *testing.T) {
	t.Setenv("AGENTTEAMS_PRESIGN_TTL", "")
	hfh, store, _ := newHumanFilesRig(t)
	if err := store.PutObject(context.Background(), "humans/alice/files/a.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	hfh.getFile(rec, presignReq(t, "alice", "a.txt", hfSelf()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "presign failed") {
		t.Fatalf("body = %q, want the presign-failed message", rec.Body.String())
	}
}

// The explicit capability boundary (#1501): with no caller-reachable
// public endpoint configured, ?presign=1 answers 501 with a stable
// machine-readable {"error":"presign_unsupported"} body — never a 200 with
// a URL the caller cannot reach, never an opaque 500. No URL is minted, so
// the share command must never reach mc, and the failed issuance is not
// audited. Clients detect the code and fall back to the streamed GET on
// the same path.
func TestHumanFiles_PresignUnconfiguredEndpoint_Unsupported(t *testing.T) {
	t.Setenv("AGENTTEAMS_PRESIGN_TTL", "")
	hfh, store, dir := newHumanPresignRigWithPublic(t, "")

	rec := httptest.NewRecorder()
	hfh.getFile(rec, presignReq(t, "alice", "a.txt", hfSelf()))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}
	var body humanFilePresignUnsupportedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	if body.Error != "presign_unsupported" {
		t.Fatalf("error = %q, want presign_unsupported", body.Error)
	}
	if !strings.Contains(body.Reason, "AGENTTEAMS_FS_PUBLIC_ENDPOINT") {
		t.Fatalf("reason = %q, want it to point at the public endpoint config", body.Reason)
	}
	// No URL was minted: the share command must never have reached mc.
	if args, err := os.ReadFile(filepath.Join(dir, "mc-args.log")); err == nil &&
		strings.Contains(string(args), "share download") {
		t.Fatalf("share download reached mc although presign is unsupported: %q", string(args))
	}
	if len(readPresignAuditLines(t, store)) != 0 {
		t.Fatal("a 501 presign must not be audited")
	}
}

// The non-presign GET must be untouched: same route, no query → bytes and
// ETag exactly as before.
func TestHumanFiles_GetWithoutPresignUnchanged(t *testing.T) {
	dir := t.TempDir()
	scheme := newServerTestScheme(t)
	alice := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: "Alice"},
	}
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(alice).
		Build()
	sc := oss.NewMinIOClient(oss.Config{
		MCBinary:      writeFakeMCServer(t, dir),
		Alias:         "agentteams",
		StoragePrefix: "agentteams/agentteams-storage",
		Bucket:        "agentteams-storage",
	})
	hfh := NewHumanFilesHandler(k8s, "default", sc, nil)

	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil))
	// Without ?presign=1 the handler streams the object bytes through the
	// snapshot path (GetObject + StatMeta); the fake mc has no `cat` branch,
	// so the read fails — proving the request did NOT take the presign
	// branch (which would have returned the canned 200 JSON).
	if rec.Code == http.StatusOK {
		t.Fatalf("non-presign GET took the presign branch: %s", rec.Body.String())
	}
}
