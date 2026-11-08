// Package ossfake provides in-memory fakes of the oss.StorageClient interface
// for use in unit and integration tests that exercise code paths dependent on
// object storage (package handler uploads, runtime configuration, etc.).
//
// The Memory client stores objects in a map keyed by their full object path.
// Paths are treated as opaque strings — there is no bucket/prefix logic, so
// tests see the exact keys that the production code passes in.
package ossfake

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"io"
	"sync/atomic"

	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// Memory is an in-memory implementation of oss.StorageClient suitable for
// tests. All methods are safe for concurrent use.
type Memory struct {
	mu           sync.RWMutex
	objects      map[string][]byte
	contentTypes map[string]string
	metadata     map[string]map[string]string
	modTime      time.Time
	next         int64

	// StrictUnsupported makes PutObjectIfMatchStrict answer 501, for tests.
	StrictUnsupported atomic.Bool
	// StatMetaCalls counts StatMeta invocations so tests can assert that
	// the list paths no longer stat every file (only zero-byte candidates).
	StatMetaCalls atomic.Int64
	// OpenRangeCalls counts OpenRange invocations so tests can assert the
	// single-range-read cost (one ranged open per requested range).
	OpenRangeCalls atomic.Int64
	// GetObjectCalls counts GetObject invocations so tests can assert the
	// metadata fast path answered from StatMeta alone (zero GetObject).
	GetObjectCalls atomic.Int64
	// MetadataWritesFail makes the *WithMetadata writes fail with a
	// non-conflict error, for tests of the caller's plain-write fallback.
	MetadataWritesFail atomic.Bool
}

// md5Hex returns the lowercase MD5 hex digest of data (MinIO single-part
// ETag semantics).
func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// NewMemory constructs an empty in-memory storage client.
func NewMemory() *Memory {
	return &Memory{
		objects:      make(map[string][]byte),
		contentTypes: make(map[string]string),
		metadata:     make(map[string]map[string]string),
		modTime:      time.Unix(1700000000, 0),
	}
}

// PutObject stores data under key. Like a S3 PUT without x-amz-meta-*
// headers, it replaces the object AND clears its user metadata.
func (m *Memory) PutObject(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[key] = buf
	delete(m.contentTypes, key)
	delete(m.metadata, key)
	m.modTime = m.modTime.Add(time.Second)
	m.next++
	return nil
}

// PutObjectIfMatchWithType stores data with an explicit content type when the
// current ETag matches, mirroring the typed conditional write.
func (m *Memory) PutObjectIfMatchWithType(_ context.Context, key string, data []byte, contentType, matchETag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.objects[key]
	if !ok {
		// S3 semantics: If-Match against a missing object fails.
		return oss.ErrPreconditionFailed
	}
	if matchETag == "" || md5Hex(cur) != matchETag {
		return oss.ErrPreconditionFailed
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[key] = buf
	m.contentTypes[key] = contentType
	delete(m.metadata, key)
	m.modTime = m.modTime.Add(time.Second)
	m.next++
	return nil
}

// PutFile reads a local file and stores its contents under key.
func (m *Memory) PutFile(ctx context.Context, localPath, key string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", localPath, err)
	}
	return m.PutObject(ctx, key, data)
}

// GetObject returns the bytes stored under key. Returns os.ErrNotExist when
// the key is missing to match the production MinIO client's behavior.
func (m *Memory) GetObject(_ context.Context, key string) ([]byte, error) {
	m.GetObjectCalls.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// Stat returns nil when key exists, os.ErrNotExist otherwise. ManagerConfigStore
// relies on errors.Is(err, os.ErrNotExist) to detect first-time writes.
func (m *Memory) Stat(_ context.Context, key string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.objects[key]; !ok {
		return os.ErrNotExist
	}
	return nil
}

// LastWriteTime returns the fake's global write clock (the mtime that
// ListObjectsDetailed reports for every entry).
func (m *Memory) LastWriteTime() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.modTime
}

