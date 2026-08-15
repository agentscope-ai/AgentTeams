package oss

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestBuildMCHostEnv_FullURL(t *testing.T) {
	got, err := buildMCHostEnv("agentteams", "https://oss-cn-hangzhou.aliyuncs.com", Credentials{
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		SecurityToken:   "TOKEN",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "MC_HOST_agentteams=https://AK:SK:TOKEN@oss-cn-hangzhou.aliyuncs.com"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildMCHostEnv_BareHostname(t *testing.T) {
	got, err := buildMCHostEnv("agentteams", "oss-cn-hangzhou.aliyuncs.com", Credentials{
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		SecurityToken:   "TOKEN",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "MC_HOST_agentteams=https://AK:SK:TOKEN@oss-cn-hangzhou.aliyuncs.com"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildMCHostEnv_NoToken(t *testing.T) {
	got, err := buildMCHostEnv("agentteams", "oss-cn-hangzhou.aliyuncs.com", Credentials{
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "MC_HOST_agentteams=https://AK:SK@") {
		t.Fatalf("expected userinfo without token, got %q", got)
	}
}

func TestBuildMCHostEnv_EmptyEndpoint(t *testing.T) {
	if _, err := buildMCHostEnv("agentteams", "", Credentials{AccessKeyID: "AK", AccessKeySecret: "SK"}); err == nil {
		t.Fatalf("expected error for empty endpoint")
	}
}

// STS tokens from Alibaba Cloud are base64-style (A-Z, a-z, 0-9, +, /, =),
// which is safe inside URL userinfo without percent-encoding. mc (tested
// with RELEASE.2025-08-13) does not url-decode the userinfo segment, so
// any encoding we apply here leaks into the signed X-Amz-Security-Token
// header and breaks OSS auth. This test guards against accidentally
// reintroducing encoding.
func TestBuildMCHostEnv_NoPercentEncoding(t *testing.T) {
	got, err := buildMCHostEnv("agentteams", "https://oss-cn-hangzhou.aliyuncs.com", Credentials{
		AccessKeyID:     "STS.NYabc123",
		AccessKeySecret: "sk+with/slash=pad",
		SecurityToken:   "CAIS+Base64/Token==",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "MC_HOST_agentteams=https://STS.NYabc123:sk+with/slash=pad:CAIS+Base64/Token==@oss-cn-hangzhou.aliyuncs.com"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "%2F") || strings.Contains(got, "%2B") || strings.Contains(got, "%3D") {
		t.Fatalf("credentials must not be percent-encoded, got %q", got)
	}
}

func TestOSSFallback_ObservedConflictRejected(t *testing.T) {
	// OSS has no If-Match: the fallback must reject a conflict observed by
	// the pre-write ETag re-check, and must NOT perform the plain write.
	statCalls := 0
	putCalls := 0
	err := ossFallbackWrite(
		t.Context(), "projects/p1/meta.json", []byte("new"),
		"etag-read",
		func(_ context.Context, key string) (ObjectMeta, error) {
			statCalls++
			return ObjectMeta{ETag: "etag-changed-by-worker"}, nil
		},
		func(_ context.Context, key string, data []byte) error {
			putCalls++
			return nil
		},
	)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("err=%v, want ErrPreconditionFailed", err)
	}
	if statCalls != 1 {
		t.Fatalf("statCalls=%d, want 1", statCalls)
	}
	if putCalls != 0 {
		t.Fatalf("putCalls=%d, want 0 (conflict must not write)", putCalls)
	}
}

func TestOSSFallback_MatchingEtagPlainPut(t *testing.T) {
	// The fallback re-checks the ETag immediately before a PLAIN PutObject —
	// no If-Match header is involved on this path (the SDK conditional write
	// already returned NotImplemented and is not retried).
	statCalls := 0
	putCalls := 0
	var putData []byte
	err := ossFallbackWrite(
		t.Context(), "projects/p1/meta.json", []byte("new"),
		"etag-read",
		func(_ context.Context, key string) (ObjectMeta, error) {
			statCalls++
			return ObjectMeta{ETag: "etag-read"}, nil
		},
		func(_ context.Context, key string, data []byte) error {
			putCalls++
			putData = data
			return nil
		},
	)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if putCalls != 1 || string(putData) != "new" {
		t.Fatalf("putCalls=%d data=%q, want 1 call with the new payload", putCalls, putData)
	}
}

func TestOSSFallback_UppercaseQuotedMD5Matches(t *testing.T) {
	// OSS commonly returns the object MD5 ETag uppercase and quoted
	// ("A1B2..."); the read-bound matchETag is lowercase hex. The fallback
	// must canonicalize both sides so unchanged content is accepted.
	putCalls := 0
	err := ossFallbackWrite(
		t.Context(), "projects/p1/meta.json", []byte("new"),
		"a1b2c3d4e5f60718293a4b5c6d7e8f90",
		func(_ context.Context, key string) (ObjectMeta, error) {
			return ObjectMeta{ETag: "\"A1B2C3D4E5F60718293A4B5C6D7E8F90\""}, nil
		},
		func(_ context.Context, key string, data []byte) error {
			putCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("err=%v, want nil (quoted uppercase MD5 must match)", err)
	}
	if putCalls != 1 {
		t.Fatalf("putCalls=%d, want 1", putCalls)
	}
}

func TestOSSFallback_UppercaseUnquotedMD5Matches(t *testing.T) {
	// Same as above without the S3-style quotes.
	putCalls := 0
	err := ossFallbackWrite(
		t.Context(), "projects/p1/meta.json", []byte("new"),
		"a1b2c3d4e5f60718293a4b5c6d7e8f90",
		func(_ context.Context, key string) (ObjectMeta, error) {
			return ObjectMeta{ETag: "A1B2C3D4E5F60718293A4B5C6D7E8F90"}, nil
		},
		func(_ context.Context, key string, data []byte) error {
			putCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("err=%v, want nil (uppercase MD5 must match)", err)
	}
	if putCalls != 1 {
		t.Fatalf("putCalls=%d, want 1", putCalls)
	}
}

func TestOSSFallback_ChangedMD5Rejected(t *testing.T) {
	// A genuinely different MD5 (not just formatting) must still reject,
	// even when both sides canonicalize to the same case.
	putCalls := 0
	err := ossFallbackWrite(
		t.Context(), "projects/p1/meta.json", []byte("new"),
		"a1b2c3d4e5f60718293a4b5c6d7e8f90",
		func(_ context.Context, key string) (ObjectMeta, error) {
			return ObjectMeta{ETag: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"}, nil
		},
		func(_ context.Context, key string, data []byte) error {
			putCalls++
			return nil
		},
	)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("err=%v, want ErrPreconditionFailed", err)
	}
	if putCalls != 0 {
		t.Fatalf("putCalls=%d, want 0 (changed content must not write)", putCalls)
	}
}

func TestOSSFallback_StatFailurePropagates(t *testing.T) {
	// If the pre-write re-check cannot establish the current ETag, fail
	// closed — never write blindly.
	putCalls := 0
	err := ossFallbackWrite(
		t.Context(), "k", []byte("new"), "etag-read",
		func(_ context.Context, key string) (ObjectMeta, error) {
			return ObjectMeta{}, os.ErrNotExist
		},
		func(_ context.Context, key string, data []byte) error {
			putCalls++
			return nil
		},
	)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err=%v, want os.ErrNotExist", err)
	}
	if putCalls != 0 {
		t.Fatalf("putCalls=%d, want 0", putCalls)
	}
}

type failingCredSource struct{}

func (failingCredSource) Resolve(context.Context) (Credentials, error) {
	return Credentials{}, errors.New("sts unavailable")
}

func TestSDKClient_CredentialResolutionFailurePropagates(t *testing.T) {
	// A dynamic-STS resolution failure must be returned, never silently
	// replaced by the static credential pair.
	c := NewMinIOClient(Config{
		Endpoint:  "https://oss-cn-hangzhou.aliyuncs.com",
		AccessKey: "static-ak", SecretKey: "static-sk",
	})
	c.credSource = failingCredSource{}
	_, err := c.sdkClient()
	if err == nil || !strings.Contains(err.Error(), "resolve dynamic oss credentials") {
		t.Fatalf("err=%v, want dynamic-credential resolution error", err)
	}
}
