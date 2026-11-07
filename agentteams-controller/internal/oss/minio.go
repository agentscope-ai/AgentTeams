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
	// publicAliasReady tracks the one-time `mc alias set` of the presign
	// alias (the caller-reachable public endpoint).
	publicAliasReady bool
	// sdkPutIfMatch overrides the SDK conditional write for tests; nil uses
	// the real minio-go path.
	sdkPutIfMatch func(ctx context.Context, key string, data []byte, matchETag string) error
	// sdkPutIfAbsent overrides the SDK create-if-absent write used by the
	// human-file paths (main); nil uses the real minio-go path.
	sdkPutIfAbsent func(ctx context.Context, key string, data []byte) error
	// sdkPutIfNoneMatch overrides the SDK create-if-absent write used by the
	// task-meta first creation; nil uses the real minio-go path.
	sdkPutIfNoneMatch func(ctx context.Context, key string, data []byte) error
	// sdkListObjects overrides the SDK ListObjects channel for tests; nil
	// uses the real minio-go path. It receives the exact bucket and options
	// the production path builds, so a coordinate bug (a request prefix
	// wrapped with the mc StoragePrefix) surfaces as an empty walk against
	// an S3-faithful store.
	sdkListObjects func(ctx context.Context, bucket string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo
}

// NewMinIOClient creates a StorageClient backed by the mc CLI.
func NewMinIOClient(cfg Config) *MinIOClient {
	if cfg.MCBinary == "" {
		cfg.MCBinary = "mc"
	}
	if cfg.Alias == "" {
		cfg.Alias = "agentteams"
	}
	if cfg.PublicAlias == "" {
		cfg.PublicAlias = "agentteams-public"
	}
	return &MinIOClient{config: cfg}
}

