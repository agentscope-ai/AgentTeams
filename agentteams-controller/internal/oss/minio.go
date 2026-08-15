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
	// sdkPutIfMatch overrides the SDK conditional write for tests; nil uses
	// the real minio-go path.
	sdkPutIfMatch func(ctx context.Context, key string, data []byte, matchETag string) error
}

// NewMinIOClient creates a StorageClient backed by the mc CLI.
func NewMinIOClient(cfg Config) *MinIOClient {
	if cfg.MCBinary == "" {
		cfg.MCBinary = "mc"
	}
	if cfg.Alias == "" {
		cfg.Alias = "agentteams"
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
	put := c.sdkPutIfMatch
	if put == nil {
		put = c.sdkPutObjectIfMatch
	}
	err := put(ctx, key, data, matchETag)
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
		return ossFallbackWrite(ctx, key, data, matchETag, c.StatMeta, c.PutObject)
	}
	return err
}

// ossFallbackWrite is the OSS degradation for providers without If-Match
// (Alibaba Cloud OSS returns NotImplemented). It re-checks the read-bound
// ETag immediately before a plain PutObject: an observed conflict is
// rejected; only the narrow compare-to-put race is accepted. Extracted as a
// pure function so the contract is testable without a live backend.
func ossFallbackWrite(ctx context.Context, key string, data []byte, matchETag string,
	stat func(context.Context, string) (ObjectMeta, error),
	put func(context.Context, string, []byte) error) error {
	cur, err := stat(ctx, key)
	if err != nil {
		return err
	}
	if canonicalizeETag(cur.ETag) != matchETag {
		return ErrPreconditionFailed
	}
	return put(ctx, key, data)
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
	var info struct {
		LastModified string `json:"lastModified"`
		Size         int64  `json:"size"`
		ETag         string `json:"etag"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return ObjectMeta{}, fmt.Errorf("parse mc stat --json: %w", err)
	}
	modTime, err := time.Parse(time.RFC3339, info.LastModified)
	if err != nil {
		return ObjectMeta{}, fmt.Errorf("parse lastModified %q: %w", info.LastModified, err)
	}
	return ObjectMeta{Size: info.Size, ModTime: modTime, ETag: info.ETag}, nil
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
	if err := c.ensureAlias(ctx); err != nil {
		return nil, err
	}
	out, err := c.runMC(ctx, "ls", c.fullPath(prefix))
	if err != nil {
		return nil, err
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// mc ls output format: "[date] [size] filename"
		parts := strings.Fields(line)
		if len(parts) > 0 {
			names = append(names, parts[len(parts)-1])
		}
	}
	return names, nil
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
