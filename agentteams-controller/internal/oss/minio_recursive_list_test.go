package oss

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// fakeS3ListStore answers an injected SDK ListObjects request the way a
// real S3-compatible store (MinIO, RustFS) does: plain string-prefix
// match on the bucket keys, resume strictly after StartAfter, at most
// MaxKeys keys, in key order. It holds only bucket-relative keys — the
// coordinate every real backend lists — so a request prefix wrapped with
// the client's mc StoragePrefix (alias/bucket) matches nothing and the
// walk comes up empty: the exact symptom the production handler
// reproduced on RustFS 1.0.4.
type fakeS3ListStore struct {
	keys []string
}

func (s *fakeS3ListStore) list(opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	matched := make([]string, 0, len(s.keys))
	for _, key := range s.keys {
		if strings.HasPrefix(key, opts.Prefix) && key > opts.StartAfter {
			matched = append(matched, key)
		}
	}
	sort.Strings(matched)
	if len(matched) > opts.MaxKeys {
		matched = matched[:opts.MaxKeys]
	}
	ch := make(chan minio.ObjectInfo, len(matched))
	for _, key := range matched {
		ch <- minio.ObjectInfo{
			Key:          key,
			Size:         int64(len(key)),
			ETag:         "etag-of-" + key,
			LastModified: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	close(ch)
	return ch
}

// TestMinIOListObjectsRecursiveBucketRelative pins the coordinate
// contract of the native walk: the request Prefix/StartAfter are
// bucket-relative (the bucket rides on the SDK call), and the returned
// Name/nextKey are storage-relative. If the request Prefix is ever
// wrapped with the mc StoragePrefix again, the store answers empty and
// both the page assertions and the direct opts assertions below go red.
func TestMinIOListObjectsRecursiveBucketRelative(t *testing.T) {
	const (
		prefix = "humans/alice/files/"
		bucket = "agentteams-storage"
	)
	store := &fakeS3ListStore{keys: []string{
		prefix + "a.txt",
		prefix + "docs/b.txt",
		prefix + "docs/nested/c.txt",
		prefix + "x/y/z/deep.txt",
		"humans/bob/files/b.txt", // outside the prefix: must never leak in
	}}

	type seenReq struct {
		bucket, pfx, startAfter string
	}
	var seen []seenReq
	// The production coordinate setup: StoragePrefix is the mc
	// alias/bucket path, exactly as NewMinIOClient consumers configure it.
	c := NewMinIOClient(Config{
		Endpoint:      "http://127.0.0.1:9000",
		AccessKey:     "access-key",
		SecretKey:     "secret-key",
		Alias:         "agentteams",
		Bucket:        bucket,
		StoragePrefix: "agentteams/" + bucket,
	})
	c.sdkListObjects = func(_ context.Context, b string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
		seen = append(seen, seenReq{bucket: b, pfx: opts.Prefix, startAfter: opts.StartAfter})
		return store.list(opts)
	}

	var walked []ObjectInfo
	cursor := ""
	for page := 0; page < 8; page++ {
		infos, next, err := c.ListObjectsRecursive(context.Background(), prefix, cursor, 2)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(infos) > 2 {
			t.Fatalf("page %d: %d entries, want at most 2: %+v", page, len(infos), infos)
		}
		walked = append(walked, infos...)
		if next == "" {
			break
		}
		if !strings.HasPrefix(next, prefix) {
			t.Fatalf("page %d: nextKey %q is not a storage-relative key under %q", page, next, prefix)
		}
		cursor = next
	}

	wantNames := []string{"a.txt", "docs/b.txt", "docs/nested/c.txt", "x/y/z/deep.txt"}
	if len(walked) != len(wantNames) {
		t.Fatalf("walk collected %d entries, want %d: %+v", len(walked), len(wantNames), walked)
	}
	for i, want := range wantNames {
		got := walked[i]
		if got.Name != want {
			t.Fatalf("walked[%d].Name = %q, want %q (sorted flat order): %+v", i, got.Name, want, walked)
		}
		if wantETag := "etag-of-" + prefix + want; got.ETag != wantETag {
			t.Fatalf("walked[%d].ETag = %q, want %q", i, got.ETag, wantETag)
		}
	}

	// The coordinate assertions: every SDK request carries the bucket
	// separately and a bucket-relative Prefix — never the mc alias/bucket
	// wrapper. Re-wrapping the Prefix with StoragePrefix makes the store
	// answer empty (the RustFS symptom) and these assertions fail.
	if len(seen) != 3 {
		t.Fatalf("the walk made %d SDK requests, want 3 (two full pages + the empty final page)", len(seen))
	}
	for i, req := range seen {
		if req.bucket != bucket {
			t.Fatalf("request %d: bucket = %q, want %q", i, req.bucket, bucket)
		}
		if req.pfx != prefix {
			t.Fatalf("request %d: Prefix = %q, want the bucket-relative %q (fullPath would make the store answer empty)", i, req.pfx, prefix)
		}
	}
	if seen[0].startAfter != "" {
		t.Fatalf("request 0: StartAfter = %q, want empty for the first page", seen[0].startAfter)
	}
	if want := prefix + "docs/b.txt"; seen[1].startAfter != want {
		t.Fatalf("request 1: StartAfter = %q, want the previous page's cursor %q", seen[1].startAfter, want)
	}
	if want := prefix + "x/y/z/deep.txt"; seen[2].startAfter != want {
		t.Fatalf("request 2: StartAfter = %q, want the previous page's cursor %q", seen[2].startAfter, want)
	}
}
