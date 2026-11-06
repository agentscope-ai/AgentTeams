package oss

// backend_matrix_test.go — env-gated real-backend matrix: the storage
// contract (put/get/stat/list/presign-get/presign-put/delete-prefix/
// conditional-put/error-mapping) runs against whichever S3-compatible
// backend AGENTTEAMS_OSS_KIND names (minio or rustfs), so a backend swap
// is caught here instead of in production.
//
// Environment — when any variable is unset every test in this file is
// t.Skip'd and CI is unaffected:
//
//	AGENTTEAMS_OSS_KIND=minio|rustfs
//	AGENTTEAMS_TEST_OSS_ENDPOINT=http://127.0.0.1:9000
//	AGENTTEAMS_TEST_OSS_AK=minioadmin
//	AGENTTEAMS_TEST_OSS_SK=minioadmin
//	AGENTTEAMS_TEST_OSS_BUCKET=agentteams-matrix
//
// Example (the CI recipe is the RustFS job in
// .github/workflows/test-human-files-s3.yml):
//
//	docker run -d --name rustfs-matrix -p 9000:9000 \
//	  -e RUSTFS_ACCESS_KEY=minioadmin -e RUSTFS_SECRET_KEY=minioadmin \
//	  rustfs/rustfs:1.0.4
//	AGENTTEAMS_OSS_KIND=rustfs AGENTTEAMS_TEST_OSS_ENDPOINT=http://127.0.0.1:9000 \
//	  AGENTTEAMS_TEST_OSS_AK=minioadmin AGENTTEAMS_TEST_OSS_SK=minioadmin \
//	  AGENTTEAMS_TEST_OSS_BUCKET=agentteams-matrix \
//	  go test ./internal/oss/ -run 'TestMatrix' -count=1 -v

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// backendEnv is the resolved matrix configuration.
type backendEnv struct {
	kind     string // "minio" or "rustfs"
	endpoint string
	ak       string
	sk       string
	bucket   string
}

func backendMatrixEnv() (backendEnv, bool) {
	env := backendEnv{
		kind:     strings.ToLower(strings.TrimSpace(os.Getenv("AGENTTEAMS_OSS_KIND"))),
		endpoint: strings.TrimSpace(os.Getenv("AGENTTEAMS_TEST_OSS_ENDPOINT")),
		ak:       os.Getenv("AGENTTEAMS_TEST_OSS_AK"),
		sk:       os.Getenv("AGENTTEAMS_TEST_OSS_SK"),
		bucket:   os.Getenv("AGENTTEAMS_TEST_OSS_BUCKET"),
	}
	if env.kind != "minio" && env.kind != "rustfs" {
		return backendEnv{}, false
	}
	if env.endpoint == "" || env.ak == "" || env.sk == "" || env.bucket == "" {
		return backendEnv{}, false
	}
	return env, true
}

// requireBackend resolves the matrix environment or skips the test.
func requireBackend(t *testing.T) backendEnv {
	t.Helper()
	env, ok := backendMatrixEnv()
	if !ok {
		t.Skip("set AGENTTEAMS_OSS_KIND=minio|rustfs plus AGENTTEAMS_TEST_OSS_ENDPOINT / AGENTTEAMS_TEST_OSS_AK / AGENTTEAMS_TEST_OSS_SK / AGENTTEAMS_TEST_OSS_BUCKET to run the real-backend matrix")
	}
	return env
}

// newMatrixClient builds the client under test: object traffic is
// confined to a unique per-test prefix inside the configured bucket and
// swept again through DeletePrefix on cleanup (DeletePrefix is itself
// under test, so a failed sweep fails the test, not just the log).
func newMatrixClient(t *testing.T, env backendEnv, sub string) (*MinIOClient, string) {
	t.Helper()
	root := fmt.Sprintf("oss-matrix/%s-%d/%s/", env.kind, time.Now().UnixNano(), sub)
	c := NewMinIOClient(Config{
		Endpoint:       env.endpoint,
		AccessKey:      env.ak,
		SecretKey:      env.sk,
		Bucket:         env.bucket,
		StoragePrefix:  env.bucket,
		Alias:          "matrix-" + env.kind,
		PublicEndpoint: env.endpoint,
	})
	t.Cleanup(func() {
		// t.Context() is already canceled when Cleanup runs; sweep on a
		// separate bounded context instead.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := c.DeletePrefix(ctx, root); err != nil {
			t.Errorf("matrix cleanup DeletePrefix(%s): %v", root, err)
		}
	})
	return c, root
}

