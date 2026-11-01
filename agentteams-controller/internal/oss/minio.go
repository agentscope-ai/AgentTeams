package oss

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"time"
)

// MinIOClient implements StorageClient using the mc (MinIO Client) CLI.
// This provides zero-migration-risk compatibility with the existing shell scripts
// while hiding the mc implementation detail behind the StorageClient interface.
//
// The client supports two credential modes:
//
//   - Static (default): AccessKey/SecretKey from Config are installed once via
//     `mc alias set` and reused for every subsequent command.
//   - Dynamic (credSource != nil): the client skips persistent alias setup and
//     instead exports MC_HOST_<alias> on every invocation, populated from
//     CredentialSource.Resolve. This mode is what the external-OSS deployment
//     uses to feed STS triples from the credential-provider sidecar.
type MinIOClient struct {
	config     Config
	credSource CredentialSource
	aliasReady bool
	// lsFormatProbe caches the one-time capability probe of the mc binary
	// (see lsJSONAvailable); a pointer so the WithCredentialSource shallow
	// clone shares the cached answer for the same binary.
	lsFormatProbe *lsFormatProbe
	// sdkPutIfMatch overrides the SDK conditional write for tests; nil uses
	// the real minio-go path.
	sdkPutIfMatch  func(ctx context.Context, key string, data []byte, matchETag string) error
	sdkPutIfAbsent func(ctx context.Context, key string, data []byte) error
}

// NewMinIOClient creates a StorageClient backed by the mc CLI.
func NewMinIOClient(cfg Config) *MinIOClient {
	if cfg.MCBinary == "" {
		cfg.MCBinary = "mc"
	}
	if cfg.Alias == "" {
		cfg.Alias = "agentteams"
	}
	return &MinIOClient{config: cfg, lsFormatProbe: &lsFormatProbe{}}
}

// WithCredentialSource returns a copy of the client that fetches credentials
// dynamically on every mc invocation. Intended for external-OSS deployments
// where STS tokens expire periodically.
func (c *MinIOClient) WithCredentialSource(src CredentialSource) *MinIOClient {
	clone := *c
	clone.credSource = src
	clone.aliasReady = false
	return &clone
}

func (c *MinIOClient) ensureAlias(ctx context.Context) error {
	if c.credSource != nil {
		// Dynamic mode: no persistent alias. MC_HOST_* env vars are
		// prepared per call in runMC.
		return nil
	}
	if c.aliasReady || c.config.Endpoint == "" {
		return nil
	}
	_, err := c.runMC(ctx, "alias", "set", c.config.Alias, c.config.Endpoint, c.config.AccessKey, c.config.SecretKey)
	if err != nil {
		return fmt.Errorf("mc alias set: %w", err)
	}
	c.aliasReady = true
	return nil
}

func (c *MinIOClient) fullPath(key string) string {
	return c.config.StoragePrefix + "/" + strings.TrimPrefix(key, "/")
}

// PutObjectIfMatch performs a conditional write: the object is only replaced
// when its current ETag equals matchETag. This closes the check-then-act
// race of the optimistic lock — a Worker write landing between the
// Controller's read and its write makes the ETag change and the write fails
// with ErrPreconditionFailed instead of silently overwriting.
func (c *MinIOClient) PutObjectIfMatch(ctx context.Context, key string, data []byte, matchETag string) error {
	return c.PutObjectIfMatchWithType(ctx, key, data, "application/json", matchETag)
}

// PutObjectIfMatchWithType is the conditional write with an explicit content
// type; the file API uses it to write deletion tombstones.
func (c *MinIOClient) PutObjectIfMatchWithType(ctx context.Context, key string, data []byte, contentType, matchETag string) error {
	var err error
	if put := c.sdkPutIfMatch; put != nil {
		err = put(ctx, key, data, matchETag)
	} else {
		sdk, serr := c.sdkClient()
		if serr != nil {
			return serr
		}
		reader := bytes.NewReader(data)
		opts := minio.PutObjectOptions{ContentType: contentType}
		opts.SetMatchETag(matchETag)
		_, err = sdk.PutObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), reader, int64(len(data)), opts)
	}
	if err == nil {
		return nil
	}

	// MinIO rejects an If-Match mismatch with a precondition-failed
	// response; the SDK surfaces it as an ErrorResponse with that code.
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && (resp.Code == "PreconditionFailed" || resp.Code == "412") {
		return ErrPreconditionFailed
	}

	// Provider fallback: Alibaba Cloud OSS does not support If-Match on
	// PutObject (returns NotImplemented). Availability beats eliminating the
	// tiny compare-to-put race there: re-check the read-bound ETag against
	// the current object and, if unchanged, do a plain PutObject. An
	// observed conflict is still rejected; only the narrow compare-to-put
	// window is accepted on OSS.
	if errors.As(err, &resp) && (resp.Code == "NotImplemented" || resp.Code == "501") {
		// The fallback must keep the requested media type: dropping it turns a
		// tombstone into an ordinary empty object, which reads as a live empty
		// file and makes an ordinary restore answer path_conflict.
		return ossFallbackWrite(ctx, key, data, matchETag, contentType, c.StatMeta, c.PutObjectWithType)
	}
	return err
}

