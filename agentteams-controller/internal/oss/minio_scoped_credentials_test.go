package oss

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMissingObjectWithPrefixScopedBucketPermission(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		objectStatus, listStatus int
		listCode                 string
		wantMissing              bool
	}{
		{"allowed_prefix", 404, 200, "", true},
		{"prefix_listing_denied", 404, 403, "AccessDenied", false},
		{"bucket_gone", 404, 404, "NoSuchBucket", false},
		{"object_access_denied", 403, 200, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lists atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := strings.Trim(r.URL.Path, "/")
				switch {
				case r.URL.Query().Has("location"):
					_, _ = fmt.Fprint(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
				case path == "test" && r.Method == http.MethodHead:
					w.WriteHeader(http.StatusForbidden)
				case path == "test" && r.URL.Query().Get("list-type") == "2":
					lists.Add(1)
					if r.URL.Query().Get("prefix") != "scope/missing.txt" {
						t.Errorf("bucket probe escaped key prefix: %s", r.URL.RawQuery)
					}
					if tc.listStatus != 200 {
						w.WriteHeader(tc.listStatus)
						_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>test listing rejected</Message></Error>`, tc.listCode)
						return
					}
					_, _ = fmt.Fprint(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>test</Name><Prefix>scope/missing.txt</Prefix><MaxKeys>1</MaxKeys><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`)
				case path == "test/scope/missing.txt":
					w.WriteHeader(tc.objectStatus)
					code := "NoSuchKey"
					if tc.objectStatus == 403 {
						code = "AccessDenied"
					}
					_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>test object missing or denied</Message></Error>`, code)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()
			c := NewMinIOClient(Config{Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk", Bucket: "test", StoragePrefix: "test"})
			for _, read := range []string{"get", "stat", "stat-meta"} {
				var err error
				switch read {
				case "get":
					_, err = c.GetObject(context.Background(), "scope/missing.txt")
				case "stat":
					err = c.Stat(context.Background(), "scope/missing.txt")
				case "stat-meta":
					_, err = c.StatMeta(context.Background(), "scope/missing.txt")
				}
				if err == nil || errors.Is(err, os.ErrNotExist) != tc.wantMissing {
					t.Errorf("%s: %v, wantMissing=%v", read, err, tc.wantMissing)
				}
			}
			if tc.wantMissing && lists.Load() == 0 {
				t.Error("missing object did not use its permitted prefix probe")
			}
			if tc.objectStatus == 403 && lists.Load() != 0 {
				t.Error("object authorization failure must not become a missing-object probe")
			}
		})
	}
}