// OpenRange returns the bytes of [start, start+length) under key (or
// [start, end) when length < 0), S3-faithful:
//
//   - missing key                      -> os.ErrNotExist;
//   - matchETag set and not the current MD5 -> oss.ErrPreconditionFailed;
//   - start >= size (the range cannot fit)  -> oss.ErrRangeNotSatisfiable.
//
// The MD5 bound is what lets a stat-then-open caller detect a concurrent
// writer between its StatMeta and its ranged open, exactly like the
// provider's If-Match on a Range GET. Every open increments OpenRangeCalls.
func (m *Memory) OpenRange(_ context.Context, key string, start, length int64, matchETag string) (io.ReadCloser, error) {
	m.OpenRangeCalls.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	if matchETag != "" && md5Hex(data) != matchETag {
		return nil, oss.ErrPreconditionFailed
	}
	size := int64(len(data))
	if start < 0 || start >= size {
		return nil, oss.ErrRangeNotSatisfiable
	}
	end := size
	if length > 0 {
		end = start + length
		if end > size {
			end = size
		}
	}
	out := make([]byte, end-start)
	copy(out, data[start:end])
	return io.NopCloser(bytes.NewReader(out)), nil
}

// StatMeta returns a monotonic mtime for the object. Writes advance the clock,
// so a test can verify the optimistic-lock conflict path by writing after a
// read. The ETag is the content MD5 (mirroring MinIO single-part semantics),
// so a content-changing concurrent write also changes the ETag. Returns
// os.ErrNotExist when the key is missing.
func (m *Memory) StatMeta(_ context.Context, key string) (oss.ObjectMeta, error) {
	m.StatMetaCalls.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.objects[key]
	if !ok {
		return oss.ObjectMeta{}, os.ErrNotExist
	}
	var userMeta map[string]string
	if raw := m.metadata[key]; len(raw) > 0 {
		userMeta = make(map[string]string, len(raw))
		for k, v := range raw {
			userMeta[k] = v
		}
	}
	return oss.ObjectMeta{
		Size:         int64(len(data)),
		ModTime:      m.modTime,
		ETag:         md5Hex(data),
		ContentType:  m.contentTypes[key],
		UserMetadata: userMeta,
	}, nil
}