// TestMatrix_PutGetStat runs the basic round trip: put bytes, read them
// back, stat them, and check the metadata the write endpoints rely on.
func TestMatrix_PutGetStat(t *testing.T) {
	env := requireBackend(t)
	c, root := newMatrixClient(t, env, "pg")
	ctx := t.Context()
	key := root + "a.txt"
	data := []byte("hello " + env.kind)

	if err := c.PutObject(ctx, key, data); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	got, err := c.GetObject(ctx, key)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("round trip = %q, want %q", got, data)
	}
	if err := c.Stat(ctx, key); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	meta, err := c.StatMeta(ctx, key)
	if err != nil {
		t.Fatalf("StatMeta: %v", err)
	}
	if meta.Size != int64(len(data)) {
		t.Fatalf("StatMeta.Size = %d, want %d", meta.Size, len(data))
	}
	// Single-part writes carry the MD5 as ETag on both backends
	// (quotes and case normalized: the provider may return either).
	sum := fmt.Sprintf("%x", md5.Sum(data))
	if canonicalizeETag(meta.ETag) != sum {
		t.Fatalf("StatMeta.ETag = %q, want canonical %q", meta.ETag, sum)
	}
}

// TestMatrix_List pins the one-level and the recursive listing contracts:
// the one-level view folds nested subtrees into "dir/" entries, and the
// recursive walk pages by cursor without gaps or duplicates.
func TestMatrix_List(t *testing.T) {
	env := requireBackend(t)
	c, root := newMatrixClient(t, env, "list")
	ctx := t.Context()

	seeds := map[string]string{
		root + "a":     "1",
		root + "b":     "2",
		root + "sub/c": "3",
	}
	for k, v := range seeds {
		if err := c.PutObject(ctx, k, []byte(v)); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	// One-level view: direct children by name, nested subtree as "sub/".
	names, err := c.ListObjects(ctx, root)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	sort.Strings(names)
	want := []string{"a", "b", "sub/"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("ListObjects = %v, want %v", names, want)
	}

	// The detailed view reports the direct children's sizes; the
	// directory entry carries none.
	detailed, err := c.ListObjectsDetailed(ctx, root)
	if err != nil {
		t.Fatalf("ListObjectsDetailed: %v", err)
	}
	byName := map[string]ObjectInfo{}
	for _, info := range detailed {
		byName[info.Name] = info
	}
	if len(byName) != 3 {
		t.Fatalf("ListObjectsDetailed = %+v, want 3 entries", detailed)
	}
	if byName["a"].Size != 1 || byName["b"].Size != 1 {
		t.Fatalf("direct child sizes = %d/%d, want 1/1", byName["a"].Size, byName["b"].Size)
	}
	if byName["sub/"].Size != 0 || byName["sub/"].ETag != "" {
		t.Fatalf("directory entry = %+v, want no size and no etag", byName["sub/"])
	}

	// Recursive walk with a page bound of 2: three objects take two
	// pages, the cursor resumes strictly after the last key, and the
	// walk terminates with an empty cursor.
	var walked []string
	cursor := ""
	for i := 0; i < 10; i++ {
		page, next, err := c.ListObjectsRecursive(ctx, root, cursor, 2)
		if err != nil {
			t.Fatalf("ListObjectsRecursive (page %d, cursor %q): %v", i, cursor, err)
		}
		for _, info := range page {
			walked = append(walked, info.Name)
		}
		if cursor != "" {
			// A cursor must never repeat a page: re-passing the same
			// cursor must not yield the same entries twice.
			if len(page) == 0 {
				t.Fatalf("cursor %q stalled with an empty page", cursor)
			}
		}
		if next == "" {
			break
		}
		if next == cursor {
			t.Fatalf("cursor did not advance (still %q)", cursor)
		}
		cursor = next
	}
	sort.Strings(walked)
	if strings.Join(walked, ",") != "a,b,sub/c" {
		t.Fatalf("recursive walk = %v, want [a b sub/c]", walked)
	}
}

// TestMatrix_PresignGet pins the signed-download contract: the URL is
// minted for the caller-reachable origin with a SigV4 signature, and an
// ANONYMOUS GET on it returns exactly the object's bytes — the property
// the human-files download fallback depends on.
func TestMatrix_PresignGet(t *testing.T) {
	env := requireBackend(t)
	c, root := newMatrixClient(t, env, "pget")
	ctx := t.Context()
	key := root + "download.bin"
	data := []byte("presigned body " + env.kind)
	if err := c.PutObject(ctx, key, data); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := c.PresignGet(ctx, key, 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	u, perr := url.Parse(got)
	if perr != nil {
		t.Fatalf("parse %q: %v", got, perr)
	}
	endpoint, perr := url.Parse(env.endpoint)
	if perr != nil {
		t.Fatalf("parse endpoint %q: %v", env.endpoint, perr)
	}
	if u.Scheme != endpoint.Scheme || u.Host != endpoint.Host {
		t.Fatalf("presigned origin = %s://%s, want the configured %s://%s", u.Scheme, u.Host, endpoint.Scheme, endpoint.Host)
	}
	if u.Path != "/" + env.bucket + "/" + key {
		t.Fatalf("presigned path = %q, want /%s/%s", u.Path, env.bucket, key)
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "300" {
		t.Fatalf("X-Amz-Expires = %q, want 300", q.Get("X-Amz-Expires"))
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Fatal("presigned URL carries no X-Amz-Signature")
	}

	// The whole point: nobody with the URL (no credentials) can read it.
	resp, err := http.Get(got)
	if err != nil {
		t.Fatalf("anonymous GET on the presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET status = %d, want 200", resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read presigned body: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("presigned body = %q, want %q", buf.Bytes(), data)
	}
}

// TestMatrix_PresignUpload pins the direct-upload contract for both
// provider shapes: a POST policy form (MinIO's default) and the presigned
// PUT the client falls back to. After the upload the object must be
// readable at exactly the signed key.
func TestMatrix_PresignUpload(t *testing.T) {
	env := requireBackend(t)
	c, root := newMatrixClient(t, env, "pup")
	ctx := t.Context()
	key := root + "upload.bin"
	data := []byte("direct upload " + env.kind)

	method, uploadURL, fields, err := c.PresignUpload(ctx, key, 1<<20, "text/plain", "", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}

	var do func() (*http.Response, error)
	switch method {
	case "POST":
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range fields {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatalf("write form field %s: %v", k, err)
			}
		}
		fw, err := mw.CreateFormFile("file", "upload.bin")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatalf("write form file: %v", err)
		}
		if err := mw.Close(); err != nil {
			t.Fatalf("close multipart: %v", err)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, &body)
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		do = func() (*http.Response, error) { return http.DefaultClient.Do(req) }
	case "PUT":
		req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("build PUT: %v", err)
		}
		req.Header.Set("Content-Type", "text/plain")
		do = func() (*http.Response, error) { return http.DefaultClient.Do(req) }
	default:
		t.Fatalf("PresignUpload method = %q, want POST or PUT", method)
	}

	resp, err := do()
	if err != nil {
		t.Fatalf("direct upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("direct upload status = %d, want 200 or 204", resp.StatusCode)
	}

	got, err := c.GetObject(ctx, key)
	if err != nil {
		t.Fatalf("GetObject after upload: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("uploaded object = %q, want %q", got, data)
	}
}

// TestMatrix_DeletePrefix pins the directory-sweep contract: DeletePrefix
// deletes exactly the object at the prefix key and everything under it
// (tombstone marker objects included), a sibling that merely shares the
// prefix string survives, a trailing-slash prefix sweeps the same scope,
// and a second sweep of the now-empty prefix is a clean no-op.
func TestMatrix_DeletePrefix(t *testing.T) {
	env := requireBackend(t)
	c, root := newMatrixClient(t, env, "dp")
	ctx := t.Context()

	// assertScopeEmpty walks the directory scope (boundary slash
	// included) and fails if anything is left under it. It runs in the
	// test body, where t.Context() is still live — the separate-context
	// fix applies to t.Cleanup only.
	assertScopeEmpty := func(scope string) {
		page, cursor, err := c.ListObjectsRecursive(ctx, scope, "", 100)
		if err != nil {
			t.Fatalf("walk of %s after sweep: %v", scope, err)
		}
		if len(page) != 0 || cursor != "" {
			t.Fatalf("walk of %s after sweep = %+v cursor %q, want empty", scope, page, cursor)
		}
	}

	target := root + "x"
	for _, k := range []string{
		target + "/1",
		target + "/y/2",
		target + "-sibling/keep", // shares the string prefix, not a subdirectory → must survive
	} {
		if err := c.PutObject(ctx, k, []byte("v")); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	// A tombstone marker must be swept too: it is an ordinary object as
	// far as the storage API is concerned.
	if err := c.PutObjectWithType(ctx, target+"/tombstone", nil, tombstoneContentType); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}

	if err := c.DeletePrefix(ctx, target); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}

	// Everything at and under the target is gone.
	for _, k := range []string{target + "/1", target + "/y/2", target + "/tombstone"} {
		if err := c.Stat(ctx, k); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat(%s) after sweep = %v, want ErrNotExist", k, err)
		}
	}
	assertScopeEmpty(target + "/")

	// A sibling that merely shares the prefix string survives.
	if err := c.Stat(ctx, target+"-sibling/keep"); err != nil {
		t.Fatalf("sibling vanished: Stat(%s) = %v", target+"-sibling/keep", err)
	}

	// Sweeping an already-empty prefix is a no-op.
	if err := c.DeletePrefix(ctx, target); err != nil {
		t.Fatalf("second DeletePrefix: %v", err)
	}
	assertScopeEmpty(target + "/")
	if err := c.Stat(ctx, target+"-sibling/keep"); err != nil {
		t.Fatalf("sibling vanished after no-op sweep: Stat(%s) = %v", target+"-sibling/keep", err)
	}

	// Trailing slash sweeps the same directory scope: the same seeds —
	// plus the marker object at the bare key — and the same survivors.
	target2 := root + "z"
	for _, k := range []string{
		target2, // the object whose key is exactly the target
		target2 + "/1",
		target2 + "/y/2",
		target2 + "-sibling/keep",
	} {
		if err := c.PutObject(ctx, k, []byte("v")); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	if err := c.DeletePrefix(ctx, target2+"/"); err != nil {
		t.Fatalf("DeletePrefix (trailing slash): %v", err)
	}
	for _, k := range []string{target2, target2 + "/1", target2 + "/y/2"} {
		if err := c.Stat(ctx, k); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat(%s) after trailing-slash sweep = %v, want ErrNotExist", k, err)
		}
	}
	assertScopeEmpty(target2 + "/")
	if err := c.Stat(ctx, target2+"-sibling/keep"); err != nil {
		t.Fatalf("sibling vanished: Stat(%s) = %v", target2+"-sibling/keep", err)
	}
}

// TestMatrix_ConditionalPut pins the write-conflict semantics both
// backends must provide: create-if-absent, compare-and-swap on the fresh
// ETag, rejection of a stale ETag, and the strict variants' fail-closed
// errors. (Neither backend is allowed to answer 501 here — conditional
// PUT is part of the S3-only contract.)
func TestMatrix_ConditionalPut(t *testing.T) {
	env := requireBackend(t)
	c, root := newMatrixClient(t, env, "cond")
	ctx := t.Context()
	key := root + "v"

	// Create-if-absent succeeds once and fails on the existing key.
	if err := c.PutObjectIfAbsent(ctx, key, []byte("one")); err != nil {
		t.Fatalf("PutObjectIfAbsent: %v", err)
	}
	if err := c.PutObjectIfAbsent(ctx, key, []byte("one-again")); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("second PutObjectIfAbsent = %v, want ErrPreconditionFailed", err)
	}

	fresh, err := c.StatMeta(ctx, key)
	if err != nil || fresh.ETag == "" {
		t.Fatalf("StatMeta: meta=%+v err=%v", fresh, err)
	}

	// Compare-and-swap on the current ETag applies.
	if err := c.PutObjectIfMatch(ctx, key, []byte("two"), fresh.ETag); err != nil {
		t.Fatalf("PutObjectIfMatch (fresh etag): %v", err)
	}
	// A stale ETag is rejected without writing.
	stale := "00000000000000000000000000000000"
	if err := c.PutObjectIfMatch(ctx, key, []byte("three"), stale); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("PutObjectIfMatch (stale etag) = %v, want ErrPreconditionFailed", err)
	}

	// The strict variants must not degrade to 501 on either backend.
	current, err := c.StatMeta(ctx, key)
	if err != nil {
		t.Fatalf("StatMeta before strict: %v", err)
	}
	if err := c.PutObjectIfMatchStrict(ctx, key, []byte("four"), current.ETag); err != nil {
		t.Fatalf("PutObjectIfMatchStrict: %v", err)
	}
	if err := c.PutObjectIfNoneMatchStrict(ctx, key, []byte("five")); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("PutObjectIfNoneMatchStrict (existing key) = %v, want ErrPreconditionFailed", err)
	}
}

