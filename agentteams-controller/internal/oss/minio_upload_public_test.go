package oss

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUploadRequiresPublicOrigin(t *testing.T) {
	c := NewMinIOClient(Config{Endpoint: "http://127.0.0.1:9", AccessKey: "ak", SecretKey: "sk", Bucket: "bucket"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _, err := c.PresignUpload(ctx, "humans/alice/.uploads/a/file.txt", 64, "text/plain", "", time.Minute)
	if !errors.Is(err, ErrPresignUnsupported) {
		t.Fatalf("no configured public origin: err=%v, want unsupported", err)
	}
}

type uploadCredentials struct{ calls int }

func (c *uploadCredentials) Resolve(context.Context) (Credentials, error) {
	c.calls++
	return Credentials{AccessKeyID: fmt.Sprintf("temporary%d", c.calls), AccessKeySecret: "owned-test-secret", SecurityToken: fmt.Sprintf("token%d", c.calls)}, nil
}

func TestUploadSignsPublicOriginAndRefreshesCredentials(t *testing.T) {
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
	}))
	defer public.Close()
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
	}))
	defer internal.Close()
	creds := &uploadCredentials{}
	c := NewMinIOClient(Config{Endpoint: internal.URL, PublicEndpoint: public.URL, AccessKey: "static", SecretKey: "static", Bucket: "bucket"}).WithCredentialSource(creds)
	key := "humans/alice/.uploads/a/file.txt"
	for i := 1; i <= 2; i++ {
		method, signed, fields, err := c.PresignUpload(context.Background(), key, 64, "text/plain", "", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if method != "POST" || !strings.HasPrefix(signed, public.URL+"/") || fields["key"] != key {
			t.Fatalf("recipe must sign public origin and relative key: method=%s url=%s key=%s", method, signed, fields["key"])
		}
		if !strings.HasPrefix(fields["x-amz-credential"], fmt.Sprintf("temporary%d/", i)) || fields["x-amz-security-token"] != fmt.Sprintf("token%d", i) {
			t.Fatalf("recipe did not resolve current temporary identity on issuance %d", i)
		}
	}
}