// PutObjectIfAbsent stores data only when the key does not exist. The check
// and write happen under one lock so two handlers sharing a store cannot both
// win; this mirrors the storage-level If-None-Match used by the real client.
func (m *Memory) PutObjectIfAbsent(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; ok {
		return oss.ErrPreconditionFailed
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[key] = buf
	delete(m.metadata, key)
	m.modTime = m.modTime.Add(time.Second)
	m.next++
	return nil
}

// PutObjectIfMatch writes only when the current object ETag equals matchETag.
// Mirrors the MinIO conditional-write semantics: mismatch returns
// oss.ErrPreconditionFailed and leaves the object untouched.
func (m *Memory) PutObjectIfMatch(_ context.Context, key string, data []byte, matchETag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.objects[key]
	if !ok {
		// S3 semantics: If-Match against a missing object fails.
		return oss.ErrPreconditionFailed
	}
	if matchETag == "" || md5Hex(cur) != matchETag {
		return oss.ErrPreconditionFailed
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[key] = buf
	delete(m.metadata, key)
	m.modTime = m.modTime.Add(time.Second)
	m.next++
	return nil
}

// PutObjectIfMatchWithMetadata is PutObjectIfMatch with the object's user
// metadata set in the same operation (the S3 conditional PUT carries the
// x-amz-meta-* headers). metadata keys are normalized to bare lowercase
// keys (the x-amz-meta- prefix is stripped, matching what the S3 backends
// report on read); empty keys or values are dropped. A failed
// precondition leaves the object — and its metadata — untouched. With
// MetadataWritesFail set it fails with a non-conflict error, for tests of
// the caller's plain-write fallback.
func (m *Memory) PutObjectIfMatchWithMetadata(_ context.Context, key string, data []byte, contentType, matchETag string, metadata map[string]string) error {
	if m.MetadataWritesFail.Load() {
		return errors.New("fake: metadata write rejected")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.objects[key]
	if !ok {
		// S3 semantics: If-Match against a missing object fails.
		return oss.ErrPreconditionFailed
	}
	if matchETag == "" || md5Hex(cur) != matchETag {
		return oss.ErrPreconditionFailed
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[key] = buf
	m.contentTypes[key] = contentType
	if meta := normalizeFakeMetadata(metadata); meta != nil {
		m.metadata[key] = meta
	} else {
		delete(m.metadata, key)
	}
	m.modTime = m.modTime.Add(time.Second)
	m.next++
	return nil
}

// PutObjectIfAbsentWithMetadata is PutObjectIfAbsent with the object's
// user metadata set in the same operation. An existing key returns
// oss.ErrPreconditionFailed and is left untouched. metadata handling and
// MetadataWritesFail behave as in PutObjectIfMatchWithMetadata.
func (m *Memory) PutObjectIfAbsentWithMetadata(_ context.Context, key string, data []byte, contentType string, metadata map[string]string) error {
	if m.MetadataWritesFail.Load() {
		return errors.New("fake: metadata write rejected")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; ok {
		return oss.ErrPreconditionFailed
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[key] = buf
	m.contentTypes[key] = contentType
	if meta := normalizeFakeMetadata(metadata); meta != nil {
		m.metadata[key] = meta
	} else {
		delete(m.metadata, key)
	}
	m.modTime = m.modTime.Add(time.Second)
	m.next++
	return nil
}

// normalizeFakeMetadata mirrors the production key normalization (bare
// lowercase keys, x-amz-meta- prefix stripped, empty keys or values
// dropped) so a write through the fake round-trips through StatMeta in
// the same shape the S3 backends report.
func normalizeFakeMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]string, len(metadata))
	for k, v := range metadata {
		nk := normalizeFakeMetaKey(k)
		if nk == "" || v == "" {
			continue
		}
		out[nk] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeFakeMetaKey(k string) string {
	return strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-")
}

// PutObjectIfMatchStrict is the strict conditional write (see
// MinIOClient.PutObjectIfMatchStrict). In memory it is identical to
// PutObjectIfMatch — the CAS already runs under the lock and never
// degrades; the method exists so the fake advertises the conflict-safety
// guarantee that a degraded backend (compare-then-put) does not offer.
// With StrictUnsupported set it models a 501 provider and returns
// oss.ErrConditionalUnsupported without writing.
func (m *Memory) PutObjectIfMatchStrict(ctx context.Context, key string, data []byte, matchETag string) error {
	if m.StrictUnsupported.Load() {
		return oss.ErrConditionalUnsupported
	}
	return m.PutObjectIfMatch(ctx, key, data, matchETag)
}

// PutObjectIfNoneMatchStrict is the strict create-if-absent write (see
// MinIOClient.PutObjectIfNoneMatchStrict): the object is created only when
// it does not exist; an existing object returns oss.ErrPreconditionFailed
// and is left untouched — a concurrent create that landed in between the
// caller's absence probe and the write must surface as a conflict, never
// as a clobber.
func (m *Memory) PutObjectIfNoneMatchStrict(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; ok {
		return oss.ErrPreconditionFailed
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[key] = buf
	m.modTime = m.modTime.Add(time.Second)
	m.next++
	return nil
}

// DeleteObject removes the object stored under key. Deleting a missing key
// is a no-op.
func (m *Memory) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// Mirror copies every object under src to dst by swapping the src prefix for
// dst. src may be an in-memory prefix or a local directory ("/" prefix,
// walked on disk — matching the minio backend's local-src semantics).
// MirrorOptions.Exclude is currently ignored; Overwrite is implicit (existing
// destination keys are replaced). With Remove=true, destination objects with
// no counterpart at source are deleted (mc --remove, exact-copy semantics).
func (m *Memory) Mirror(_ context.Context, src, dst string, opts oss.MirrorOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	src = strings.TrimSuffix(src, "/")
	dst = strings.TrimSuffix(dst, "/")

	srcObjects := make(map[string][]byte)
	if strings.HasPrefix(src, "/") {
		// Local directory source.
		if err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, rerr := filepath.Rel(src, path)
			if rerr != nil {
				return rerr
			}
			data, derr := os.ReadFile(path)
			if derr != nil {
				return derr
			}
			// rel carries a leading slash (or is "" for the marker),
			// uniformly, so newKey = dst + rel in both source kinds.
			srcObjects["/"+filepath.ToSlash(rel)] = data
			return nil
		}); err != nil {
			return err
		}
	} else {
		for key, data := range m.objects {
			if key != src && !strings.HasPrefix(key, src+"/") {
				continue
			}
			// TrimPrefix leaves the leading "/" (or "" for the marker).
			srcObjects[strings.TrimPrefix(key, src)] = data
		}
	}

	written := make(map[string]struct{}, len(srcObjects))
	for rel, data := range srcObjects {
		newKey := dst + rel
		buf := make([]byte, len(data))
		copy(buf, data)
		m.objects[newKey] = buf
		written[newKey] = struct{}{}
	}

	if opts.Remove {
		for key := range m.objects {
			if key == dst || strings.HasPrefix(key, dst+"/") {
				if _, ok := written[key]; !ok {
					delete(m.objects, key)
				}
			}
		}
	}
	return nil
}

// ListObjects returns every object under prefix, sorted, as names RELATIVE
// to the prefix — mirroring the production MinIOClient, whose ListObjects
// wraps `mc ls <prefix>` and reports the bare child name of each output
// line ("[date] [size] <name>"). Callers that need a full object key must
// re-attach the prefix themselves (see project_handler's
// metaKeyFromListResult); passing a listed name straight to GetObject reads
// the bucket root instead of the prefixed path.
func (m *Memory) ListObjects(_ context.Context, prefix string) ([]string, error) {
	infos, err := m.ListObjectsDetailed(context.Background(), prefix)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.Name)
	}
	return out, nil
}

// ListObjectsDetailed reports the fake's single global write clock as every
// entry's UpdatedAt (the fake has no per-object mtime; writes advance the
// clock, so the value reflects the most recent write). Entry names are
// RELATIVE to prefix, matching the production `mc ls` contract — the
// previous full-key behavior let callers skip the prefix re-attach that
// GetObject requires, silently reading the bucket root (see the
// mcp-servers catalog regression). The fake lists the whole prefix subtree,
// whereas non-recursive `mc ls` shows only the first level; consumers that
// filter on flat child names are unaffected by that difference.
func (m *Memory) ListObjectsDetailed(_ context.Context, prefix string) ([]oss.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0)
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	updatedAt := m.modTime.UTC().Format(time.RFC3339)
	infos := make([]oss.ObjectInfo, 0, len(keys))
	for _, key := range keys {
		infos = append(infos, oss.ObjectInfo{
			Name:      strings.TrimPrefix(key, prefix),
			UpdatedAt: updatedAt,
			Size:      int64(len(m.objects[key])),
			ETag:      md5Hex(m.objects[key]),
		})
	}
	return infos, nil
}

// ListObjectsRecursive lists every object under prefix at all levels,
// lexicographically by full key, with the production client's page
// contract (S3 ListObjectsV2 semantics): startAfter restricts the page to
// keys strictly greater than it, limit bounds the page, and the returned
// cursor is the last key of the page ("" when the listing is exhausted).
func (m *Memory) ListObjectsRecursive(_ context.Context, prefix, startAfter string, limit int) ([]oss.ObjectInfo, string, error) {
	if limit <= 0 {
		return nil, "", fmt.Errorf("recursive list limit must be positive, got %d", limit)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0)
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) && key > startAfter {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	// page is the slice to return; keep limit intact: the cursor rule below
	// depends on whether the page filled the caller's bound.
	page := limit
	if page > len(keys) {
		page = len(keys)
	}
	updatedAt := m.modTime.UTC().Format(time.RFC3339)
	infos := make([]oss.ObjectInfo, 0, page)
	for _, key := range keys[:page] {
		infos = append(infos, oss.ObjectInfo{
			Name:      strings.TrimPrefix(key, prefix),
			UpdatedAt: updatedAt,
			Size:      int64(len(m.objects[key])),
			ETag:      md5Hex(m.objects[key]),
		})
	}
	nextKey := ""
	// A full page always yields its last key as the cursor: the caller
	// resumes strictly after it and gets an empty page when exhausted.
	// A short page means the walk is over, so no cursor is returned (the
	// handler surfaces that as nextKey: null).
	if page > 0 && page == limit {
		nextKey = keys[page-1]
	}
	return infos, nextKey, nil
}

// DeletePrefix removes every object whose key starts with prefix.
func (m *Memory) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			delete(m.objects, key)
		}
	}
	return nil
}