// TestMatrix_ErrorMapping pins the S3-only error normalization: a missing
// object and a missing bucket both surface as os.ErrNotExist — the
// client keys off the 404 status, never a MinIO-specific error code (the
// backend may answer object_missing or NoSuchKey).
func TestMatrix_ErrorMapping(t *testing.T) {
	env := requireBackend(t)
	c, root := newMatrixClient(t, env, "err")
	ctx := t.Context()
	missing := root + "nope"

	for name, op := range map[string]func() error{
		"GetObject": func() error { _, err := c.GetObject(ctx, missing); return err },
		"Stat":      func() error { return c.Stat(ctx, missing) },
		"StatMeta":  func() error { _, err := c.StatMeta(ctx, missing); return err },
	} {
		err := op()
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s(missing key) = %v, want ErrNotExist (kind=%s)", name, err, env.kind)
		}
	}

	// DeleteObject is idempotent: S3 answers 204 for a missing key.
	if err := c.DeleteObject(ctx, missing); err != nil {
		t.Fatalf("DeleteObject(missing key) = %v, want nil (S3 deletes are idempotent)", err)
	}

	// A request against a bucket that does not exist is also a
	// normalized not-found, not a backend-specific error code.
	ghost := NewMinIOClient(Config{
		Endpoint:       env.endpoint,
		AccessKey:      env.ak,
		SecretKey:      env.sk,
		Bucket:         fmt.Sprintf("no-such-%s-%d", env.kind, time.Now().UnixNano()),
		StoragePrefix:  "",
		Alias:          "matrix-" + env.kind + "-ghost",
	})
	for name, op := range map[string]func() error{
		"GetObject(ghost bucket)": func() error { _, err := ghost.GetObject(ctx, "a/b"); return err },
		"Stat(ghost bucket)":      func() error { return ghost.Stat(ctx, "a/b") },
		"StatMeta(ghost bucket)":  func() error { _, err := ghost.StatMeta(ctx, "a/b"); return err },
	} {
		err := op()
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s = %v, want ErrNotExist (kind=%s)", name, err, env.kind)
		}
	}
}
