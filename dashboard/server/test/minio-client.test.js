'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const { MinioClient } = require('../src/minio-client');
const { signRequest } = require('../src/sigv4');

function startUpstream(handler) {
  const server = http.createServer(handler);
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server)));
}

test('signRequest produces a well-formed Authorization header', () => {
  const { headers } = signRequest({
    method: 'GET',
    host: '127.0.0.1:9000',
    path: '/mybucket/shared/tasks/t1/meta.json',
    query: {},
    accessKey: 'AKIAEXAMPLE',
    secretKey: 'secretkey',
    date: new Date('2026-07-03T00:00:00Z'),
  });
  assert.match(headers.authorization, /^AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE\//);
  assert.match(headers.authorization, /SignedHeaders=host;x-amz-content-sha256;x-amz-date/);
  assert.equal(headers['x-amz-date'], '20260703T000000Z');
});

test('MinioClient.getObject signs the request and returns the upstream body', async () => {
  let seenAuth;
  let seenPath;
  const upstream = await startUpstream((req, res) => {
    seenAuth = req.headers.authorization;
    seenPath = req.url;
    if (req.url === '/hiclaw-storage/shared/tasks/t1/meta.json') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end('{"task_id":"t1"}');
    } else {
      res.writeHead(404);
      res.end();
    }
  });
  try {
    const { port } = upstream.address();
    const client = new MinioClient({
      endpoint: `http://127.0.0.1:${port}`,
      accessKey: 'ak',
      secretKey: 'sk',
      bucket: 'hiclaw-storage',
    });
    const res = await client.getObject('shared/tasks/t1/meta.json');
    assert.equal(res.statusCode, 200);
    assert.equal(res.body.toString('utf8'), '{"task_id":"t1"}');
    assert.ok(seenAuth.startsWith('AWS4-HMAC-SHA256'));
    assert.equal(seenPath, '/hiclaw-storage/shared/tasks/t1/meta.json');
  } finally {
    upstream.close();
  }
});

test('MinioClient.getObject percent-encodes a key with spaces on the wire while keeping the SignedHeaders shape unchanged', async () => {
  let seenPath;
  let seenAuth;
  const upstream = await startUpstream((req, res) => {
    seenPath = req.url;
    seenAuth = req.headers.authorization;
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end('{"task_id":"t1"}');
  });
  let seenAuthPlainAscii;
  const upstream2 = await startUpstream((req, res) => {
    seenAuthPlainAscii = req.headers.authorization;
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end('{"task_id":"t1"}');
  });
  try {
    const { port } = upstream.address();
    const client = new MinioClient({
      endpoint: `http://127.0.0.1:${port}`,
      accessKey: 'ak',
      secretKey: 'sk',
      bucket: 'hiclaw-storage',
    });
    const res = await client.getObject('shared/tasks/a b/meta.json');
    assert.equal(res.statusCode, 200);
    // The request line actually sent on the wire must be percent-encoded --
    // a raw space would both break the HTTP request line and mismatch the
    // signature MinIO recomputes from the real path (SignatureDoesNotMatch).
    assert.equal(seenPath, '/hiclaw-storage/shared/tasks/a%20b/meta.json');
    assert.ok(seenAuth.startsWith('AWS4-HMAC-SHA256'));

    const { port: port2 } = upstream2.address();
    const client2 = new MinioClient({
      endpoint: `http://127.0.0.1:${port2}`,
      accessKey: 'ak',
      secretKey: 'sk',
      bucket: 'hiclaw-storage',
    });
    await client2.getObject('shared/tasks/t1/meta.json');

    // The SignedHeaders portion of the Authorization header must be
    // byte-identical to a plain-ASCII key -- encoding only ever affects the
    // canonical URI, never which headers are signed.
    const signedHeadersOf = (auth) => auth.match(/SignedHeaders=([^,]*)/)[1];
    assert.equal(signedHeadersOf(seenAuth), signedHeadersOf(seenAuthPlainAscii));
    assert.equal(signedHeadersOf(seenAuth), 'host;x-amz-content-sha256;x-amz-date');
  } finally {
    upstream.close();
    upstream2.close();
  }
});

