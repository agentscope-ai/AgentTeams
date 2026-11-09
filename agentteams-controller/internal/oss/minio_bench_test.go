package oss

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

// ossBenchConfig returns the Config pointed at a real S3-compatible
// backend from the test environment, or skips the benchmark: the hot-path
// numbers are only meaningful against live storage (the mc-era baseline
// was measured the same way), and there is no meaningful fake to run
// them against.
func ossBenchConfig(tb testing.TB) Config {
	tb.Helper()
	endpoint := os.Getenv("AGENTTEAMS_TEST_OSS_ENDPOINT")
	if endpoint == "" {
		tb.Skip("set AGENTTEAMS_TEST_OSS_ENDPOINT (plus AGENTTEAMS_TEST_OSS_ACCESS_KEY/_SECRET_KEY, and optionally _BUCKET, default agentteams-storage) to run the OSS benchmarks")
	}
	bucket := os.Getenv("AGENTTEAMS_TEST_OSS_BUCKET")
	if bucket == "" {
		bucket = "agentteams-storage"
	}
	return Config{
		Endpoint:  endpoint,
		AccessKey: os.Getenv("AGENTTEAMS_TEST_OSS_ACCESS_KEY"),
		SecretKey: os.Getenv("AGENTTEAMS_TEST_OSS_SECRET_KEY"),
		Bucket:    bucket,
	}
}

// benchPutObject streams `size` bytes to a unique key per iteration —
// the exact hot-path call the controller makes on worker artifact
// uploads.
func benchPutObject(b *testing.B, size int) {
	b.Helper()
	c := NewMinIOClient(ossBenchConfig(b))
	data := bytes.Repeat([]byte{0x5A}, size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench/putobject/%d/%d", size, i)
		if err := c.PutObject(b.Context(), key, data); err != nil {
			b.Fatalf("PutObject(%d): %v", size, err)
		}
	}
}

func BenchmarkPutObject256KB(b *testing.B) { benchPutObject(b, 256*1024) }

func BenchmarkPutObject1MB(b *testing.B) { benchPutObject(b, 1<<20) }
