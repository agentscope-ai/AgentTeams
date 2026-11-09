package oss

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// MinIOClient implements StorageClient. The object path (put / get / stat /
// delete / list, recursive prefix deletion, bucket creation and presigned
// URLs) talks to the S3 API directly through minio-go: one cached client per
// credential identity, bytes streamed straight to the wire — no temp file,
// no process spawn, no extra copy per call. The S3-only contract means the
// object path works on any S3-compatible backend (MinIO is the default
// deployment; RustFS is the migration target): no mc subprocess, no
// MinIO-specific headers and no MinIO-specific error-code assumptions. The
// mc CLI is an optional management tool, kept only where the S3 API has no
// equivalent — tree mirroring with deletion (Mirror) and user/policy admin
// (MinIOAdminClient) — never a runtime requirement of the object path.
//
// The client supports two credential modes:
//
//   - Static (default): AccessKey/SecretKey from Config sign every request.
//   - Dynamic (credSource != nil): CredentialSource.Resolve is consulted on
//     every call and the cached SDK client is rebuilt when the resolved
//     triple changes (STS rotation). This mode is what the external-OSS
//     deployment uses to feed STS triples from the credential-provider
//     sidecar; the mc-backed paths export MC_HOST_<alias> per invocation.
type MinIOClient struct {
	config     Config
	credSource CredentialSource
	aliasReady bool
	// sdk caches the minio-go client for the internal endpoint (see
	// s3Client); a fresh cache per client so a WithCredentialSource clone
	// never signs with the original client's credential profile.
	sdk *s3Client
	// sdkPutIfMatch overrides the SDK conditional write for tests; nil uses
	// the real minio-go path.
	sdkPutIfMatch func(ctx context.Context, key string, data []byte, matchETag string) error
	// sdkPutIfAbsent overrides the SDK create-if-absent write used by the
	// human-file paths (main); nil uses the real minio-go path.
	sdkPutIfAbsent func(ctx context.Context, key string, data []byte) error
	// sdkPutIfNoneMatch overrides the SDK create-if-absent write used by the
	// task-meta first creation; nil uses the real minio-go path.
	sdkPutIfNoneMatch func(ctx context.Context, key string, data []byte) error
	// sdkPutIfMatchWithMetadata overrides the SDK conditional write with
	// user metadata for tests; nil uses the real minio-go path.
	sdkPutIfMatchWithMetadata func(ctx context.Context, key string, data []byte, contentType, matchETag string, metadata map[string]string) error
	// sdkPutIfAbsentWithMetadata overrides the SDK create-if-absent write
	// with user metadata for tests; nil uses the real minio-go path.
	sdkPutIfAbsentWithMetadata func(ctx context.Context, key string, data []byte, contentType string, metadata map[string]string) error
	// sdkListObjects overrides the SDK ListObjects channel for tests; nil
	// uses the real minio-go path. It is the seam for every listing walk
	// (ListObjectsDetailed, ListObjectsRecursive and DeletePrefix's sweep
	// list) and receives the exact bucket and options the production path
	// builds, so a coordinate bug (a request prefix wrapped with the mc
	// StoragePrefix) surfaces as an empty walk against an S3-faithful store.
	sdkListObjects func(ctx context.Context, bucket string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo
	// sdkRemoveObjects overrides the SDK RemoveObjects channel for tests;
	// nil uses the real minio-go path. It is the seam for DeletePrefix's
	// multi-delete batches and receives the exact bucket and key batch the
	// production path deletes (the real path converts the batch to the
	// SDK's ObjectInfo input channel).
	sdkRemoveObjects func(ctx context.Context, bucket string, objects []string, opts minio.RemoveObjectsOptions) <-chan minio.RemoveObjectError
	// sdkRemoveObject overrides the SDK single-object delete for tests;
	// nil uses the real minio-go path. It is the seam for DeleteObject
	// and for DeletePrefix's marker-key step, and receives the exact
	// bucket and key the production path deletes.
	sdkRemoveObject func(ctx context.Context, bucket, key string) error
}

// NewMinIOClient creates a StorageClient. Object I/O goes through the
// minio-go SDK (s3Client); the mc CLI is only for the management paths
// listed on the type.
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
	return &MinIOClient{config: cfg, sdk: &s3Client{}}
}

