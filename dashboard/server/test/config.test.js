'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { loadConfig, readTokenFile, invalidateTokenFile } = require('../src/config');

test('loadConfig applies documented defaults when env is empty', () => {
  const pwDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dashboard-cfg-'));
  const pwFile = path.join(pwDir, 'pw');
  fs.writeFileSync(pwFile, 'secret\n');
  try {
    const cfg = loadConfig({
      AGENTTEAMS_DASHBOARD_USERNAME: 'admin',
      AGENTTEAMS_DASHBOARD_PASSWORD_FILE: pwFile,
    });
    assert.equal(cfg.port, 8090);
    assert.equal(cfg.controllerUrl, 'http://127.0.0.1:8080');
    assert.equal(cfg.tokenFile, '/var/run/agentteams/cli-token');
    assert.equal(cfg.minio.endpoint, 'http://127.0.0.1:9000');
    assert.equal(cfg.minio.bucket, 'agentteams-storage');
  } finally {
    fs.rmSync(pwDir, { recursive: true, force: true });
  }
});

test('loadConfig reads every documented env var, never hardcoding secrets/bucket', () => {
  const pwDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dashboard-cfg-'));
  const pwFile = path.join(pwDir, 'pw');
  fs.writeFileSync(pwFile, 'secret\n');
  try {
    const env = {
      PORT: '9999',
      AGENTTEAMS_DASHBOARD_USERNAME: 'admin',
      AGENTTEAMS_DASHBOARD_PASSWORD_FILE: pwFile,
      HICLAW_CONTROLLER_URL: 'http://controller.internal:8080',
      HICLAW_AUTH_TOKEN_FILE: '/tmp/tok',
      MINIO_ENDPOINT: 'http://minio.internal:9000',
      MINIO_ACCESS_KEY: 'ak',
      MINIO_SECRET_KEY: 'sk',
      HICLAW_FS_BUCKET: 'custom-bucket',
    };
    const cfg = loadConfig(env);
    assert.equal(cfg.port, 9999);
    assert.equal(cfg.controllerUrl, 'http://controller.internal:8080');
    assert.equal(cfg.tokenFile, '/tmp/tok');
    assert.equal(cfg.minio.endpoint, 'http://minio.internal:9000');
    assert.equal(cfg.minio.accessKey, 'ak');
    assert.equal(cfg.minio.secretKey, 'sk');
    assert.equal(cfg.minio.bucket, 'custom-bucket');
  } finally {
    fs.rmSync(pwDir, { recursive: true, force: true });
  }
});

test('readTokenFile trims trailing whitespace/newlines', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dashboard-test-'));
  const file = path.join(dir, 'token');
  fs.writeFileSync(file, 'abc123\n');
  assert.equal(readTokenFile(file), 'abc123');
  fs.rmSync(dir, { recursive: true, force: true });
});

// Fixed, whole-second timestamps so utimesSync round-trips exactly -- avoids
// flakiness from sub-millisecond mtime precision differing across
// filesystems/platforms when comparing before/after stat() results.
const FIXED_MTIME = new Date('2024-01-01T00:00:00.000Z');
const LATER_MTIME = new Date('2024-01-01T00:00:05.000Z');

test('readTokenFile caches in memory and does not re-read the file until mtime changes', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dashboard-test-'));
  const file = path.join(dir, 'token');
  try {
    fs.writeFileSync(file, 'first\n');
    fs.utimesSync(file, FIXED_MTIME, FIXED_MTIME);
    assert.equal(readTokenFile(file), 'first');

    // Rewrite the on-disk content but pin mtime back to the same fixed
    // value: the cache must not notice and must keep returning "first".
    fs.writeFileSync(file, 'second\n');
    fs.utimesSync(file, FIXED_MTIME, FIXED_MTIME);
    assert.equal(readTokenFile(file), 'first');

    // Bump mtime forward -> cache must be invalidated and re-read.
    fs.utimesSync(file, LATER_MTIME, LATER_MTIME);
    assert.equal(readTokenFile(file), 'second');
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('invalidateTokenFile forces a re-read on the next call even without an mtime change', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dashboard-test-'));
  const file = path.join(dir, 'token');
  try {
    fs.writeFileSync(file, 'alpha\n');
    fs.utimesSync(file, FIXED_MTIME, FIXED_MTIME);
    assert.equal(readTokenFile(file), 'alpha');

    fs.writeFileSync(file, 'beta\n');
    fs.utimesSync(file, FIXED_MTIME, FIXED_MTIME); // keep mtime identical

    assert.equal(readTokenFile(file), 'alpha'); // still cached

    invalidateTokenFile(file);
    assert.equal(readTokenFile(file), 'beta'); // forced re-read
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
