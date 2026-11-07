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