// WithCredentialSource returns a copy of the client that fetches credentials
// dynamically on every call. Intended for external-OSS deployments where
// STS tokens expire periodically.
func (c *MinIOClient) WithCredentialSource(src CredentialSource) *MinIOClient {
	clone := *c
	clone.credSource = src
	clone.aliasReady = false
	clone.sdk = &s3Client{} // the clone signs as a different identity
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
	// A fault that survived both semantic checks (a 5xx body, a dead
	// connection, ...) is a store-availability fault, not a write failure
	// the caller should treat as terminal.
	return classifySDKError(err)
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
	return classifySDKError(err)
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
	return classifySDKError(err)
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
	return classifySDKError(err)
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

// PutObjectIfMatchWithMetadata is PutObjectIfMatchWithType plus S3 user
// metadata, attached in the same conditional PUT (a follow-up metadata
// update would race the ETag and could land out of order with a
// concurrent write). metadata keys are passed bare (no x-amz-meta-
// prefix) and are stored lowercase. An ETag mismatch surfaces as
// ErrPreconditionFailed; the plain-write fallback of PutObjectIfMatch is
// deliberately NOT replicated here — degrading a metadata write to a
// plain put would silently drop the metadata the caller asked to store,
// and the sync-status consumer already falls back to the manifest body
// when the metadata is absent.
func (c *MinIOClient) PutObjectIfMatchWithMetadata(ctx context.Context, key string, data []byte, contentType, matchETag string, metadata map[string]string) error {
	metadata = normalizeUserMetadataKeys(metadata)
	var err error
	if put := c.sdkPutIfMatchWithMetadata; put != nil {
		err = put(ctx, key, data, contentType, matchETag, metadata)
	} else {
		err = c.sdkPutObjectIfMatchWithMetadata(ctx, key, data, contentType, matchETag, metadata)
	}
	if err == nil {
		return nil
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && (resp.Code == "PreconditionFailed" || resp.Code == "412") {
		return ErrPreconditionFailed
	}
	return classifySDKError(err)
}

// PutObjectIfAbsentWithMetadata is PutObjectIfAbsent plus S3 user
// metadata, attached in the same conditional PUT. An existing key
// surfaces as ErrPreconditionFailed; metadata keys are handled as in
// PutObjectIfMatchWithMetadata.
func (c *MinIOClient) PutObjectIfAbsentWithMetadata(ctx context.Context, key string, data []byte, contentType string, metadata map[string]string) error {
	metadata = normalizeUserMetadataKeys(metadata)
	var err error
	if put := c.sdkPutIfAbsentWithMetadata; put != nil {
		err = put(ctx, key, data, contentType, metadata)
	} else {
		err = c.sdkPutObjectIfAbsentWithMetadata(ctx, key, data, contentType, metadata)
	}
	if err == nil {
		return nil
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && (resp.Code == "PreconditionFailed" || resp.Code == "412") {
		return ErrPreconditionFailed
	}
	return classifySDKError(err)
}

// sdkPutObjectIfMatchWithMetadata is the MinIO SDK conditional write with
// user metadata. It is a separate function so tests can swap the
// transport via sdkPutIfMatchWithMetadata.
func (c *MinIOClient) sdkPutObjectIfMatchWithMetadata(ctx context.Context, key string, data []byte, contentType, matchETag string, metadata map[string]string) error {
	sdk, err := c.sdkClient()
	if err != nil {
		return err
	}
	reader := bytes.NewReader(data)
	opts := minio.PutObjectOptions{ContentType: contentType, UserMetadata: metadata}
	opts.SetMatchETag(matchETag)
	_, err = sdk.PutObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), reader, int64(len(data)), opts)
	return err
}

// sdkPutObjectIfAbsentWithMetadata is the MinIO SDK create-if-absent
// write (If-None-Match: *) with user metadata. It is a separate function
// so tests can swap the transport via sdkPutIfAbsentWithMetadata.
func (c *MinIOClient) sdkPutObjectIfAbsentWithMetadata(ctx context.Context, key string, data []byte, contentType string, metadata map[string]string) error {
	sdk, err := c.sdkClient()
	if err != nil {
		return err
	}
	reader := bytes.NewReader(data)
	opts := minio.PutObjectOptions{ContentType: contentType, UserMetadata: metadata}
	opts.SetMatchETagExcept("*")
	_, err = sdk.PutObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), reader, int64(len(data)), opts)
	return err
}

