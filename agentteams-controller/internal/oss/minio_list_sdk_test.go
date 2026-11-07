package oss

import (
	"context"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// TestListObjectsDetailedKeepsTimestamps pins the reason the listing moved to
// the SDK: the mc fallback (`mc find`) has no timestamp column, so a directory
// listing lost modified times and the UI hid its time column.
func TestListObjectsDetailedKeepsTimestamps(t *testing.T) {
	ts := time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC)
	c := NewMinIOClient(Config{Bucket: "b", StoragePrefix: "files/", Endpoint: "http://x", AccessKey: "k", SecretKey: "s"})
	c.sdkListObjects = func(_ context.Context, bucket string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
		if bucket != "b" {
			t.Errorf("bucket = %q, want b", bucket)
		}
		if opts.Prefix != "humans/alice/" || opts.Recursive {
			t.Errorf("opts = %+v, want prefix humans/alice/ non-recursive", opts)
		}
		ch := make(chan minio.ObjectInfo, 3)
		ch <- minio.ObjectInfo{Key: "humans/alice/a.txt", Size: 12, LastModified: ts}
		ch <- minio.ObjectInfo{Key: "humans/alice/sub/"}  // common prefix
		ch <- minio.ObjectInfo{Key: "humans/alice/.keep"} // marker, handler hides it
		close(ch)
		return ch
	}

	infos, err := c.ListObjectsDetailed(context.Background(), "humans/alice/")
	if err != nil {
		t.Fatalf("ListObjectsDetailed: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("len = %d, want 3 (%v)", len(infos), infos)
	}
	if infos[0].Name != "a.txt" || infos[0].Size != 12 {
		t.Fatalf("entry 0 = %+v", infos[0])
	}
	if infos[0].UpdatedAt != ts.Format(time.RFC3339) {
		t.Fatalf("UpdatedAt = %q, want %q", infos[0].UpdatedAt, ts.Format(time.RFC3339))
	}
	if infos[1].Name != "sub/" {
		t.Fatalf("prefix entry = %+v, want name sub/", infos[1])
	}
	if infos[1].UpdatedAt != "" {
		t.Fatalf("prefix entry must not invent a timestamp: %+v", infos[1])
	}
}

// TestListObjectsDetailedNormalizesDirectoryPrefix pins the delimiter
// boundary: a bare prefix ("scope/target") must query "scope/target/",
// otherwise the listing also matches sibling names such as "target-other".
func TestListObjectsDetailedNormalizesDirectoryPrefix(t *testing.T) {
	c := NewMinIOClient(Config{Bucket: "b", Endpoint: "http://x", AccessKey: "k", SecretKey: "s"})
	var got string
	c.sdkListObjects = func(_ context.Context, _ string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
		got = opts.Prefix
		ch := make(chan minio.ObjectInfo, 2)
		ch <- minio.ObjectInfo{Key: "scope/target/meeting notes.txt", Size: 1}
		ch <- minio.ObjectInfo{Key: "scope/target/nested/"}
		close(ch)
		return ch
	}

	infos, err := c.ListObjectsDetailed(context.Background(), "scope/target")
	if err != nil {
		t.Fatalf("ListObjectsDetailed: %v", err)
	}
	if got != "scope/target/" {
		t.Fatalf("prefix = %q, want scope/target/", got)
	}
	if len(infos) != 2 || infos[0].Name != "meeting notes.txt" || infos[1].Name != "nested/" {
		t.Fatalf("entries = %+v", infos)
	}
}

// TestListObjectsDetailedKeepsBucketRootPrefix: an empty prefix must stay
// empty (no leading slash is invented at the bucket root).
func TestListObjectsDetailedKeepsBucketRootPrefix(t *testing.T) {
	c := NewMinIOClient(Config{Bucket: "b", Endpoint: "http://x", AccessKey: "k", SecretKey: "s"})
	var got string
	c.sdkListObjects = func(_ context.Context, _ string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
		got = opts.Prefix
		ch := make(chan minio.ObjectInfo, 1)
		ch <- minio.ObjectInfo{Key: "files/a.txt"}
		close(ch)
		return ch
	}
	if _, err := c.ListObjectsDetailed(context.Background(), ""); err != nil {
		t.Fatalf("ListObjectsDetailed: %v", err)
	}
	if got != "" {
		t.Fatalf("prefix = %q, want empty", got)
	}
}