test('MinioClient.listObjects parses ListObjectsV2 XML into prefixes/objects', async () => {
  const xml = `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult>
  <Name>hiclaw-storage</Name>
  <Prefix>shared/projects/</Prefix>
  <CommonPrefixes><Prefix>shared/projects/proj-1/</Prefix></CommonPrefixes>
  <Contents>
    <Key>shared/projects/readme.txt</Key>
    <Size>42</Size>
    <LastModified>2026-01-01T00:00:00.000Z</LastModified>
  </Contents>
</ListBucketResult>`;
  const upstream = await startUpstream((req, res) => {
    res.writeHead(200, { 'content-type': 'application/xml' });
    res.end(xml);
  });
  try {
    const { port } = upstream.address();
    const client = new MinioClient({
      endpoint: `http://127.0.0.1:${port}`,
      accessKey: 'ak',
      secretKey: 'sk',
      bucket: 'hiclaw-storage',
    });
    const listing = await client.listObjects('shared/projects/');
    assert.deepEqual(listing.prefixes, ['shared/projects/proj-1/']);
    assert.equal(listing.objects.length, 1);
    assert.equal(listing.objects[0].key, 'shared/projects/readme.txt');
    assert.equal(listing.objects[0].size, 42);
  } finally {
    upstream.close();
  }
});

test('MinioClient.getObject forwards conditional headers unsigned, without changing SignedHeaders', async () => {
  let seenAuthWithConditional;
  let seenIfNoneMatch;
  let seenIfModifiedSince;
  const upstream = await startUpstream((req, res) => {
    if (req.url === '/hiclaw-storage/shared/tasks/t1/meta.json') {
      seenAuthWithConditional = req.headers.authorization;
      seenIfNoneMatch = req.headers['if-none-match'];
      seenIfModifiedSince = req.headers['if-modified-since'];
      res.writeHead(200, { 'content-type': 'application/json', etag: '"abc123"', 'last-modified': 'Thu, 01 Jan 2026 00:00:00 GMT' });
      res.end('{"task_id":"t1"}');
    } else {
      res.writeHead(404);
      res.end();
    }
  });
  let seenAuthNoConditional;
  const upstream2 = await startUpstream((req, res) => {
    seenAuthNoConditional = req.headers.authorization;
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end('{"task_id":"t1"}');
  });
  try {
    const { port } = upstream.address();
    const client = new MinioClient({
      endpoint: `http://127.0.0.1:${port}`,
      accessKey: 'ak',
      secretKey: 'sk',
      bucket: 'hiclaw-storage',
    });
    const res = await client.getObject('shared/tasks/t1/meta.json', {
      conditionalHeaders: { 'if-none-match': '"abc123"', 'if-modified-since': 'Wed, 31 Dec 2025 00:00:00 GMT' },
    });
    assert.equal(res.statusCode, 200);
    assert.equal(res.headers.etag, '"abc123"');
    assert.equal(seenIfNoneMatch, '"abc123"');
    assert.equal(seenIfModifiedSince, 'Wed, 31 Dec 2025 00:00:00 GMT');

    const { port: port2 } = upstream2.address();
    const client2 = new MinioClient({
      endpoint: `http://127.0.0.1:${port2}`,
      accessKey: 'ak',
      secretKey: 'sk',
      bucket: 'hiclaw-storage',
    });
    await client2.getObject('shared/tasks/t1/meta.json');

    // The SignedHeaders portion of the Authorization header must be
    // byte-identical whether or not conditional headers were supplied --
    // they are merged onto the transport request AFTER signing and must
    // never appear in SignedHeaders (plan Milestone 3, Step 2).
    const signedHeadersOf = (auth) => auth.match(/SignedHeaders=([^,]*)/)[1];
    assert.equal(signedHeadersOf(seenAuthWithConditional), signedHeadersOf(seenAuthNoConditional));
    assert.equal(signedHeadersOf(seenAuthWithConditional), 'host;x-amz-content-sha256;x-amz-date');
  } finally {
    upstream.close();
    upstream2.close();
  }
});

test('MinioClient.getObject relays a 304 from upstream with no body expectation', async () => {
  const upstream = await startUpstream((req, res) => {
    res.writeHead(304, {});
    res.end();
  });
  try {
    const { port } = upstream.address();
    const client = new MinioClient({
      endpoint: `http://127.0.0.1:${port}`,
      accessKey: 'ak',
      secretKey: 'sk',
      bucket: 'hiclaw-storage',
    });
    const res = await client.getObject('shared/tasks/t1/meta.json', {
      conditionalHeaders: { 'if-none-match': '"abc123"' },
    });
    assert.equal(res.statusCode, 304);
  } finally {
    upstream.close();
  }
});

test('MinioClient.listObjects throws on an error status', async () => {
  const upstream = await startUpstream((req, res) => {
    res.writeHead(403, { 'content-type': 'application/xml' });
    res.end('<Error><Code>AccessDenied</Code></Error>');
  });
  try {
    const { port } = upstream.address();
    const client = new MinioClient({
      endpoint: `http://127.0.0.1:${port}`,
      accessKey: 'ak',
      secretKey: 'sk',
      bucket: 'hiclaw-storage',
    });
    await assert.rejects(() => client.listObjects('shared/'));
  } finally {
    upstream.close();
  }
});
