package oss

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestClassifySDKError pins the generic (non-key-scoped) PR-0
// classification contract. Missing-object normalization lives in
// classifyKeyError, which must probe the bucket first; this gate only
// turns store-availability faults into ErrStorageUnavailable and leaves
// every semantic answer unchanged.
func TestClassifySDKError(t *testing.T) {
	notExist := minio.ErrorResponse{Code: "NoSuchKey", StatusCode: 404}
	cases := []struct {
		name string
		err  error
		want string // "notexist", "unavailable", or "passthrough"
	}{
		// Class 1: object absent -> os.ErrNotExist (the 404 signal).
		{"nosuchkey", notExist, "passthrough"},
		{"nosuchobject", minio.ErrorResponse{Code: "NoSuchObject", StatusCode: 404}, "passthrough"},
		{"notfound code", minio.ErrorResponse{Code: "NotFound", StatusCode: 404}, "passthrough"},
		{"head 404 empty code", minio.ErrorResponse{StatusCode: 404}, "passthrough"},
		{"nosuchkey wrapped", fmt.Errorf("outer: %w", notExist), "passthrough"},

		// Class 2: bucket-level / configuration 404s and auth faults stay
		// ordinary errors (a 500, never a 404 or a 503).
		{"nosuchbucket", minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: 404}, "passthrough"},
		{"nosuchupload", minio.ErrorResponse{Code: "NoSuchUpload", StatusCode: 404}, "passthrough"},
		{"nosuchversion", minio.ErrorResponse{Code: "NoSuchVersion", StatusCode: 404}, "passthrough"},
		{"accessdenied", minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, "passthrough"},
		{"invalidaccesskeyid", minio.ErrorResponse{Code: "InvalidAccessKeyId", StatusCode: 403}, "passthrough"},
		{"signaturedoesnotmatch", minio.ErrorResponse{Code: "SignatureDoesNotMatch", StatusCode: 403}, "passthrough"},
		{"other 4xx", minio.ErrorResponse{Code: "InvalidArgument", StatusCode: 400}, "passthrough"},
		{"409 conflict", minio.ErrorResponse{Code: "BucketAlreadyExists", StatusCode: 409}, "passthrough"},
		{"canceled", context.Canceled, "passthrough"},
		{"plain error", errors.New("generic failure"), "passthrough"},

		// Class 3: the store did not answer -> ErrStorageUnavailable.
		{"internalerror 500", minio.ErrorResponse{Code: "InternalError", StatusCode: 500}, "unavailable"},
		{"serviceunavailable 503", minio.ErrorResponse{Code: "ServiceUnavailable", StatusCode: 503}, "unavailable"},
		{"slowdown 503", minio.ErrorResponse{Code: "SlowDown", StatusCode: 503}, "unavailable"},
		{"badgateway 502", minio.ErrorResponse{Code: "InternalError", StatusCode: 502}, "unavailable"},
		{"requesttimeout 400", minio.ErrorResponse{Code: "RequestTimeout", StatusCode: 400}, "unavailable"},
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, "unavailable"},
		{"connection reset", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, "unavailable"},
		{"eof mid-body", io.EOF, "unavailable"},
		{"unexpected eof", io.ErrUnexpectedEOF, "unavailable"},
		{"conn closed", net.ErrClosed, "unavailable"},
		{"context deadline", context.DeadlineExceeded, "unavailable"},
		{"refused wrapped", fmt.Errorf("dial tcp 127.0.0.1:9000: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}), "unavailable"},
		{"5xx wrapped in url error", &urlError{Err: minio.ErrorResponse{Code: "InternalError", StatusCode: 500}}, "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySDKError(tc.err)
			switch tc.want {
			case "notexist":
				if !errors.Is(got, os.ErrNotExist) {
					t.Fatalf("classify(%v) = %v, want os.ErrNotExist", tc.err, got)
				}
				if errors.Is(got, ErrStorageUnavailable) {
					t.Fatalf("classify(%v) must not also be storage-unavailable: %v", tc.err, got)
				}
			case "unavailable":
				if !errors.Is(got, ErrStorageUnavailable) {
					t.Fatalf("classify(%v) = %v, want ErrStorageUnavailable", tc.err, got)
				}
			case "passthrough":
				if errors.Is(got, os.ErrNotExist) {
					t.Fatalf("classify(%v) must not be os.ErrNotExist: %v", tc.err, got)
				}
				if errors.Is(got, ErrStorageUnavailable) {
					t.Fatalf("classify(%v) must not be ErrStorageUnavailable: %v", tc.err, got)
				}
				if got != tc.err {
					t.Fatalf("classify(%v) = %v, want the error returned unchanged", tc.err, got)
				}
			}
		})
	}
}

// urlError is the *url.Error shape the SDK wraps request faults in: a
// net.Error (Timeout false here) carrying the underlying fault.
type urlError struct {
	Op  string
	Err error
}

func (e *urlError) Error() string   { return e.Op + ": " + e.Err.Error() }
func (e *urlError) Timeout() bool   { return false }
func (e *urlError) Temporary() bool { return false }
func (e *urlError) Unwrap() error   { return e.Err }

// TestClassifySDKErrorPreservesTheCause pins the %w contract: the original
// fault stays reachable through the storage-unavailable wrap, so callers
// (and logs) can still see what actually happened.
func TestClassifySDKErrorPreservesTheCause(t *testing.T) {
	resp := minio.ErrorResponse{Code: "InternalError", StatusCode: 500}
	got := classifySDKError(&urlError{Op: "Get", Err: resp})

	var asResp minio.ErrorResponse
	if !errors.As(got, &asResp) || asResp.Code != "InternalError" {
		t.Fatalf("cause not recoverable via errors.As: %v", got)
	}
	var netErr net.Error
	if !errors.As(got, &netErr) {
		t.Fatalf("the transport fault must stay reachable as net.Error: %v", got)
	}
}

// TestClassifySDKErrorIsIdempotent pins that an already-classified error
// passes through unchanged, so wrapping a classified error in a message and
// re-classifying it (or classifying twice) never changes its class.
func TestClassifySDKErrorIsIdempotent(t *testing.T) {
	if got := classifySDKError(nil); got != nil {
		t.Fatalf("classify(nil) = %v, want nil", got)
	}
	if got := classifySDKError(os.ErrNotExist); got != os.ErrNotExist {
		t.Fatalf("classify(os.ErrNotExist) = %v, want it unchanged", got)
	}
	wrapped := fmt.Errorf("read body: %w", classifySDKError(io.EOF))
	if !errors.Is(wrapped, ErrStorageUnavailable) {
		t.Fatalf("wrapped classify(io.EOF) = %v, want ErrStorageUnavailable", wrapped)
	}
	if got := classifySDKError(wrapped); got != wrapped {
		t.Fatalf("re-classify(%v) = %v, want the same error back", wrapped, got)
	}
}