// userMetadataPrefix is the S3 wire prefix of user metadata keys. It is
// applied on the wire only: the SDK's PutObjectOptions.UserMetadata
// prefixes keys with it, and StatObject strips it into ObjectInfo's
// metadata maps. Callers of this package deal in bare lowercase keys.
const userMetadataPrefix = "x-amz-meta-"

// normalizeUserMetadataKeys canonicalizes caller-supplied metadata to the
// bare lowercase key form the store uses: the x-amz-meta- prefix is
// stripped and keys are lowercased (S3 metadata keys are case-insensitive
// on read but the wire form varies by backend — MinIO and RustFS were
// verified to both return lowercase). Empty keys or values are dropped.
// Returns nil when nothing survives.
func normalizeUserMetadataKeys(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]string, len(metadata))
	for k, v := range metadata {
		nk := stripUserMetadataPrefix(k)
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

// stripUserMetadataPrefix lowercases k and strips the S3 wire prefix.
func stripUserMetadataPrefix(k string) string {
	return strings.TrimPrefix(strings.ToLower(k), userMetadataPrefix)
}

// objectUserMetadata merges the user metadata a StatObject reported in
// both SDK shapes into bare lowercase keys: info.UserMetadata already
// carries stripped keys (MinIO form) and wins; entries in info.Metadata
// (the raw response-header form, `x-amz-meta-*`, as S3 and RustFS
// populate) fill the gaps. Returns nil when the object has no user
// metadata.
func objectUserMetadata(info minio.ObjectInfo) map[string]string {
	merged := make(map[string]string, len(info.UserMetadata)+len(info.Metadata))
	for k, v := range info.UserMetadata {
		nk := stripUserMetadataPrefix(k)
		if nk == "" || v == "" {
			continue
		}
		merged[nk] = v
	}
	for k, vals := range info.Metadata {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, userMetadataPrefix) {
			continue
		}
		nk := strings.TrimPrefix(lk, userMetadataPrefix)
		// minio-go exposes the raw response headers as http.Header
		// (map[string][]string); S3 user metadata is single-valued, but
		// join defensively instead of discarding extra values.
		v := strings.Join(vals, ",")
		if nk == "" || v == "" {
			continue
		}
		if _, exists := merged[nk]; exists {
			continue
		}
		merged[nk] = v
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// isSDKNotFound reports whether an SDK error means "the object does not
// exist": a 404 carrying the S3 NoSuchKey code, or a 404 without a
// parseable code (backends answer some not-found requests with an empty
// body). This is the SDK equivalent of the mc "Object does not exist"
// string match the CLI path used to do.
func isSDKNotFound(err error) bool {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	// Only a missing *object* is a not-found. A 404 for the bucket (wrong
	// bucket name, missing bucket, IAM misconfiguration) must stay an error:
	// mapping it to os.ErrNotExist would turn a deployment mistake into a
	// delete-propagation event for the sync client (the D1 failure mode).
	switch resp.Code {
	case "NoSuchBucket", "NoSuchUpload", "NoSuchVersion":
		return false
	}
	if resp.Code == "NoSuchKey" || resp.Code == "NoSuchObject" || resp.Code == "NotFound" {
		return true
	}
	// HEAD answers a missing key with an empty body, so the SDK reports 404
	// without a code; that shape still means "object missing".
	return resp.StatusCode == http.StatusNotFound && resp.Code == ""
}

// isSDKBucketGone reports the codes a bucket-level 404 carries.
func isSDKBucketGone(err error) bool {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	switch resp.Code {
	case "NoSuchBucket", "NoSuchUpload", "NoSuchVersion":
		return true
	}
	return false
}

// classifyKeyError normalizes the error of a key-scoped call.
//
// A not-found answer is not proof that the object is gone: minio-go maps a
// 404 on HEAD to NoSuchKey even when the bucket itself disappeared (an
// out-of-band deletion leaves the cached client's location table intact, so
// the request still reaches the store and gets the bucket-level 404). Probing
// bucket existence before returning os.ErrNotExist keeps a storage failure
// from reading as a deleted file — the same class of bug as D1's outage
// handling. Every other error goes through classifySDKError, so a transport
// fault is a retryable ErrStorageUnavailable, never a raw 500.
func (c *MinIOClient) classifyKeyError(ctx context.Context, key string, err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrStorageUnavailable) || errors.Is(err, ErrStorageBucketUnavailable) {
		return err
	}
	if isSDKBucketGone(err) {
		return err // bucket-level codes stay errors, never os.ErrNotExist
	}
	if !isSDKNotFound(err) {
		return classifySDKError(err)
	}
	ok, perr := c.bucketAvailable(ctx, key)
	if perr != nil {
		// Cannot prove the bucket exists → surface the probe failure rather
		// than claim the object is missing. classifySDKError keeps a probe
		// transport fault retryable (503) and a semantic fault a 500.
		return classifySDKError(perr)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrStorageBucketUnavailable, c.config.Bucket)
	}
	return os.ErrNotExist
}