// PutObjectIfAbsent creates an object only when the key does not exist.
// This is the atomic initialization primitive for the per-human quota
// ledger; unlike If-Match on a missing object, If-None-Match:* succeeds on
// create and fails on an existing key. Providers that do not implement
// If-None-Match return an error (fail closed) rather than falling back to a
// racy Stat-then-Put.
func (c *MinIOClient) PutObjectIfAbsent(ctx context.Context, key string, data []byte) error {
	put := c.sdkPutIfAbsent
	if put == nil {
		put = c.sdkPutObjectIfAbsent
	}
	err := put(ctx, key, data)
	if err == nil {
		return nil
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && (resp.Code == "PreconditionFailed" || resp.Code == "412") {
		return ErrPreconditionFailed
	}
	return err
}

// sdkPutObjectIfAbsent is the MinIO SDK If-None-Match:* write.
func (c *MinIOClient) sdkPutObjectIfAbsent(ctx context.Context, key string, data []byte) error {
	sdk, err := c.sdkClient()
	if err != nil {
		return err
	}
	reader := bytes.NewReader(data)
	opts := minio.PutObjectOptions{ContentType: "application/json"}
	opts.SetMatchETagExcept("*")
	_, err = sdk.PutObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), reader, int64(len(data)), opts)
	return err
}

// ossFallbackWrite is the OSS degradation for providers without If-Match
// (Alibaba Cloud OSS returns NotImplemented). It re-checks the read-bound
// ETag immediately before a plain PutObject: an observed conflict is
// rejected; only the narrow compare-to-put race is accepted. Extracted as a
// pure function so the contract is testable without a live backend.
func ossFallbackWrite(ctx context.Context, key string, data []byte, matchETag, contentType string,
	stat func(context.Context, string) (ObjectMeta, error),
	put func(context.Context, string, []byte, string) error) error {
	cur, err := stat(ctx, key)
	if err != nil {
		return err
	}
	if canonicalizeETag(cur.ETag) != matchETag {
		return ErrPreconditionFailed
	}
	return put(ctx, key, data, contentType)
}

// canonicalizeETag normalizes a provider ETag for comparison: S3-style
// quotes are stripped and hex is lowercased. OSS commonly returns uppercase
// and/or quoted MD5 ETags while our read-bound ETag is a lowercase hex MD5,
// so a verbatim comparison would deterministically reject unchanged content.
func canonicalizeETag(etag string) string {
	return strings.ToLower(strings.Trim(etag, "\""))
}

// sdkPutObjectIfMatch is the MinIO SDK conditional write. It is a separate
// function so tests can swap the transport via sdkPutIfMatch.
func (c *MinIOClient) sdkPutObjectIfMatch(ctx context.Context, key string, data []byte, matchETag string) error {
	sdk, err := c.sdkClient()
	if err != nil {
		return err
	}
	reader := bytes.NewReader(data)
	opts := minio.PutObjectOptions{ContentType: "application/json"}
	opts.SetMatchETag(matchETag)
	_, err = sdk.PutObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), reader, int64(len(data)), opts)
	return err
}

