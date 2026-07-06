'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { createStaticFileServer } = require('../src/static');

function makeDist() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dashboard-static-test-'));
  fs.writeFileSync(path.join(dir, 'index.html'), '<html>index</html>');
  fs.mkdirSync(path.join(dir, 'assets'));
  fs.writeFileSync(path.join(dir, 'assets', 'app.js'), 'console.log(1)');
  return dir;
}

test('createStaticFileServer serves files from the in-memory cache built at startup', () => {
  const dir = makeDist();
  try {
    const resolve = createStaticFileServer(dir);

    const asset = resolve('/assets/app.js');
    assert.ok(asset);
    assert.equal(asset.contentType, 'text/javascript; charset=utf-8');
    assert.equal(asset.body.toString('utf8'), 'console.log(1)');

    // Mutate the file on disk after the cache was built: resolve() must keep
    // returning the cached (stale) content, proving it never re-reads from
    // disk per request.
    fs.writeFileSync(path.join(dir, 'assets', 'app.js'), 'console.log(2)');
    const cachedAgain = resolve('/assets/app.js');
    assert.equal(cachedAgain.body.toString('utf8'), 'console.log(1)');
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('createStaticFileServer falls back to cached index.html for extensionless paths', () => {
  const dir = makeDist();
  try {
    const resolve = createStaticFileServer(dir);
    const asset = resolve('/some/spa/route');
    assert.ok(asset);
    assert.equal(asset.contentType, 'text/html; charset=utf-8');
    assert.equal(asset.body.toString('utf8'), '<html>index</html>');
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('createStaticFileServer returns null for unknown extensioned paths', () => {
  const dir = makeDist();
  try {
    const resolve = createStaticFileServer(dir);
    assert.equal(resolve('/missing.png'), null);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('createStaticFileServer blocks path traversal outside rootDir', () => {
  const dir = makeDist();
  try {
    const resolve = createStaticFileServer(dir);
    assert.equal(resolve('/../../../../etc/passwd.js'), null);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