// bucketAvailable reports whether the configured bucket still exists.
//
// There is deliberately no cache: a positive answer is only true at the
// moment it was observed. A cached "present" is exactly what made an
// out-of-band deletion read as a missing file in the first place — the
// ordinary not-found check on a genuinely absent key would warm the cache,
// and a bucket deleted a second later would still be reported as present.
// Each ambiguous 404 therefore pays one HEAD; correctness over a saved
// round-trip on an error path.
func (c *MinIOClient) bucketAvailable(ctx context.Context, key string) (bool, error) {
	sdk, err := c.sdkClient()
	if err != nil {
		return false, err
	}
	ok, err := sdk.BucketExists(ctx, c.config.Bucket)
	if err != nil {
		var response minio.ErrorResponse
		if !errors.As(err, &response) || response.StatusCode != http.StatusForbidden {
			return false, fmt.Errorf("oss: bucket probe: %w", err)
		}
		// Prefix-scoped STS may list this key's scope while bucket HEAD is
		// forbidden. A successful scoped listing proves the bucket exists
		// without requiring broader permissions; any probe failure stays
		// an error, never a missing-object answer.
		probeCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		for object := range sdk.ListObjects(probeCtx, c.config.Bucket, minio.ListObjectsOptions{
			Prefix: strings.TrimPrefix(key, "/"), Recursive: true, MaxKeys: 1,
		}) {
			if object.Err != nil {
				return false, fmt.Errorf("oss: scoped bucket probe: %w", object.Err)
			}
			return true, nil
		}
		if err := probeCtx.Err(); err != nil {
			return false, fmt.Errorf("oss: scoped bucket probe: %w", err)
		}
		return true, nil
	}
	return ok, nil
}

// classifySDKError is the single non-key-scoped error-classification gate
// the S3 object path runs every error through before it returns to a caller
// (PR-0). Missing-object normalization is NOT done here: only a key-scoped
// caller can prove an object is absent, and it must first prove the bucket
// still exists (classifyKeyError). This gate sorts the remaining answers:
//
//   - store unavailable (HTTP 5xx, RequestTimeout, or a transport failure
//     that never reached a semantic answer: refused/reset/closed
//     connections, EOF mid-body, timeouts) -> ErrStorageUnavailable wrapped
//     with %w, the "503 storage_unavailable, retry later" signal;
//   - everything else (404s from bucket-level/config faults, 403
//     AccessDenied, InvalidAccessKeyId, ...) -> the error unchanged: a
//     deployment fault is a 500, never a 404 (absence) or a 503 (retry).
//
// The classification is idempotent: an already-classified error (nil,
// os.ErrNotExist, ErrStorageUnavailable, ErrStorageBucketUnavailable)
// passes through unchanged.
func classifySDKError(err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrStorageUnavailable) || errors.Is(err, ErrStorageBucketUnavailable) {
		return err
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		// A 5xx body (InternalError, ServiceUnavailable, ...) or the S3
		// RequestTimeout code (400) is the store admitting it could not
		// serve the request. Everything else the SDK hands us as an
		// ErrorResponse is a semantic 4xx answer (NoSuchBucket, AccessDenied,
		// InvalidAccessKeyId, ...) and passes through unchanged.
		if resp.StatusCode >= http.StatusInternalServerError || resp.Code == "RequestTimeout" {
			return fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
		}
		return err
	}
	if isTransportUnavailable(err) {
		return fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
	}
	return err
}