// WithCredentialSource returns a copy of the client that fetches credentials
// dynamically on every mc invocation. Intended for external-OSS deployments
// where STS tokens expire periodically.
func (c *MinIOClient) WithCredentialSource(src CredentialSource) *MinIOClient {
	clone := *c
	clone.credSource = src
	clone.aliasReady = false
	clone.publicAliasReady = false
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

// ensurePublicAlias registers the presign alias — `mc alias set` against
// the caller-reachable public endpoint with the same storage credentials —
// once per client. The alias is the only difference between the internal
// and the presign view of the storage; mc resolves the URL's host from the
// alias's endpoint when signing. Dynamic mode has no persistent alias:
// runMCOnAlias injects MC_HOST_<publicAlias> per call instead.
func (c *MinIOClient) ensurePublicAlias(ctx context.Context) error {
	if c.credSource != nil {
		return nil
	}
	if c.publicAliasReady || c.config.PublicEndpoint == "" {
		return nil
	}
	_, err := c.runMC(ctx, "alias", "set", c.config.PublicAlias, c.config.PublicEndpoint, c.config.AccessKey, c.config.SecretKey)
	if err != nil {
		return fmt.Errorf("mc alias set %s: %w", c.config.PublicAlias, err)
	}
	c.publicAliasReady = true
	return nil
}

func (c *MinIOClient) fullPath(key string) string {
	return c.config.StoragePrefix + "/" + strings.TrimPrefix(key, "/")
}

// publicFullPath renders the mc path for the presign alias: the public
// alias + the same bucket. It must not reuse StoragePrefix, whose first
// segment IS the main alias in every deployment (the presign alias differs
// from it).
func (c *MinIOClient) publicFullPath(key string) string {
	return c.config.PublicAlias + "/" + c.config.Bucket + "/" + strings.TrimPrefix(key, "/")
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

// PutObjectIfMatchStrict is the non-degrading variant of PutObjectIfMatch:
// only the SDK conditional write is used. An ETag mismatch surfaces as
// ErrPreconditionFailed, and a provider that cannot honor If-Match
// (NotImplemented/501) surfaces as ErrConditionalUnsupported — with NO
// write performed and no compare-then-put fallback. Conflict-sensitive
// callers (pause-source updates) must use this and fail closed on
// ErrConditionalUnsupported; PutObjectIfMatch keeps the weak fallback for
// callers that can accept the narrow compare-to-put window.
func (c *MinIOClient) PutObjectIfMatchStrict(ctx context.Context, key string, data []byte, matchETag string) error {
	put := c.sdkPutIfMatch
	if put == nil {
		put = c.sdkPutObjectIfMatch
	}
	err := put(ctx, key, data, matchETag)
	if err == nil {
		return nil
	}

	var resp minio.ErrorResponse
	if errors.As(err, &resp) && (resp.Code == "PreconditionFailed" || resp.Code == "412") {
		return ErrPreconditionFailed
	}
	// Never degrade: a compare-then-put here would reopen exactly the
	// conflict window this method exists to close.
	if errors.As(err, &resp) && (resp.Code == "NotImplemented" || resp.Code == "501") {
		return ErrConditionalUnsupported
	}
	return err
}

// PutObjectIfNoneMatchStrict is the create-if-absent (If-None-Match: *)
// variant of PutObjectIfMatchStrict: the object is created only when it
// does not exist, and an existing object (a concurrent create landed in
// between) surfaces as ErrPreconditionFailed. A provider that cannot
// honor If-None-Match (NotImplemented/501) surfaces as
// ErrConditionalUnsupported — with NO write performed — so the
// create-if-absent callers (the task meta first creation) fail closed
// exactly like the If-Match strict path: a check-then-create would
// reopen exactly the create/cancel race this method exists to close.
func (c *MinIOClient) PutObjectIfNoneMatchStrict(ctx context.Context, key string, data []byte) error {
	put := c.sdkPutIfNoneMatch
	if put == nil {
		put = c.sdkPutObjectIfNoneMatch
	}
	err := put(ctx, key, data)
	if err == nil {
		return nil
	}

	var resp minio.ErrorResponse
	if errors.As(err, &resp) && (resp.Code == "PreconditionFailed" || resp.Code == "412") {
		return ErrPreconditionFailed
	}
	if errors.As(err, &resp) && (resp.Code == "NotImplemented" || resp.Code == "501") {
		return ErrConditionalUnsupported
	}
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

// sdkPutObjectIfNoneMatch is the MinIO SDK create-if-absent write
// (If-None-Match: *). It is a separate function so tests can swap the
// transport via sdkPutIfNoneMatch.
func (c *MinIOClient) sdkPutObjectIfNoneMatch(ctx context.Context, key string, data []byte) error {
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

// PresignUpload implements PresignUploader. The POST policy form is
// preferred: its conditions carry the content-length bound (the presign
// limit) and the content type, so an oversized upload is rejected by the
// provider itself. When the provider cannot build or sign a POST policy the
// call degrades to a presigned PUT — the bound and the digest are then
// enforced by the commit endpoint, which re-checks size and sha256 before
// accounting. (A POST policy condition cannot bind the sha256 portably, so
// the digest never appears in the signed form; commit is its enforcer.)
func (c *MinIOClient) PresignUpload(ctx context.Context, key string, maxBytes int64, contentType, sha256Hex string, ttl time.Duration) (string, string, map[string]string, error) {
	if c.config.PublicEndpoint == "" {
		return "", "", nil, ErrPresignUnsupported
	}
	// Sign for the caller-reachable origin. Rewriting the signed URL's
	// host later would invalidate its signature; internal I/O keeps Endpoint.
	public := *c
	public.config.Endpoint = c.config.PublicEndpoint
	sdk, err := public.sdkClient()
	if err != nil {
		return "", "", nil, err
	}
	// The SDK selects the bucket itself, so the signed object name is the
	// bucket-relative key — fullPath (mc alias/bucket prefix) only applies
	// to mc invocations. Signing fullPath made the browser upload land under
	// agentteams/agentteams-storage/<key> while commit read <key>, which the
	// real RustFS surfaced as 404 object_missing after a 204 upload.
	object := strings.TrimPrefix(key, "/")
	policy := minio.NewPostPolicy()
	perr := policy.SetBucket(c.config.Bucket)
	if perr == nil {
		perr = policy.SetKey(object)
	}
	if perr == nil {
		perr = policy.SetExpires(time.Now().UTC().Add(ttl))
	}
	if perr == nil {
		perr = policy.SetContentLengthRange(0, maxBytes)
	}
	if perr == nil && contentType != "" {
		perr = policy.SetContentType(contentType)
	}
	if perr == nil {
		if u, form, serr := sdk.PresignedPostPolicy(ctx, policy); serr == nil {
			return "POST", u.String(), form, nil
		}
	}
	// The policy could not be built or signed; fall back to a presigned PUT
	// (whose error is what is returned when that fails too).
	u, perr := sdk.PresignedPutObject(ctx, c.config.Bucket, object, ttl)
	if perr != nil {
		return "", "", nil, perr
	}
	return "PUT", u.String(), nil, nil
}

var _ PresignUploader = (*MinIOClient)(nil)

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

// PresignGet mints a presigned download URL for the object at key that
// stays valid for ttl (#1475 PR-4, #1501). It shells out to
// `mc share download --expire <ttl> --json <publicFullPath>` against the
// PUBLIC alias and returns the URL from the `share` field: the mc CLI
// computes the S3 V4 signature locally from the alias credentials, so
// neither the object bytes nor the credentials ever leave the process.
//
// The URL is signed for the public endpoint's origin, so its host is the
// caller-reachable host. The Host participates in the S3 V4 signature,
// which is exactly why the public alias exists: a URL minted for the
// internal endpoint cannot be rewritten to a public hostname afterwards —
// storage would answer SignatureDoesNotMatch. The URL grants anonymous
// read access to exactly that object until it expires.
//
// When Config.PublicEndpoint is empty there is no caller-reachable origin
// to sign for: PresignGet returns ErrPresignUnsupported WITHOUT invoking
// mc, so the API reports the capability boundary explicitly (501
// presign_unsupported) instead of leaking an unusable URL.
func (c *MinIOClient) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("presign ttl must be positive, got %s", ttl)
	}
	if c.config.PublicEndpoint == "" {
		return "", ErrPresignUnsupported
	}
	if err := c.ensurePublicAlias(ctx); err != nil {
		return "", err
	}
	out, err := c.runMCOnAlias(ctx, c.config.PublicAlias, "share", "download", "--expire", ttl.String(), "--json", c.publicFullPath(key))
	if err != nil {
		return "", err
	}
	var msg struct {
		Share string `json:"share"`
	}
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		return "", fmt.Errorf("parse mc share download --json: %w", err)
	}
	if msg.Share == "" {
		return "", errors.New("mc share download returned an empty share URL")
	}
	return msg.Share, nil
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
	// One-level listing through the SDK, the same path ListObjectsRecursive
	// already uses. The mc fallbacks this replaces depended on the binary:
	// `mc ls --json` carries `modified`, but when that flag or alias probe
	// failed the code fell back to `mc find`, which has no timestamp column —
	// so directory listings silently lost their modification times and the
	// UI dropped its time column. Driving the SDK keeps names, sizes, ETags
	// and timestamps intact regardless of the mc build.
	//
	// Keys stay prefix-relative and directory entries keep their trailing
	// slash: the handler synthesises directories from any name containing a
	// slash, so both shapes must be preserved.
	p := strings.TrimPrefix(strings.TrimPrefix(prefix, "/"), c.config.StoragePrefix+"/")
	// A delimiter listing needs a directory boundary: without the trailing
	// slash S3 treats the prefix as a plain string prefix, so "scope/target"
	// also matches "scope/target-sibling" and returns it as a child. Every
	// caller of this method passes a directory, so normalise the boundary and
	// trim returned keys against the same form.
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	opts := minio.ListObjectsOptions{Prefix: p, Recursive: false}
	ctx, cancel := context.WithCancel(ctx)
	var ch <-chan minio.ObjectInfo
	if list := c.sdkListObjects; list != nil {
		ch = list(ctx, c.config.Bucket, opts)
	} else {
		sdk, err := c.sdkClient()
		if err != nil {
			cancel()
			return nil, err
		}
		ch = sdk.ListObjects(ctx, c.config.Bucket, opts)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()
	var out []ObjectInfo
	for o := range ch {
		if o.Err != nil {
			return nil, fmt.Errorf("list objects: %w", o.Err)
		}
		rel := strings.TrimPrefix(o.Key, p)
		if rel == "" {
			continue // the prefix marker itself (e.g. "files/")
		}
		info := ObjectInfo{Name: rel, Size: o.Size, ETag: o.ETag}
		if !o.LastModified.IsZero() {
			info.UpdatedAt = o.LastModified.UTC().Format(time.RFC3339)
		}
		out = append(out, info)
	}
	return out, nil
}

// ListObjectsRecursive walks every object under prefix at all levels, one
// page at a time. It drives the SDK ListObjects channel with Recursive and
// StartAfter instead of the mc CLI: the cursor (StartAfter) and the page
// bound (MaxKeys) are storage parameters, so a page is one storage request
// and the cursor is the last returned key — re-passing it as startAfter
// walks the prefix with no gaps and no duplicates. (The mc CLI has no
// StartAfter equivalent; mc ls --recursive cannot be paged.)
//
// prefix and startAfter are storage-relative keys (the same key space as
// the rest of StorageClient). The SDK lists bucket keys and receives the
// bucket separately, so both are passed as bucket-relative coordinates:
// wrapping them with fullPath would lift them into the mc alias/bucket
// path no bucket key carries, and the walk would come up empty on a real
// backend. Entries are returned with Name relative to prefix (the same
// contract as ListObjectsDetailed), and nextKey is the storage-relative
// key of the last returned entry — empty when the prefix is exhausted, so
// the caller can pass it back verbatim as ?cursor=.
func (c *MinIOClient) ListObjectsRecursive(ctx context.Context, prefix, startAfter string, limit int) ([]ObjectInfo, string, error) {
	if limit <= 0 {
		return nil, "", fmt.Errorf("recursive list limit must be positive, got %d", limit)
	}
	// Bucket-relative coordinates: the bucket rides on the SDK call, and
	// fullPath's mc alias/bucket wrapper is not part of any bucket key.
	p := strings.TrimPrefix(prefix, "/")
	opts := minio.ListObjectsOptions{
		Prefix:    p,
		Recursive: true,
		MaxKeys:   limit,
	}
	if startAfter != "" {
		opts.StartAfter = strings.TrimPrefix(startAfter, "/")
	}
	ctx, cancel := context.WithCancel(ctx)
	var ch <-chan minio.ObjectInfo
	if list := c.sdkListObjects; list != nil {
		ch = list(ctx, c.config.Bucket, opts)
	} else {
		sdk, err := c.sdkClient()
		if err != nil {
			cancel()
			return nil, "", err
		}
		ch = sdk.ListObjects(ctx, c.config.Bucket, opts)
	}
	// The SDK may still be sending when this page reaches its limit.
	// Cancel and drain its terminal error so its producer can exit.
	defer func() {
		cancel()
		for range ch {
		}
	}()
	infos := make([]ObjectInfo, 0, limit)
	lastKey := ""
	for o := range ch {
		if o.Err != nil {
			return nil, "", fmt.Errorf("list objects (recursive): %w", o.Err)
		}
		rel := strings.TrimPrefix(o.Key, p)
		if rel == "" {
			continue // the prefix marker object itself (e.g. "files/")
		}
		infos = append(infos, ObjectInfo{
			Name:      rel,
			Size:      o.Size,
			ETag:      o.ETag,
			UpdatedAt: o.LastModified.UTC().Format(time.RFC3339),
		})
		// o.Key is already bucket-relative — the same coordinate the
		// cursor resumes from and the key the caller stats or gets.
		lastKey = o.Key
		if len(infos) == limit {
			break
		}
	}
	nextKey := ""
	// A full page always yields its last key as the cursor: the caller
	// resumes strictly after it and gets an empty page when exhausted
	// (the same contract the fake and the handler tests pin).
	if len(infos) == limit {
		nextKey = lastKey
	}
	return infos, nextKey, nil
}

func (c *MinIOClient) EnsureBucket(ctx context.Context) error {
	if err := c.ensureAlias(ctx); err != nil {
		return err
	}
	target := c.config.Alias + "/" + c.config.Bucket
	_, err := c.runMC(ctx, "mb", target, "--ignore-existing")
	return err
}

func (c *MinIOClient) runMC(ctx context.Context, args ...string) (string, error) {
	return c.runMCOnAlias(ctx, c.config.Alias, args...)
}

// runMCOnAlias is runMC bound to a specific alias. In dynamic-credential
// mode the MC_HOST_<alias> env var is built from that alias's OWN endpoint
// (endpointForAlias), so the presign alias signs against the public
// endpoint while every other command stays on the internal one.
func (c *MinIOClient) runMCOnAlias(ctx context.Context, alias string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.config.MCBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if c.credSource != nil {
		creds, err := c.credSource.Resolve(ctx)
		if err != nil {
			return "", fmt.Errorf("resolve oss credentials: %w", err)
		}
		hostEnv, herr := buildMCHostEnv(alias, c.endpointForAlias(alias), creds)
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

// endpointForAlias resolves the endpoint an alias binds to: the presign
// alias is the caller-reachable public endpoint, every other alias the
// internal one.
func (c *MinIOClient) endpointForAlias(alias string) string {
	if alias == c.config.PublicAlias {
		return c.config.PublicEndpoint
	}
	return c.config.Endpoint
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
