package oss

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// statS3Server stands up the minimal S3 surface the SDK stat/read path
// needs: the one-time signed location lookup, then either a 404 for
// every object op (missing) or the canned object — a zero-byte tombstone
// with the metadata Content-Type the human-files protocol depends on.
func statS3Server(t *testing.T, missing bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if missing {
			// A 404 with the NoSuchKey error body: the SDK maps it to
			// minio.ErrorResponse{Code:"NoSuchKey", StatusCode:404}.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`))
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
			w.Header().Set("Content-Type", "application/vnd.agentteams.tombstone")
			w.Header().Set("Content-Length", "0")
			w.Header().Set("Last-Modified", "Fri, 30 Oct 2026 21:00:00 GMT")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Last-Modified", "Fri, 30 Oct 2026 21:00:00 GMT")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("hello-s3"))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func statClient(srv *httptest.Server) *MinIOClient {
	return NewMinIOClient(Config{
		Endpoint:  srv.URL,
		AccessKey: "access-key",
		SecretKey: "secret-key",
		Bucket:    "bucket",
	})
}

func TestStatMetaReadsS3ResponseHeaders(t *testing.T) {
	srv := statS3Server(t, false)
	c := statClient(srv)

	meta, err := c.StatMeta(context.Background(), "humans/alice/files/tomb.json")
	if err != nil {
		t.Fatal(err)
	}
	// The tombstone marker the protocol keys off...
	if meta.ContentType != "application/vnd.agentteams.tombstone" {
		t.Fatalf("ContentType = %q, want the tombstone marker", meta.ContentType)
	}
	// ...plus the unquoted ETag (the SDK strips S3's quotes, the same form
	// the mc path returned) and the second-precision last-modified.
	if meta.ETag != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("ETag = %q, want the unquoted ETag", meta.ETag)
	}
	if meta.Size != 0 {
		t.Fatalf("Size = %d, want 0", meta.Size)
	}
	wantTime := time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC)
	if !meta.ModTime.Equal(wantTime) {
		t.Fatalf("ModTime = %v, want %v", meta.ModTime, wantTime)
	}
}

func TestStatAndStatMeta404MapToNotExist(t *testing.T) {
	srv := statS3Server(t, true)
	c := statClient(srv)

	if err := c.Stat(context.Background(), "humans/alice/files/missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat err = %v, want os.ErrNotExist", err)
	}
	if _, err := c.StatMeta(context.Background(), "humans/alice/files/missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("StatMeta err = %v, want os.ErrNotExist", err)
	}
}

func TestGetObject404MapsToNotExist(t *testing.T) {
	srv := statS3Server(t, true)
	c := statClient(srv)

	_, err := c.GetObject(context.Background(), "humans/alice/files/missing.txt")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GetObject err = %v, want os.ErrNotExist", err)
	}
}

func TestGetObjectReturnsBodyVerbatim(t *testing.T) {
	srv := statS3Server(t, false)
	c := statClient(srv)

	// A space-bearing key: the wire form must be URL-encoded, the returned
	// body the stored bytes verbatim.
	data, err := c.GetObject(context.Background(), "humans/alice/files/meeting notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello-s3" {
		t.Fatalf("body = %q, want the stored bytes verbatim", data)
	}
}

// TestGetObjectRequestsAreURLPercentEncoded pins the wire encoding of a
// space-bearing key (the maintainer's repro shape): the request target
// carries %20, never a literal space.
func TestGetObjectRequestsAreURLPercentEncoded(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		gotURI = r.RequestURI
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Last-Modified", "Fri, 30 Oct 2026 21:00:00 GMT")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello-s3"))
	}))
	defer srv.Close()
	c := statClient(srv)

	if _, err := c.GetObject(context.Background(), "humans/alice/files/meeting notes.txt"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotURI, "meeting%20notes.txt") {
		t.Fatalf("request URI = %q, want the percent-encoded key", gotURI)
	}
	if strings.Contains(gotURI, " ") {
		t.Fatalf("request URI = %q, must not carry a literal space", gotURI)
	}
}
