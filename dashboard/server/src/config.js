'use strict';

const fs = require('node:fs');

/**
 * loadConfig reads proxy configuration from environment variables.
 * Nothing here hardcodes a bucket name, controller URL, or MinIO endpoint --
 * all of it is env-driven per the brief.
 *
 * @param {NodeJS.ProcessEnv} [env]
 */
function loadConfig(env = process.env) {
  return {
    port: Number(env.PORT || 8090),
    controllerUrl: env.HICLAW_CONTROLLER_URL || 'http://127.0.0.1:8080',
    tokenFile: env.HICLAW_AUTH_TOKEN_FILE || '/var/run/hiclaw/cli-token',
    minio: {
      endpoint: env.MINIO_ENDPOINT || 'http://127.0.0.1:9000',
      accessKey: env.MINIO_ACCESS_KEY || env.MINIO_ACCESS || '',
      secretKey: env.MINIO_SECRET_KEY || env.MINIO_SECRET || '',
      bucket: env.HICLAW_FS_BUCKET || 'hiclaw-storage',
    },
  };
}

// Per-tokenFile in-memory cache: { mtimeMs, token }. Keyed by path so tests
// (and any future multi-file use) don't cross-contaminate.
const tokenCache = new Map();

/**
 * readTokenFile reads and trims the admin token from disk. Throws if the
 * file cannot be read -- callers decide how to surface that (the server
 * treats it as a 502 upstream-config error, never falls back to "no auth").
 *
 * Cached in memory keyed by `tokenFile`; only re-read when the file's mtime
 * has changed (statSync) or the cache has been explicitly invalidated (e.g.
 * on an upstream 401 -- the on-disk token may have been rotated).
 *
 * @param {string} tokenFile
 */
function readTokenFile(tokenFile) {
  const stat = fs.statSync(tokenFile);
  const cached = tokenCache.get(tokenFile);
  if (cached && cached.mtimeMs === stat.mtimeMs) {
    return cached.token;
  }
  const token = fs.readFileSync(tokenFile, 'utf8').trim();
  tokenCache.set(tokenFile, { mtimeMs: stat.mtimeMs, token });
  return token;
}

/**
 * invalidateTokenFile drops the cached token for `tokenFile`, forcing the
 * next readTokenFile call to re-read from disk regardless of mtime. Used
 * when the upstream controller rejects the cached token with a 401, in case
 * the token was rotated without an mtime change we've observed yet.
 *
 * @param {string} tokenFile
 */
function invalidateTokenFile(tokenFile) {
  tokenCache.delete(tokenFile);
}

module.exports = { loadConfig, readTokenFile, invalidateTokenFile };