// isTransportUnavailable reports whether err is a network-layer failure:
// the request or its body never received a semantic answer. net/http wraps
// every connection-level fault (dial refused, reset or close mid-response,
// DNS) in a *url.Error — a net.Error that reports Timeout()==false — so any
// net.Error in the chain means the store never answered. A context deadline
// is matched by identity as well: the bare sentinel carries no transport
// type of its own, and from the caller's view a deadline means the store
// did not answer in time, so the API layer answers 503.
func isTransportUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed)
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
	// A fresh one-shot client per issuance (see newPublicSDKClient): the
	// signature must carry the credential identity resolved at this moment.
	sdk, err := c.newPublicSDKClient()
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
var _ ConditionalMetadataWriter = (*MinIOClient)(nil)

// putStream is the SDK put shared by the hot-path writes: the bytes are
// streamed straight to the provider (one PUT, no temp file, no process
// spawn). contentType is sent verbatim as the object's Content-Type.
func (c *MinIOClient) putStream(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	sdk, err := c.sdkClient()
	if err != nil {
		return err
	}
	_, err = sdk.PutObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), r, size, minio.PutObjectOptions{ContentType: contentType})
	return classifySDKError(err)
}

func (c *MinIOClient) PutObject(ctx context.Context, key string, data []byte) error {
	// The mc path stored application/octet-stream (the temp file's .tmp
	// extension maps to nothing better); keep the stored type identical.
	return c.putStream(ctx, key, bytes.NewReader(data), int64(len(data)), "application/octet-stream")
}

// PutObjectWithType writes data with an explicit content type (sent as the
// object's Content-Type header). An empty type degrades to PutObject, which
// stores application/octet-stream — the same default mc fell back to, so
// callers that must carry a marker (deletion tombstones) set one.
func (c *MinIOClient) PutObjectWithType(ctx context.Context, key string, data []byte, contentType string) error {
	if contentType == "" {
		return c.PutObject(ctx, key, data)
	}
	return c.putStream(ctx, key, bytes.NewReader(data), int64(len(data)), contentType)
}

// PutFile streams the local file to key. The content type is inferred from
// the local path's extension exactly the way mc cp did (mime lookup,
// application/octet-stream when the extension is unknown).
func (c *MinIOClient) PutFile(ctx context.Context, localPath, key string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	contentType := mime.TypeByExtension(filepath.Ext(localPath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return c.putStream(ctx, key, f, fi.Size(), contentType)
}

func (c *MinIOClient) GetObject(ctx context.Context, key string) ([]byte, error) {
	sdk, err := c.sdkClient()
	if err != nil {
		return nil, err
	}
	// minio-go issues the GET on the first read: a missing key surfaces as
	// the 404 error there, not from the open.
	obj, err := sdk.GetObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), minio.GetObjectOptions{})
	if err != nil {
		return nil, c.classifyKeyError(ctx, key, err)
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, c.classifyKeyError(ctx, key, err)
	}
	return data, nil
}

// OpenRange implements RangeReader: it opens [start, start+length) of the
// object (or [start, end) when length < 0) as a streamed S3 Range GET,
// bound to matchETag through the provider's If-Match so a stat-then-open
// caller can never read a version it did not stat. minio-go issues the
// GET eagerly, so the semantic failures surface here, at open time,
// before any byte is read:
//
//   - 412 PreconditionFailed -> ErrPreconditionFailed (the content moved);
//   - 416 InvalidRange       -> ErrRangeNotSatisfiable (the range no longer
//     fits the current size);
//   - 404                     -> os.ErrNotExist via the bucket probe, and
//     every transport/5xx fault -> the wrapped ErrStorageUnavailable,
//     exactly as GetObject (a down store is a 503, never an absence).
func (c *MinIOClient) OpenRange(ctx context.Context, key string, start, length int64, matchETag string) (io.ReadCloser, error) {
	sdk, err := c.sdkClient()
	if err != nil {
		return nil, err
	}
	if length == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	opts := minio.GetObjectOptions{}
	// RangeReader speaks (start, length); minio-go SetRange speaks an
	// inclusive end offset. Passing length straight through served
	// bytes=10-19 for a 20-byte window and made suffix/short windows fail.
	switch {
	case length < 0:
		if start > 0 {
			if serr := opts.SetRange(start, 0); serr != nil { // bytes=start-
				return nil, serr
			}
		}
	default:
		end := start + length - 1
		if end < start || start < 0 {
			return nil, ErrRangeNotSatisfiable
		}
		if serr := opts.SetRange(start, end); serr != nil {
			return nil, serr
		}
	}
	if matchETag != "" {
		if serr := opts.SetMatchETag(matchETag); serr != nil {
			return nil, serr
		}
	}
	obj, err := sdk.GetObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), opts)
	if err != nil {
		return nil, c.classifyRangeError(ctx, key, err)
	}
	// minio-go opens lazily: the GET happens on the first Read. Wrap the
	// stream so a 412/416/404/5xx surfaced by that first Read keeps the
	// same classification as an eager open; callers can then pre-read
	// before committing response headers.
	return &classifiedReadCloser{
		ReadCloser: obj,
		classify:   func(err error) error { return c.classifyRangeError(ctx, key, err) },
	}, nil
}