// sdkClient lazily builds a minio-go client for conditional writes. The
// regular read/write path stays on the mc CLI (mirror semantics, alias
// handling, dynamic STS credentials); only If-Match writes need the SDK.
func (c *MinIOClient) sdkClient() (*minio.Client, error) {
	// Preserve the endpoint scheme for TLS selection: https:// → Secure,
	// anything else → plain. The endpoint stays intact (host[:port]).
	endpoint := c.config.Endpoint
	secure := false
	if strings.HasPrefix(endpoint, "https://") {
		secure = true
	}
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")

	// Dynamic STS credentials win over the static pair (external OSS /
	// refreshable tokens); the static pair is the embedded-MinIO default.
	// A credential-resolution failure is returned, never silently replaced
	// by the static pair (which would be the wrong identity for STS-based
	// deployments).
	accessKey, secretKey, token := c.config.AccessKey, c.config.SecretKey, ""
	if c.credSource != nil {
		creds, err := c.credSource.Resolve(context.Background())
		if err != nil {
			return nil, fmt.Errorf("resolve dynamic oss credentials: %w", err)
		}
		accessKey, secretKey, token = creds.AccessKeyID, creds.AccessKeySecret, creds.SecurityToken
	}

	return minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, token),
		Secure: secure,
	})
}

func (c *MinIOClient) PutObject(ctx context.Context, key string, data []byte) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp("", "agentteams-oss-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()

	return c.PutFile(ctx, tmpFile.Name(), key)
}

// PutObjectWithType writes data with an explicit content type. mc infers the
// type from the temp file's extension otherwise (application/octet-stream), so
// the fallback path would lose a tombstone marker without this.
func (c *MinIOClient) PutObjectWithType(ctx context.Context, key string, data []byte, contentType string) error {
	if contentType == "" {
		return c.PutObject(ctx, key, data)
	}
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp("", "agentteams-oss-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()
	_, err = c.runMC(ctx, "cp", "--attr", "Content-Type="+contentType, tmpFile.Name(), c.fullPath(key))
	return err
}

func (c *MinIOClient) PutFile(ctx context.Context, localPath, key string) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	_, err := c.runMC(ctx, "cp", localPath, c.fullPath(key))
	return err
}

func (c *MinIOClient) GetObject(ctx context.Context, key string) ([]byte, error) {
	if err := c.ensureAlias(ctx); err != nil {
		return nil, err
	}
	out, err := c.runMC(ctx, "cat", c.fullPath(key))
	if err != nil {
		if strings.Contains(err.Error(), "Object does not exist") ||
			strings.Contains(err.Error(), "exit status") {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return []byte(out), nil
}

func (c *MinIOClient) Stat(ctx context.Context, key string) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	_, err := c.runMC(ctx, "stat", c.fullPath(key))
	if err != nil {
		if strings.Contains(err.Error(), "Object does not exist") ||
			strings.Contains(err.Error(), "exit status") {
			return os.ErrNotExist
		}
		return err
	}
	return nil
}

// StatMeta returns the object's size and last-modified time by invoking
// `mc stat --json <fullpath>` and parsing the lastModified field. The last
// modified time is second-precision (MinIO), which is sufficient for the
// low-frequency human-intervention optimistic lock (W-PR-2): the check
// catches "Controller read then Worker pushed before the write".
func (c *MinIOClient) StatMeta(ctx context.Context, key string) (ObjectMeta, error) {
	if err := c.ensureAlias(ctx); err != nil {
		return ObjectMeta{}, err
	}
	out, err := c.runMC(ctx, "stat", "--json", c.fullPath(key))
	if err != nil {
		if strings.Contains(err.Error(), "Object does not exist") ||
			strings.Contains(err.Error(), "exit status") {
			return ObjectMeta{}, os.ErrNotExist
		}
		return ObjectMeta{}, err
	}
	// mc stat --json has no top-level content type: S3 response headers land
	// in the canonicalized `metadata` map (Content-Type, X-Amz-Meta-*, ...).
	// The top-level field is accepted as a fallback so a future/other client
	// shape still works.
	return parseMCStatJSON(out)
}

// parseMCStatJSON decodes `mc stat --json` into ObjectMeta. Extracted so the
// parsing contract (metadata map vs. top-level field) is testable without the
// mc binary.
func parseMCStatJSON(out string) (ObjectMeta, error) {
	var info struct {
		LastModified string            `json:"lastModified"`
		Size         int64             `json:"size"`
		ETag         string            `json:"etag"`
		ContentType  string            `json:"contentType"`
		Metadata     map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return ObjectMeta{}, fmt.Errorf("parse mc stat --json: %w", err)
	}
	modTime, err := time.Parse(time.RFC3339, info.LastModified)
	if err != nil {
		return ObjectMeta{}, fmt.Errorf("parse lastModified %q: %w", info.LastModified, err)
	}
	contentType := info.ContentType
	if contentType == "" {
		contentType = contentTypeFromMCMetadata(info.Metadata)
	}
	return ObjectMeta{Size: info.Size, ModTime: modTime, ETag: info.ETag, ContentType: contentType}, nil
}

// contentTypeFromMCMetadata reads Content-Type out of mc's metadata map. S3
// clients canonicalize header names, but the map is decoded from JSON, so the
// lookup is case-insensitive and also accepts the lower-case and
// X-Amz-Meta-Content-Type spellings some backends emit. RustFS serves the same
// S3 headers, so the same lookup applies there.
func contentTypeFromMCMetadata(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	clean := make(map[string]string, len(metadata))
	for key, value := range metadata {
		clean[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	// Precedence is explicit so a map iteration order cannot change the result.
	for _, key := range []string{"content-type", "x-amz-meta-content-type"} {
		if value := clean[key]; value != "" {
			return value
		}
	}
	return ""
}

func (c *MinIOClient) DeleteObject(ctx context.Context, key string) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	_, err := c.runMC(ctx, "rm", c.fullPath(key))
	return err
}

func (c *MinIOClient) Mirror(ctx context.Context, src, dst string, opts MirrorOptions) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	// Apply storage prefix to paths that are not local (don't start with /).
	// This makes Mirror consistent with PutObject/GetObject which auto-prefix keys.
	if !strings.HasPrefix(src, "/") {
		src = c.fullPath(src)
	}
	if !strings.HasPrefix(dst, "/") {
		dst = c.fullPath(dst)
	}
	args := []string{"mirror", src, dst}
	if opts.Overwrite {
		args = append(args, "--overwrite")
	}
	if opts.Remove {
		args = append(args, "--remove")
	}
	for _, pattern := range opts.Exclude {
		args = append(args, "--exclude", pattern)
	}
	_, err := c.runMC(ctx, args...)
	return err
}

func (c *MinIOClient) DeletePrefix(ctx context.Context, prefix string) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	_, err := c.runMC(ctx, "rm", "--recursive", "--force", c.fullPath(prefix))
	return err
}

