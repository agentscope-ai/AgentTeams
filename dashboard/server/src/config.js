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

/**
 * readTokenFile reads and trims the admin token from disk. Throws if the
 * file cannot be read -- callers decide how to surface that (the server
 * treats it as a 502 upstream-config error, never falls back to "no auth").
 *
 * @param {string} tokenFile
 */
function readTokenFile(tokenFile) {
  return fs.readFileSync(tokenFile, 'utf8').trim();
}

module.exports = { loadConfig, readTokenFile };
