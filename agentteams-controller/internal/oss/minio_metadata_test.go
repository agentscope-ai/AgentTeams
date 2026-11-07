package oss

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/minio/minio-go/v7"
)

// --- Key normalization and the dual-shape merge -----------------------------

func TestNormalizeUserMetadataKeys(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{"nil stays nil", nil, nil},
		{"empty map is nil", map[string]string{}, nil},
		{"bare keys lowercased", map[string]string{"AGENTTEAMS-SYNC-ENTRIES": "2"}, map[string]string{"agentteams-sync-entries": "2"}},
		{"prefixed keys stripped", map[string]string{"x-amz-meta-Foo-Bar": "v"}, map[string]string{"foo-bar": "v"}},
		{"prefixed keys case-insensitive", map[string]string{"X-AMZ-META-baz": "v"}, map[string]string{"baz": "v"}},
		{"empty key dropped", map[string]string{"": "v"}, nil},
		{"prefix-only key dropped", map[string]string{"x-amz-meta-": "v"}, nil},
		{"empty value dropped", map[string]string{"k": "", "ok": "v"}, map[string]string{"ok": "v"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeUserMetadataKeys(tc.in); !maps.Equal(got, tc.want) {
				t.Fatalf("normalizeUserMetadataKeys(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestObjectUserMetadataMerge(t *testing.T) {
	cases := []struct {
		name string
		info minio.ObjectInfo
		want map[string]string
	}{
		{"no metadata is nil", minio.ObjectInfo{}, nil},
		{"user metadata shape (MinIO)",
			minio.ObjectInfo{UserMetadata: map[string]string{"agentteams-sync-entries": "2"}},
			map[string]string{"agentteams-sync-entries": "2"}},
		{"raw header shape (S3/RustFS)",
			minio.ObjectInfo{Metadata: http.Header{"X-Amz-Meta-Agentteams-Sync-Entries": []string{"2"}, "Content-Type": []string{"application/json"}}},
			map[string]string{"agentteams-sync-entries": "2"}},
		{"user metadata wins on conflict",
			minio.ObjectInfo{UserMetadata: map[string]string{"k": "a"}, Metadata: http.Header{"x-amz-meta-k": []string{"b"}}},
			map[string]string{"k": "a"}},
		{"raw headers fill the gaps",
			minio.ObjectInfo{UserMetadata: map[string]string{"a": "1"}, Metadata: http.Header{"x-amz-meta-B": []string{"2"}}},
			map[string]string{"a": "1", "b": "2"}},
		{"empty values dropped",
			minio.ObjectInfo{UserMetadata: map[string]string{"a": ""}, Metadata: http.Header{"x-amz-meta-": []string{"v"}}},
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := objectUserMetadata(tc.info); !maps.Equal(got, tc.want) {
				t.Fatalf("objectUserMetadata(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}

// --- Conditional writes with metadata (seam level) ---------------------------

func TestPutObjectIfMatchWithMetadataNormalizesBeforeTheSeam(t *testing.T) {
	c := &MinIOClient{config: Config{Bucket: "bucket"}}
	var gotKey, gotType, gotMatch string
	var gotData []byte
	var gotMeta map[string]string
	c.sdkPutIfMatchWithMetadata = func(_ context.Context, key string, data []byte, contentType, matchETag string, metadata map[string]string) error {
		gotKey, gotData, gotType, gotMatch, gotMeta = key, data, contentType, matchETag, metadata
		return nil
	}
	err := c.PutObjectIfMatchWithMetadata(context.Background(), "humans/alice/.sync/manifest.json",
		[]byte("{}"), "application/json", "base-etag",
		map[string]string{"X-Amz-Meta-AGENTTEAMS-SYNC-ENTRIES": "2", "AgentTeams-Sync-Generation": "3", "": "x", "dropped": ""})
	if err != nil {
		t.Fatalf("PutObjectIfMatchWithMetadata: %v", err)
	}
	if gotKey != "humans/alice/.sync/manifest.json" {
		t.Fatalf("key = %q", gotKey)
	}
	if string(gotData) != "{}" || gotType != "application/json" || gotMatch != "base-etag" {
		t.Fatalf("data/type/match = %q/%q/%q", gotData, gotType, gotMatch)
	}
	// The seam sees the canonical bare lowercase form: the prefix is
	// stripped, case folded, and empty keys or values are dropped.
	want := map[string]string{"agentteams-sync-entries": "2", "agentteams-sync-generation": "3"}
	if !maps.Equal(gotMeta, want) {
		t.Fatalf("seam metadata = %v, want %v", gotMeta, want)
	}
}

func TestPutObjectIfMatchWithMetadataErrorMapping(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		check func(error) bool
	}{
		{"412 named maps to precondition",
			minio.ErrorResponse{Code: "PreconditionFailed", StatusCode: 412},
			func(e error) bool { return errors.Is(e, ErrPreconditionFailed) }},
		{"412 numeric maps to precondition",
			minio.ErrorResponse{Code: "412", StatusCode: 412},
			func(e error) bool { return errors.Is(e, ErrPreconditionFailed) }},
		{"5xx maps to storage unavailable",
			minio.ErrorResponse{Code: "InternalError", StatusCode: 500},
			func(e error) bool { return errors.Is(e, ErrStorageUnavailable) }},
		{"403 passes through untouched",
			minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403},
			func(e error) bool {
				return !errors.Is(e, ErrPreconditionFailed) && !errors.Is(e, ErrStorageUnavailable)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &MinIOClient{config: Config{Bucket: "bucket"}}
			c.sdkPutIfMatchWithMetadata = func(context.Context, string, []byte, string, string, map[string]string) error {
				return tc.cause
			}
			err := c.PutObjectIfMatchWithMetadata(context.Background(), "k", []byte("{}"), "application/json", "etag", nil)
			if !tc.check(err) {
				t.Fatalf("err = %v, the mapping check failed", err)
			}
		})
	}
}

func TestPutObjectIfAbsentWithMetadataMapsPrecondition(t *testing.T) {
	c := &MinIOClient{config: Config{Bucket: "bucket"}}
	c.sdkPutIfAbsentWithMetadata = func(context.Context, string, []byte, string, map[string]string) error {
		return minio.ErrorResponse{Code: "PreconditionFailed", StatusCode: 412}
	}
	err := c.PutObjectIfAbsentWithMetadata(context.Background(), "k", []byte("{}"), "application/json",
		map[string]string{"X-Amz-Meta-Agentteams-Sync-Entries": "2"})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("err = %v, want ErrPreconditionFailed", err)
	}
}

// --- Real-SDK wire behavior (canned S3 servers) ------------------------------

// metaS3Server answers StatObject (HEAD) with a canned manifest object
// carrying user metadata headers in the S3 wire form: x-amz-meta-
// prefix, mixed case. It pins what the SDK actually reports for the
// response headers (which ObjectInfo field, which case) through the
// production StatMeta merge.
func metaS3Server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if isBucketRequest(r) {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodHead {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "2")
		w.Header().Set("Last-Modified", "Fri, 30 Oct 2026 21:00:00 GMT")
		w.Header().Set("X-Amz-Meta-Agentteams-Sync-Entries", "2")
		w.Header().Set("X-Amz-Meta-Agentteams-Sync-Tombstones", "1")
		w.Header().Set("X-Amz-Meta-Agentteams-Sync-Generation", "3")
		w.Header().Set("X-Amz-Meta-Agentteams-Sync-Updated-At", "2026-11-07T00:00:00Z")
		w.Header().Set("X-Amz-Meta-Agentteams-Sync-Updated-By", "alice")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStatMetaReadsS3UserMetadata(t *testing.T) {
	c := statClient(metaS3Server(t))
	meta, err := c.StatMeta(context.Background(), "humans/alice/.sync/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"agentteams-sync-entries":    "2",
		"agentteams-sync-tombstones": "1",
		"agentteams-sync-generation": "3",
		"agentteams-sync-updated-at": "2026-11-07T00:00:00Z",
		"agentteams-sync-updated-by": "alice",
	}
	if !maps.Equal(meta.UserMetadata, want) {
		t.Fatalf("UserMetadata = %v, want %v", meta.UserMetadata, want)
	}
}

// TestPutObjectIfMatchWithMetadataSendsS3Headers pins the write side
// through the real SDK: bare keys must leave the process as x-amz-meta-
// headers and If-Match must be the quoted ETag on the wire.
func TestPutObjectIfMatchWithMetadataSendsS3Headers(t *testing.T) {
	var putHdrs http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if isBucketRequest(r) {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodPut {
			putHdrs = r.Header.Clone()
			w.Header().Set("ETag", `"new-etag"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(srv.Close)
	c := statClient(srv)

	err := c.PutObjectIfMatchWithMetadata(context.Background(), "humans/alice/.sync/manifest.json",
		[]byte("{}"), "application/json", "base-etag",
		map[string]string{"agentteams-sync-entries": "2"})
	if err != nil {
		t.Fatalf("PutObjectIfMatchWithMetadata: %v", err)
	}
	if got := putHdrs.Get("X-Amz-Meta-Agentteams-Sync-Entries"); got != "2" {
		t.Fatalf("X-Amz-Meta-Agentteams-Sync-Entries = %q, want 2 (bare keys are prefixed on the wire)", got)
	}
	if got := putHdrs.Get("If-Match"); got != `"base-etag"` {
		t.Fatalf("If-Match = %q, want the quoted ETag (SetMatchETag quotes)", got)
	}
	if got := putHdrs.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}