func (c *MinIOClient) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	infos, err := c.ListObjectsDetailed(ctx, prefix)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	return names, nil
}

func (c *MinIOClient) ListObjectsDetailed(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := c.ensureAlias(ctx); err != nil {
		return nil, err
	}
	// The human-readable `mc ls` form is space-split and would truncate
	// object names that contain spaces ("meeting notes.txt" ->
	// "notes.txt"), silently dropping those bytes from quota accounting.
	// `mc ls --json` carries the full name intact; mc binaries that predate
	// the flag fall back to `mc find`, which also prints one full path per
	// line and never splits names on spaces.
	if c.lsJSONAvailable(ctx) {
		out, err := c.runMC(ctx, "ls", "--json", c.fullPath(prefix))
		if err != nil {
			return nil, err
		}
		return parseMCLsJSON(out, prefix)
	}
	out, err := c.runMC(ctx, "find", c.fullPath(prefix))
	if err != nil {
		return nil, err
	}
	return c.parseMCFind(out, prefix)
}

// lsFormatProbe caches the answer of the one-time `mc ls --json` capability
// probe for the life of the client.
type lsFormatProbe struct {
	mu     sync.Mutex
	probed bool
	jsonOK bool
}

// lsJSONAvailable reports whether the configured mc binary understands
// `mc ls --json`, probing once and caching the definitive answer. The probe
// lists the bucket root: a modern mc (>= RELEASE.2025-08-13) answers with
// NDJSON (the content is irrelevant), an mc that predates the flag exits
// non-zero with an unknown-flag error. A probe that fails for any other
// reason (network, credentials) is not cached, so the next listing re-probes;
// this listing degrades to the `mc find` fallback, which fails for the same
// root cause — a listing never succeeds against a half-verified store.
func (c *MinIOClient) lsJSONAvailable(ctx context.Context) bool {
	p := c.lsFormatProbe
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.probed {
		return p.jsonOK
	}
	_, err := c.runMC(ctx, "ls", "--json", c.config.StoragePrefix)
	if err == nil {
		p.probed, p.jsonOK = true, true
		return true
	}
	// Cache only the definitive "this binary has no such flag" answer; a
	// transient probe failure must not lock the client into the
	// timestamp-less fallback for the rest of the process.
	if isMCUnknownFlag(err) {
		p.probed, p.jsonOK = true, false
	}
	return false
}

// isMCUnknownFlag reports whether a runMC error is mc's unknown-flag
// rejection, the definitive signal that the binary predates the flag.
func isMCUnknownFlag(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown flag")
}