// EnsureBucket is a no-op for the in-memory fake.
func (m *Memory) EnsureBucket(_ context.Context) error { return nil }

// PresignUpload implements oss.PresignUploader: a deterministic presigned
// PUT URL keyed by the object path, so tests can assert the issued shape
// without a real provider.
func (m *Memory) PresignUpload(_ context.Context, key string, maxBytes int64, contentType, sha256Hex string, ttl time.Duration) (string, string, map[string]string, error) {
	return "PUT", "https://fake.example/" + key, nil, nil
}

// Ensure Memory satisfies the interface at compile time.
var _ oss.StorageClient = (*Memory)(nil)
var _ oss.BucketManager = (*Memory)(nil)
var _ oss.PresignUploader = (*Memory)(nil)
var _ oss.RangeReader = (*Memory)(nil)
var _ oss.ConditionalMetadataWriter = (*Memory)(nil)

// NoPresign wraps a StorageClient and strips the PresignUploader
// capability, for tests that exercise the API's 500 fail-closed answer
// against a backend that cannot issue direct upload URLs. It forwards
// every StorageClient method verbatim; only PresignUpload is deliberately
// absent.
type NoPresign struct {
	inner oss.StorageClient
}

// NewNoPresign wraps store so the type assertion to oss.PresignUploader
// fails in the handler.
func NewNoPresign(store oss.StorageClient) *NoPresign {
	return &NoPresign{inner: store}
}

