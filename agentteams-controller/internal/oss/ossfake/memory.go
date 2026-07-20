// Package ossfake provides in-memory fakes of the oss.StorageClient interface
// for use in unit and integration tests that exercise code paths dependent on
// object storage (legacy registry writes, package handler uploads, etc.).
//
// The Memory client stores objects in a map keyed by their full object path.
// Paths are treated as opaque strings — there is no bucket/prefix logic, so
// tests see the exact keys that the production code passes in.
package ossfake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// Memory is an in-memory implementation of oss.StorageClient suitable for
// tests. All methods are safe for concurrent use.
type Memory struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

// NewMemory constructs an empty in-memory storage client.
func NewMemory() *Memory {
	return &Memory{objects: make(map[string][]byte)}
}

// PutObject stores data under key.
func (m *Memory) PutObject(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[key] = buf
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

// Stat returns nil when key exists, os.ErrNotExist otherwise. LegacyCompat
// relies on errors.Is(err, os.ErrNotExist) to detect first-time writes.
func (m *Memory) Stat(_ context.Context, key string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.objects[key]; !ok {
		return os.ErrNotExist
	}
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
// dst. Local filesystem sources/destinations are not supported by the fake —
// both src and dst must be in-memory prefixes. MirrorOptions.Exclude is
// currently ignored; Overwrite=true is implicit (existing destination keys
// are replaced).
func (m *Memory) Mirror(_ context.Context, src, dst string, _ oss.MirrorOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	src = strings.TrimSuffix(src, "/")
	dst = strings.TrimSuffix(dst, "/")
	for key, data := range m.objects {
		if key != src && !strings.HasPrefix(key, src+"/") {
			continue
		}
		rel := strings.TrimPrefix(key, src)
		newKey := dst + rel
		buf := make([]byte, len(data))
		copy(buf, data)
		m.objects[newKey] = buf
	}
	return nil
}

// ListObjects returns all keys whose names start with prefix, sorted.
func (m *Memory) ListObjects(_ context.Context, prefix string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0)
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
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
