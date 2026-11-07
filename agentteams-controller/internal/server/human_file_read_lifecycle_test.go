package server

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// closeTrackingReader counts Close on a real ranged stream; the handler
// must release it after ServeContent (ServeContent does not close).
type closeTrackingReader struct {
	io.ReadCloser
	closed *int
}

func (r *closeTrackingReader) Close() error {
	*r.closed++
	return r.ReadCloser.Close()
}

type closeTrackingOSS struct {
	*ossfake.Memory
	closes int
}

func (c *closeTrackingOSS) OpenRange(ctx context.Context, key string, start, length int64, matchETag string) (io.ReadCloser, error) {
	rc, err := c.Memory.OpenRange(ctx, key, start, length, matchETag)
	if err != nil {
		return nil, err
	}
	return &closeTrackingReader{ReadCloser: rc, closed: &c.closes}, nil
}

func TestHumanFiles_GetClosesRangedReader(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "close.txt", hfSelf(), "close me"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	tracked := &closeTrackingOSS{Memory: store}
	hfh.oss = tracked

	rec := hfGetFile(t, hfh, "alice", "close.txt", http.MethodGet, hfSelf(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if tracked.closes == 0 {
		t.Fatal("ranged reader was never closed after a successful GET")
	}
}

// lazyFaultReader succeeds at OpenRange and fails on the first Read,
// exactly like a lazy minio-go object when S3 answers 503 to the GET.
type lazyFaultReader struct {
	err error
}

func (r *lazyFaultReader) Read([]byte) (int, error) { return 0, r.err }
func (r *lazyFaultReader) Close() error             { return nil }

type lazyFaultOSS struct {
	*ossfake.Memory
	err error
}

func (l *lazyFaultOSS) OpenRange(context.Context, string, int64, int64, string) (io.ReadCloser, error) {
	return &lazyFaultReader{err: l.err}, nil
}

func TestHumanFiles_LazyRangeReadFaultIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "lazy.txt", hfSelf(), "lazy"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	hfh.oss = &lazyFaultOSS{Memory: store, err: oss.ErrStorageUnavailable}

	rec := hfGetFile(t, hfh, "alice", "lazy.txt", http.MethodGet, hfSelf(), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET: got %d %s, want 503 storage_unavailable", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage_unavailable"}` {
		t.Fatalf("body = %q", got)
	}
}

// manifestRaceOSS injects a competing manifest version on the first GET
// after the HEAD, the exact HEAD-then-GET straddle the snapshot read must
// catch. The returned body must carry the validator of the injected
// version, not the preceding HEAD's ETag.
type manifestRaceOSS struct {
	*ossfake.Memory
	key   string
	next  []byte
	armed bool
}

func (m *manifestRaceOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	if m.armed && key == m.key {
		m.armed = false
		if err := m.Memory.PutObject(ctx, key, m.next); err != nil {
			return nil, err
		}
		return append([]byte(nil), m.next...), nil
	}
	return m.Memory.GetObject(ctx, key)
}

func TestHumanSync_MetadataBodyMatchesValidator(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)
	bodyV1 := syncManifestBody([]string{"files/a.txt"}, nil)
	rec := hfPutSync(t, hfh, "alice", alice, bodyV1, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	v2 := []byte(`{"version":1,"generation":2,"entries":{},"tombstones":[]}`)
	race := &manifestRaceOSS{
		Memory: store,
		key:    humanSyncManifestKey("alice"),
		next:   v2,
		armed:  true,
	}
	hfh.oss = race

	req := hfSyncReq(t, http.MethodGet, "alice", "manifest", alice, "")
	got := httptest.NewRecorder()
	hfh.getSyncManifest(got, req)
	if got.Code != http.StatusOK {
		t.Fatalf("manifest GET: %d %s", got.Code, got.Body.String())
	}
	wantETag := fmt.Sprintf(`"%x"`, md5.Sum(v2))
	if h := got.Header().Get("ETag"); h != wantETag {
		t.Fatalf("ETag = %q, want the body's validator %q", h, wantETag)
	}
	if got.Body.String() != string(v2) {
		t.Fatalf("body = %q, want the injected version", got.Body.String())
	}
}