// ListObjectsDetailed forwards to the wrapped client.
func (n *NoPresign) ListObjectsDetailed(ctx context.Context, prefix string) ([]oss.ObjectInfo, error) {
	return n.inner.ListObjectsDetailed(ctx, prefix)
}

// ListObjectsRecursive forwards to the wrapped client.
func (n *NoPresign) ListObjectsRecursive(ctx context.Context, prefix, startAfter string, limit int) ([]oss.ObjectInfo, string, error) {
	return n.inner.ListObjectsRecursive(ctx, prefix, startAfter, limit)
}

// PutObject forwards to the wrapped client.
func (n *NoPresign) PutObject(ctx context.Context, key string, data []byte) error {
	return n.inner.PutObject(ctx, key, data)
}

// PutObjectIfMatch forwards to the wrapped client.
func (n *NoPresign) PutObjectIfMatch(ctx context.Context, key string, data []byte, matchETag string) error {
	return n.inner.PutObjectIfMatch(ctx, key, data, matchETag)
}

// PutObjectIfMatchWithType forwards to the wrapped client.
func (n *NoPresign) PutObjectIfMatchWithType(ctx context.Context, key string, data []byte, contentType, matchETag string) error {
	return n.inner.PutObjectIfMatchWithType(ctx, key, data, contentType, matchETag)
}

// PutObjectIfAbsent forwards to the wrapped client.
func (n *NoPresign) PutObjectIfAbsent(ctx context.Context, key string, data []byte) error {
	return n.inner.PutObjectIfAbsent(ctx, key, data)
}

// PutFile forwards to the wrapped client.
func (n *NoPresign) PutFile(ctx context.Context, localPath, key string) error {
	return n.inner.PutFile(ctx, localPath, key)
}

// GetObject forwards to the wrapped client.
func (n *NoPresign) GetObject(ctx context.Context, key string) ([]byte, error) {
	return n.inner.GetObject(ctx, key)
}

// Stat forwards to the wrapped client.
func (n *NoPresign) Stat(ctx context.Context, key string) error {
	return n.inner.Stat(ctx, key)
}

// StatMeta forwards to the wrapped client.
func (n *NoPresign) StatMeta(ctx context.Context, key string) (oss.ObjectMeta, error) {
	return n.inner.StatMeta(ctx, key)
}

// DeleteObject forwards to the wrapped client.
func (n *NoPresign) DeleteObject(ctx context.Context, key string) error {
	return n.inner.DeleteObject(ctx, key)
}

// Mirror forwards to the wrapped client.
func (n *NoPresign) Mirror(ctx context.Context, src, dst string, opts oss.MirrorOptions) error {
	return n.inner.Mirror(ctx, src, dst, opts)
}

// ListObjects forwards to the wrapped client.
func (n *NoPresign) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	return n.inner.ListObjects(ctx, prefix)
}

// DeletePrefix forwards to the wrapped client.
func (n *NoPresign) DeletePrefix(ctx context.Context, prefix string) error {
	return n.inner.DeletePrefix(ctx, prefix)
}

// Ensure NoPresign still satisfies the base client (it must NOT satisfy
// oss.PresignUploader — that is the whole point of the wrapper).
var _ oss.StorageClient = (*NoPresign)(nil)

// ErrNotExist is re-exported for tests that want to match against the exact
// sentinel returned by GetObject/Stat without importing "os".
var ErrNotExist = errors.New("object does not exist")
