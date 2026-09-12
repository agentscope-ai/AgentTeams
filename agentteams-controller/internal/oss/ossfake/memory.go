// Package ossfake provides in-memory fakes of the oss.StorageClient interface
// for use in unit and integration tests that exercise code paths dependent on
// object storage (package handler uploads, runtime configuration, etc.).
//
// The Memory client stores objects in a map keyed by their full object path.
// Paths are treated as opaque strings — there is no bucket/prefix logic, so
// tests see the exact keys that the production code passes in.
package ossfake

import (
	"crypto/md5"
	"encoding/hex"

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
	mu      sync.RWMutex
	objects map[string][]byte
	modTime time.Time
	next    int64
}

// md5Hex returns the lowercase MD5 hex digest of data (MinIO single-part
// ETag semantics).
func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// NewMemory constructs an empty in-memory storage client.
func NewMemory() *Memory {
	return &Memory{objects: make(map[string][]byte), modTime: time.Unix(1700000000, 0)}
}

// PutObject stores data under key.
func (m *Memory) PutObject(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[key] = buf
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

// StatMeta returns a monotonic mtime for the object. Writes advance the clock,
// so a test can verify the optimistic-lock conflict path by writing after a
// read. The ETag is the content MD5 (mirroring MinIO single-part semantics),
// so a content-changing concurrent write also changes the ETag. Returns
// os.ErrNotExist when the key is missing.
func (m *Memory) StatMeta(_ context.Context, key string) (oss.ObjectMeta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.objects[key]
	if !ok {
		return oss.ObjectMeta{}, os.ErrNotExist
	}
	return oss.ObjectMeta{Size: int64(len(data)), ModTime: m.modTime, ETag: md5Hex(data)}, nil
}

// PutObjectIfMatch writes only when the current object ETag equals matchETag.
// Mirrors the MinIO conditional-write semantics: mismatch returns
// oss.ErrPreconditionFailed and leaves the object untouched.
func (m *Memory) PutObjectIfMatch(_ context.Context, key string, data []byte, matchETag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.objects[key]; ok {
		if matchETag == "" || md5Hex(cur) != matchETag {
			return oss.ErrPreconditionFailed
		}
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

// ListObjects returns all keys whose names start with prefix, sorted.
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
// clock, so the value reflects the most recent write).
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
		infos = append(infos, oss.ObjectInfo{Name: key, UpdatedAt: updatedAt})
	}
	return infos, nil
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

// Ensure Memory satisfies the interface at compile time.
var _ oss.StorageClient = (*Memory)(nil)
var _ oss.BucketManager = (*Memory)(nil)

// ErrNotExist is re-exported for tests that want to match against the exact
// sentinel returned by GetObject/Stat without importing "os".
var ErrNotExist = errors.New("object does not exist")