// parseMCLsJSON decodes `mc ls --json` (mc RELEASE.2025-08-13) NDJSON into
// one ObjectInfo per line:
//
//	{"type":"file","size":6,"title":"meeting notes.txt","modified":"2026-11-01T02:33:00.000Z","etag":"..."}
//	{"type":"dir","size":0,"title":"my dir/","modified":"..."}
//
// Parsing contract (every ListObjectsDetailed consumer relies on it):
//
//   - Name is the one-level relative name from `title`, spaces preserved;
//     directories carry a trailing slash (added when mc omits it), files
//     never do;
//   - UpdatedAt is `modified` as RFC3339 UTC, or "" when it cannot be
//     parsed (timestamp drift must never fail a listing);
//   - a single malformed line is an error: a silently skipped line would
//     undercount the quota exactly the way the old space-truncating parser
//     did.
//
// size and etag are intentionally not read: ObjectInfo is unchanged and
// callers stat each key for the exact bytes. Extracted so the contract is
// testable without the mc binary.
// parseMCLsJSON parses `mc ls --json`. The structured field is "key" (the full
// path, e.g. "bucket/alice/dir with space/f.txt"); "title" only exists in some
// mc builds and was read here, which yielded empty names and silently dropped
// every object from quota accounting. Names are returned relative to prefix,
// like parseMCFind, because callers build keys as prefix+name.
// listingRelName maps a listing entry to a name relative to prefix.
// Listings arrive in three shapes - bucket-absolute ("bucket/a/b/x"),
// bucket-relative ("a/b/x") and, for some fixtures, already relative ("x") -
// so the prefix is located as a path segment instead of being trimmed blindly.
// A zero-byte empty string with ok=false means the entry is outside the prefix.
func listingRelName(key, prefix string) (string, bool) {
	trimmed := strings.Trim(prefix, "/")
	if trimmed == "" {
		return strings.TrimPrefix(key, "/"), true
	}
	needle := trimmed + "/"
	if i := strings.Index(key, needle); i >= 0 && (i == 0 || key[i-1] == '/') {
		return key[i+len(needle):], true
	}
	// A listing may already be relative to the requested path (mc omits the
	// prefix it was given). Keeping the name as-is is the only shape that does
	// not silently drop the entry; an absolute key that merely lacks the prefix
	// is handled by the caller's stat check.
	return key, true
}

