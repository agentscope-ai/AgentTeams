package oss

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// rangeWireServer models the S3 range surface the SDK talks to: a location
// lookup, the bucket HEAD, then one object GET. It records the Range header
// so the test can assert the inclusive end conversion, and returns 206 with
// exactly the requested bytes.
func rangeWireServer(t *testing.T, payload []byte) (*httptest.Server, *string) {
	t.Helper()
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if r.Method == http.MethodHead && strings.Trim(r.URL.Path, "/") == "bucket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		gotRange = r.Header.Get("Range")
		start, end, ok := parseTestRange(gotRange, int64(len(payload)))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("Last-Modified", "Fri, 30 Oct 2026 21:00:00 GMT")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv, &gotRange
}

func parseTestRange(h string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 || parts[0] == "" {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if parts[1] == "" {
		return start, size - 1, true
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < start || end >= size {
		return 0, 0, false
	}
	return start, end, true
}

func TestOpenRangeConvertsLengthToInclusiveEnd(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 64)
	srv, gotRange := rangeWireServer(t, payload)
	c := NewMinIOClient(Config{
		Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk", Bucket: "bucket",
	})

	cases := []struct {
		name       string
		start, len int64
		wantRange  string
		wantLen    int
	}{
		{"twenty-byte window", 10, 20, "bytes=10-29", 20},
		{"tail window", 40, 5, "bytes=40-44", 5},
		{"open-ended", 10, -1, "bytes=10-", 54},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := c.OpenRange(context.Background(), "obj", tc.start, tc.len, "etag")
			if err != nil {
				t.Fatalf("OpenRange: %v", err)
			}
			defer rc.Close()
			body, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if *gotRange != tc.wantRange {
				t.Fatalf("Range header = %q, want %q", *gotRange, tc.wantRange)
			}
			if len(body) != tc.wantLen {
				t.Fatalf("body length = %d, want %d", len(body), tc.wantLen)
			}
			if !bytes.Equal(body, payload[tc.start:tc.start+int64(tc.wantLen)]) {
				t.Fatal("body is not the requested range")
			}
		})
	}
}

// TestOpenRangeLazyReadErrorClassified pins the second half of the SDK
// contract: GetObject returns a lazy object, so a 503 can surface on Read
// rather than OpenRange. The wrapper must classify it as retryable
// ErrStorageUnavailable, not let a raw SDK error escape.
func TestOpenRangeLazyReadErrorClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if r.Method == http.MethodHead && strings.Trim(r.URL.Path, "/") == "bucket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`<Error><Code>ServiceUnavailable</Code><Message>down</Message></Error>`))
	}))
	defer srv.Close()
	c := NewMinIOClient(Config{
		Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk", Bucket: "bucket",
	})
	rc, err := c.OpenRange(context.Background(), "obj", 0, 4, "etag")
	if err != nil {
		// Some minio-go versions still surface the first error eagerly;
		// that is the same contract, just an earlier point.
		if !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("OpenRange error = %v, want ErrStorageUnavailable", err)
		}
		return
	}
	defer rc.Close()
	if _, err := rc.Read(make([]byte, 1)); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("Read error = %v, want ErrStorageUnavailable", err)
	}
}