// classifyRangeError maps range-read failures: a moved snapshot is 412
// (retry/re-stat), a stale window is 416, and everything else follows the
// key-scoped storage mapping (bucket probe, 503, 500).
func (c *MinIOClient) classifyRangeError(ctx context.Context, key string, err error) error {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		switch {
		case resp.Code == "PreconditionFailed" || resp.Code == "412":
			return ErrPreconditionFailed
		case resp.Code == "InvalidRange" || resp.Code == "416" ||
			resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
			return ErrRangeNotSatisfiable
		}
	}
	return c.classifyKeyError(ctx, key, err)
}

// classifiedReadCloser defers SDK error classification to Read, where a
// lazy minio-go object actually performs the request.
type classifiedReadCloser struct {
	io.ReadCloser
	classify func(error) error
}

func (r *classifiedReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = r.classify(err)
	}
	return n, err
}

// PresignGet mints a presigned download URL for the object at key that
// stays valid for ttl (#1475 PR-4, #1501). The minio-go SDK computes the
// S3 V4 signature locally from the credential triple resolved at that
// moment and returns the signed URL: neither the object bytes nor the
// credentials ever leave the process, and no mc process is spawned on the
// per-request path (the mc share download era is over).
//
// The URL is signed for the public endpoint's origin, so its host is the
// caller-reachable host. The Host participates in the S3 V4 signature,
// which is exactly why the public endpoint exists: a URL minted for the
// internal endpoint cannot be rewritten to a public hostname afterwards —
// storage would answer SignatureDoesNotMatch. The URL grants anonymous
// read access to exactly that object until it expires.
//
// When Config.PublicEndpoint is empty there is no caller-reachable origin
// to sign for: PresignGet returns ErrPresignUnsupported WITHOUT touching
// storage, so the API reports the capability boundary explicitly (501
// presign_unsupported) instead of leaking an unusable URL.
func (c *MinIOClient) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("presign ttl must be positive, got %s", ttl)
	}
	if c.config.PublicEndpoint == "" {
		return "", ErrPresignUnsupported
	}
	// A fresh one-shot client per issuance (see newPublicSDKClient): the
	// signature must carry the credential identity resolved at this moment.
	sdk, err := c.newPublicSDKClient()
	if err != nil {
		return "", err
	}
	// The SDK selects the bucket itself, so the signed object name is the
	// bucket-relative key — signing fullPath (mc alias/bucket prefix) would
	// mint a URL for the wrong object, as it did for PresignUpload. No
	// extra query params: the plain object download is the only shape the
	// API hands to callers.
	u, err := sdk.PresignedGetObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), ttl, nil)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (c *MinIOClient) Stat(ctx context.Context, key string) error {
	sdk, err := c.sdkClient()
	if err != nil {
		return err
	}
	_, err = sdk.StatObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), minio.StatObjectOptions{})
	if err != nil {
		return c.classifyKeyError(ctx, key, err)
	}
	return nil
}