// parseMCLsJSON parses `mc ls --json`. The structured field is "key" (the full
// path, e.g. "bucket/alice/dir with space/f.txt"); "title" only exists in some
// mc builds and was the field read here before, which yielded empty names and
// silently dropped every object from quota accounting. Names are returned
// relative to prefix, one directory level at a time, exactly like parseMCFind,
// so callers can rebuild a key as prefix+name.
func parseMCLsJSON(out, prefix string) ([]ObjectInfo, error) {
	var infos []ObjectInfo
	seen := make(map[string]struct{})
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Type     string `json:"type"`
			Key      string `json:"key"`
			Title    string `json:"title"`
			Modified string `json:"modified"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("parse mc ls --json line %q: %w", line, err)
		}
		key := entry.Key
		if key == "" {
			key = entry.Title // older mc builds only emit the path as title
		}
		if key == "" {
			continue
		}
		rel, ok := listingRelName(key, prefix)
		if !ok || rel == "" {
			continue
		}
		// Normalise the shape: a listing entry may carry a trailing slash from
		// the backend, so strip it first, then collapse anything nested to its
		// first directory segment. Files never end in "/", directories always do.
		// Both real backends report a directory as a key ending in "/" - MinIO
		// and RustFS 1.0.4 do not always set type:"dir" (they emit type:"file"
		// with size 0), so the trailing slash is the authority, not `type`.
		isDir := entry.Type == "dir" || strings.HasSuffix(rel, "/")
		rel = strings.TrimSuffix(rel, "/")
		name := rel
		if k := strings.IndexByte(rel, '/'); k > 0 && !isDir {
			name = rel[:k] + "/" // nested key: synthesize its directory entry
		} else if isDir {
			name = rel + "/"
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		updated := ""
		if t, perr := time.Parse(time.RFC3339Nano, entry.Modified); perr == nil {
			updated = t.UTC().Format(time.RFC3339) // callers expect RFC3339 UTC
		}
		infos = append(infos, ObjectInfo{Name: name, UpdatedAt: updated})
	}
	return infos, nil
}

// parseMCFind decodes `mc find <path>` output into one-level ObjectInfos.
// `mc find` is the fallback for mc binaries that predate `mc ls --json`
// (see lsJSONAvailable): it prints one full object path per line — no human
// columns to split — so names with spaces survive verbatim. The walk is
// recursive (every nested key appears once), so the one-level view — file
// names plus synthesized "dir" entries for nested prefixes — is rebuilt
// from each key's first path segment. mc find carries no modification time,
// so UpdatedAt stays "" (the same degradation parseMCLsJSON applies to an
// unparseable timestamp).
//
// The printed path carries some leading context in front of the listing
// prefix — the alias/bucket form, the bare bucket form, or no prefix at
// all, depending on the mc version — so the longest known form is stripped
// before taking the first segment.
func (c *MinIOClient) parseMCFind(out string, prefix string) ([]ObjectInfo, error) {
	trimmed := strings.Trim(prefix, "/")
	candidates := []string{
		strings.TrimSuffix(c.fullPath(prefix), "/") + "/",
		c.config.Bucket + "/" + trimmed + "/",
	}
	if trimmed != "" {
		candidates = append(candidates, trimmed+"/")
	}
	seen := make(map[string]struct{})
	var infos []ObjectInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rel := line
		for _, strip := range candidates {
			if strings.HasPrefix(line, strip) {
				rel = line[len(strip):]
				break
			}
		}
		if rel == "" {
			continue
		}
		name := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			name = rel[:i] + "/" // nested key: synthesize its directory entry
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		infos = append(infos, ObjectInfo{Name: name})
	}
	return infos, nil
}

// EnsureBucket creates the configured bucket if it does not already exist.
func (c *MinIOClient) EnsureBucket(ctx context.Context) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	target := c.config.Alias + "/" + c.config.Bucket
	_, err := c.runMC(ctx, "mb", target, "--ignore-existing")
	return err
}

func (c *MinIOClient) runMC(ctx context.Context, args ...string) (string, error) {
	return c.runMCWithInput(ctx, nil, args...)
}

func (c *MinIOClient) runMCWithInput(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.config.MCBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	if c.credSource != nil {
		creds, err := c.credSource.Resolve(ctx)
		if err != nil {
			return "", fmt.Errorf("resolve oss credentials: %w", err)
		}
		hostEnv, herr := buildMCHostEnv(c.config.Alias, c.config.Endpoint, creds)
		if herr != nil {
			return "", herr
		}
		cmd.Env = append(os.Environ(), hostEnv)
	}

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mc %s: %w (stderr: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// buildMCHostEnv renders a single MC_HOST_<alias>=<scheme>://<ak>:<sk>[:<token>]@<host>
// environment-variable binding. The mc CLI accepts this form as an
// alternative to persistent ~/.mc/config.json alias entries, and
// honours the security-token component when present.
//
// The endpoint is supplied by the caller (normally MinIOClient.config.Endpoint,
// sourced from AGENTTEAMS_FS_ENDPOINT). A bare hostname (e.g.
// "oss-cn-hangzhou.aliyuncs.com") without a URL scheme is accepted; in
// that case we default to https.
//
// IMPORTANT: mc (tested with RELEASE.2025-08-13) does NOT URL-decode the
// userinfo segment of MC_HOST_* before using the values. Any percent-
// encoding applied here is forwarded verbatim into the X-Amz-Security-
// Token header (and the signed AK/SK), which Alibaba Cloud OSS rejects
// with InvalidSecurityToken. We therefore pass the triple raw; STS
// credentials issued by Alibaba Cloud contain only characters (base64
// alphabet plus "+/=") that Go's url.Parse accepts inside userinfo.
func buildMCHostEnv(alias string, endpoint string, c Credentials) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("storage endpoint is not configured (AGENTTEAMS_FS_ENDPOINT is empty)")
	}
	normalized := endpoint
	if !strings.HasPrefix(normalized, "http://") && !strings.HasPrefix(normalized, "https://") {
		normalized = "https://" + normalized
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return "", fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("endpoint %q must include scheme and host", endpoint)
	}

	userinfo := c.AccessKeyID + ":" + c.AccessKeySecret
	if c.SecurityToken != "" {
		userinfo += ":" + c.SecurityToken
	}
	value := fmt.Sprintf("%s://%s@%s", u.Scheme, userinfo, u.Host)
	return fmt.Sprintf("MC_HOST_%s=%s", alias, value), nil
}
