package oss

import (
	"errors"
	"strings"
	"testing"
)

func TestBuildMCHostEnv_FullURL(t *testing.T) {
	got, err := buildMCHostEnv("hiclaw", "https://oss-cn-hangzhou.aliyuncs.com", Credentials{
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		SecurityToken:   "TOKEN",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "MC_HOST_hiclaw=https://AK:SK:TOKEN@oss-cn-hangzhou.aliyuncs.com"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildMCHostEnv_BareHostname(t *testing.T) {
	got, err := buildMCHostEnv("hiclaw", "oss-cn-hangzhou.aliyuncs.com", Credentials{
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		SecurityToken:   "TOKEN",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "MC_HOST_hiclaw=https://AK:SK:TOKEN@oss-cn-hangzhou.aliyuncs.com"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildMCHostEnv_NoToken(t *testing.T) {
	got, err := buildMCHostEnv("hiclaw", "oss-cn-hangzhou.aliyuncs.com", Credentials{
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "MC_HOST_hiclaw=https://AK:SK@") {
		t.Fatalf("expected userinfo without token, got %q", got)
	}
}

func TestBuildMCHostEnv_EmptyEndpoint(t *testing.T) {
	if _, err := buildMCHostEnv("hiclaw", "", Credentials{AccessKeyID: "AK", AccessKeySecret: "SK"}); err == nil {
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
	got, err := buildMCHostEnv("hiclaw", "https://oss-cn-hangzhou.aliyuncs.com", Credentials{
		AccessKeyID:     "STS.NYabc123",
		AccessKeySecret: "sk+with/slash=pad",
		SecurityToken:   "CAIS+Base64/Token==",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "MC_HOST_hiclaw=https://STS.NYabc123:sk+with/slash=pad:CAIS+Base64/Token==@oss-cn-hangzhou.aliyuncs.com"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "%2F") || strings.Contains(got, "%2B") || strings.Contains(got, "%3D") {
		t.Fatalf("credentials must not be percent-encoded, got %q", got)
	}
}

// isNotExistErr must only treat mc's genuine not-found marker as
// "not exist". Generic exec failures (unreachable endpoint, bad/expired
// credentials, timeouts) all surface as "exit status N" from runMC and must
// NOT be collapsed into os.ErrNotExist, or real outages get masked and
// silently swallowed by callers that treat not-found as a non-error.
func TestIsNotExistErr_GenuineNotFound(t *testing.T) {
	err := errors.New(`mc cat hiclaw/bucket/key: exit status 1 (stderr: mc: <ERROR> Object does not exist.)`)
	if !isNotExistErr(err) {
		t.Fatalf("expected genuine not-found error to be recognized, got false for %v", err)
	}
}

func TestIsNotExistErr_ConnectionFailureNotMasked(t *testing.T) {
	err := errors.New(`mc cat hiclaw/bucket/key: exit status 1 (stderr: mc: <ERROR> Unable to initialize new alias from the provided credentials.)`)
	if isNotExistErr(err) {
		t.Fatalf("connection/auth failure must not be treated as not-exist, got true for %v", err)
	}
}

func TestIsNotExistErr_GenericExitStatusNotMasked(t *testing.T) {
	err := errors.New("exit status 1")
	if isNotExistErr(err) {
		t.Fatalf("bare exit status must not be treated as not-exist, got true for %v", err)
	}
}