// StatMeta returns the object's size, last-modified time, ETag and content
// type by reading the S3 response headers of a HEAD request. The last
// modified time is second-precision (the HTTP header), which is sufficient
// for the low-frequency human-intervention optimistic lock (W-PR-2): the
// check catches "Controller read then Worker pushed before the write".
func (c *MinIOClient) StatMeta(ctx context.Context, key string) (ObjectMeta, error) {
	sdk, err := c.sdkClient()
	if err != nil {
		return ObjectMeta{}, err
	}
	info, err := sdk.StatObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), minio.StatObjectOptions{})
	if err != nil {
		return ObjectMeta{}, c.classifyKeyError(ctx, key, err)
	}
	// The SDK strips S3's ETag quotes (the mc stat --json output was
	// already unquoted), so the two paths hand the caller the same form.
	// User metadata is merged from both SDK shapes (see objectUserMetadata).
	return ObjectMeta{
		Size:         info.Size,
		ModTime:      info.LastModified,
		ETag:         info.ETag,
		ContentType:  info.ContentType,
		UserMetadata: objectUserMetadata(info),
	}, nil
}

func (c *MinIOClient) DeleteObject(ctx context.Context, key string) error {
	if remove := c.sdkRemoveObject; remove != nil {
		return remove(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"))
	}
	sdk, err := c.sdkClient()
	if err != nil {
		return err
	}
	// S3 DeleteObject answers 204 even for a missing key, so this is
	// idempotent where `mc rm` errored on a missing object. Every caller
	// uses deletion for cleanup/rollback (errors are ignored), which the
	// idempotent form suits; none depends on the missing-key error.
	return classifySDKError(sdk.RemoveObject(ctx, c.config.Bucket, strings.TrimPrefix(key, "/"), minio.RemoveObjectOptions{}))
}

// Mirror keeps the mc CLI: there is no minio-go equivalent of
// `mc mirror --remove` (reconcile src into dst while deleting dst's
// surplus objects), and skill sync is a low-frequency management
// operation, not hot-path I/O.
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

// DeletePrefix removes every object at or under prefix through the S3
// API: a recursive list (the SDK channel pages through the storage's
// continuation tokens) of everything under the directory scope, and
// DeleteObjects multi-delete in batches of at most 1000 keys (the S3
// per-call limit). A prefix without a trailing slash also deletes the
// object whose key is exactly the prefix (a marker object, if any) first.
// S3 has no recursive delete, and the `mc rm --recursive --force` call
// was dropped because the mc CLI must not sit on the object path — agent
// deletion (deployer) and the human-file trash purge run this.
//
// prefix is matched as a directory scope, not a raw string prefix:
// DeletePrefix("a/b") deletes exactly "a/b" and everything under
// "a/b/", and never touches "a/b-sibling/*". The raw-prefix form the mc
// call matched with would have swept siblings that merely share the
// prefix string (a "a/b-sibling/x" key starts with "a/b"), silently
// deleting unrelated data.
//
// A trailing slash is the caller-declared boundary: DeletePrefix("a/b/")
// sweeps exactly what lies under "a/b/" and never deletes the bare key
// "a/b" itself. The production sweep callers (worker cleanup and the
// human-file trash purge) pass trailing-slash prefixes, and a restricted
// credential whose policy allows "a/b/*" only rejects the bare-key
// delete — so the boundary form must never issue it. A prefix that
// normalizes to the bucket root ("" or "/") keeps the raw behavior — the
// whole bucket is swept. A prefix with no objects is a no-op; per-key
// delete errors are aggregated into the returned error so a partial
// sweep is never reported as a clean one.
func (c *MinIOClient) DeletePrefix(ctx context.Context, prefix string) error {
	sdk, err := c.sdkClient()
	if err != nil {
		return err
	}
	// Bucket-relative coordinates: the bucket rides on the SDK call, and
	// fullPath's mc alias/bucket wrapper is not part of any bucket key.
	p := strings.TrimPrefix(prefix, "/")
	// An explicit trailing slash is the caller-declared boundary: the
	// sweep covers exactly what lies under the prefix and must never
	// touch the bare key itself. The production sweep callers (worker
	// cleanup and the human-file trash purge) pass trailing-slash
	// prefixes, and a restricted credential whose policy allows the
	// prefix's keys only rejects the bare-key delete — so the boundary
	// form must never issue it.
	explicitDir := strings.HasSuffix(p, "/") && p != "/"
	if !explicitDir {
		// No boundary declared: the sweep also removes the marker object
		// at the exact key, if any. The scan itself stays on the
		// directory scope — a raw string prefix would also match siblings
		// that merely share the string ("a/b-sibling/x" starts with
		// "a/b") and delete them.
		norm := strings.TrimSuffix(p, "/")
		if norm != "" {
			// S3 DeleteObject answers 204 for a missing key, so a plain
			// not-found never surfaces here; a remaining error is a real
			// failure the sweep must not paper over.
			if err := c.DeleteObject(ctx, norm); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("delete prefix %q: remove marker object %q: %w", prefix, norm, err)
			}
			p = norm + "/"
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var ch <-chan minio.ObjectInfo
	if list := c.sdkListObjects; list != nil {
		ch = list(ctx, c.config.Bucket, minio.ListObjectsOptions{Prefix: p, Recursive: true})
	} else {
		ch = sdk.ListObjects(ctx, c.config.Bucket, minio.ListObjectsOptions{Prefix: p, Recursive: true})
	}
	keys := make([]string, 0)
	for o := range ch {
		if o.Err != nil {
			return fmt.Errorf("delete prefix %q: list objects: %w", prefix, classifySDKError(o.Err))
		}
		keys = append(keys, o.Key)
	}
	var errs []error
	const maxBatch = 1000
	for start := 0; start < len(keys); start += maxBatch {
		end := min(start+maxBatch, len(keys))
		batch := keys[start:end]
		var resCh <-chan minio.RemoveObjectError
		if remove := c.sdkRemoveObjects; remove != nil {
			resCh = remove(ctx, c.config.Bucket, batch, minio.RemoveObjectsOptions{})
		} else {
			objects := make(chan minio.ObjectInfo, len(batch))
			for _, k := range batch {
				objects <- minio.ObjectInfo{Key: k}
			}
			close(objects)
			resCh = sdk.RemoveObjects(ctx, c.config.Bucket, objects, minio.RemoveObjectsOptions{})
		}
		for r := range resCh {
			if r.Err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", r.ObjectName, classifySDKError(r.Err)))
			}
		}
	}
	return errors.Join(errs...)
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

// ListObjectsDetailed returns the one-level view of prefix: direct
// children by name (spaces intact) and nested subtrees as "dir/" entries,
// in the storage's lexicographic order. It asks the storage API for the
// one-level view directly — a non-recursive listing, which the S3
// delimiter answer serves as direct-child objects plus common prefixes —
// so there is no local synthesis of directory entries and no text parsing
// that could truncate a name (the mc era split the human-readable form on
// spaces and had to bolt on the JSON/find fallbacks).
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
			return nil, fmt.Errorf("list objects: %w", classifySDKError(o.Err))
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
			return nil, "", fmt.Errorf("list objects (recursive): %w", classifySDKError(o.Err))
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

// EnsureBucket creates the configured bucket if it does not already
// exist, through the S3 MakeBucket API (this runs once at controller
// startup, not per request). An already-existing bucket is treated as
// success — the idempotent form of the `mc mb --ignore-existing` flag it
// replaced — because the controller may run against a bucket pre-created
// by the operator or by a previous controller generation.
func (c *MinIOClient) EnsureBucket(ctx context.Context) error {
	sdk, err := c.sdkClient()
	if err != nil {
		return err
	}
	err = sdk.MakeBucket(ctx, c.config.Bucket, minio.MakeBucketOptions{})
	if err == nil {
		return nil
	}
	if isBucketAlreadyExists(err) {
		return nil
	}
	return err
}

// isBucketAlreadyExists reports whether a MakeBucket error means the
// bucket already exists. The provider codes differ between backends
// (S3/MinIO answer BucketAlreadyOwnedByYou or BucketAlreadyExists; a
// backend without a standard code answers a bare conflict), so the check
// covers the codes plus the 409 status — the same tolerance
// `mc mb --ignore-existing` had, and free of any MinIO-specific
// assumption.
func isBucketAlreadyExists(err error) bool {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	return resp.Code == "BucketAlreadyOwnedByYou" || resp.Code == "BucketAlreadyExists" ||
		resp.StatusCode == http.StatusConflict
}

func (c *MinIOClient) runMC(ctx context.Context, args ...string) (string, error) {
	return c.runMCOnAlias(ctx, c.config.Alias, args...)
}

// runMCOnAlias is runMC bound to a specific alias. In dynamic-credential
// mode the MC_HOST_<alias> env var is built from that alias's endpoint
// (endpointForAlias) instead of a persistent alias file.
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
