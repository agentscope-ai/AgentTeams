'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const { createRequestHandler } = require('../src/handler');

/** Starts a real ephemeral HTTP server wrapping the handler under test. */
function startServer(deps) {
  const handler = createRequestHandler(deps);
  const server = http.createServer((req, res) => {
    handler(req, res).catch((err) => {
      res.writeHead(500);
      res.end(String(err));
    });
  });
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => resolve(server));
  });
}

function request(server, method, path, body) {
  const { port } = server.address();
  return new Promise((resolve, reject) => {
    const req = http.request(
      { host: '127.0.0.1', port, path, method },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () =>
          resolve({ statusCode: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString('utf8') }),
        );
      },
    );
    req.on('error', reject);
    if (body) req.write(body);
    req.end();
  });
}

function fakeControllerClient({ statusCode = 200, body = '{}', headers = {} } = {}) {
  const calls = [];
  return {
    calls,
    async request(method, path, reqBody) {
      calls.push({ method, path, body: reqBody ? reqBody.toString('utf8') : undefined });
      return { statusCode, headers, body: Buffer.from(body) };
    },
  };
}

function fakeMinioClient({ getObjectResult, listObjectsResult } = {}) {
  return {
    async getObject(key) {
      if (typeof getObjectResult === 'function') return getObjectResult(key);
      return getObjectResult || { statusCode: 404, body: Buffer.alloc(0) };
    },
    async listObjects(prefix) {
      if (typeof listObjectsResult === 'function') return listObjectsResult(prefix);
      return listObjectsResult || { prefixes: [], objects: [] };
    },
  };
}

test('GET /api/managers proxies to the controller and relays body/status', async () => {
  const controllerClient = fakeControllerClient({ statusCode: 200, body: '{"managers":[],"total":0}' });
  const minioClient = fakeMinioClient();
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'GET', '/api/managers');
    assert.equal(res.statusCode, 200);
    assert.deepEqual(JSON.parse(res.body), { managers: [], total: 0 });
    assert.equal(controllerClient.calls[0].method, 'GET');
    assert.equal(controllerClient.calls[0].path, '/api/v1/managers');
  } finally {
    server.close();
  }
});

test('token never appears in the relayed response headers/body', async () => {
  // Controller client is a stand-in that would echo an Authorization header
  // if the sanitizer weren't applied upstream of it; here we simulate the
  // controller responding with a header that must never survive relay by
  // asserting handler.js does not itself add or forward Authorization.
  const controllerClient = fakeControllerClient({
    statusCode: 200,
    body: '{"ok":true}',
    headers: { 'x-upstream': 'yes' },
  });
  const minioClient = fakeMinioClient();
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'GET', '/api/teams');
    assert.equal(res.headers.authorization, undefined);
    assert.ok(!res.body.includes('Bearer'));
  } finally {
    server.close();
  }
});

test('unknown path returns 404, unknown method on known path returns 405', async () => {
  const controllerClient = fakeControllerClient();
  const minioClient = fakeMinioClient();
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const notFound = await request(server, 'GET', '/api/nope');
    assert.equal(notFound.statusCode, 404);

    const notAllowed = await request(server, 'DELETE', '/api/workers');
    assert.equal(notAllowed.statusCode, 405);
  } finally {
    server.close();
  }
});

test('the three lifecycle POSTs pass through and are logged', async () => {
  const controllerClient = fakeControllerClient({ statusCode: 200, body: '{"name":"bob","phase":"Running"}' });
  const minioClient = fakeMinioClient();
  const logs = [];
  const server = await startServer({ controllerClient, minioClient, logWrite: (e) => logs.push(e) });
  try {
    const res = await request(server, 'POST', '/api/workers/bob/wake');
    assert.equal(res.statusCode, 200);
    assert.equal(controllerClient.calls[0].path, '/api/v1/workers/bob/wake');
    assert.equal(logs.length, 1);
    assert.equal(logs[0].action, 'wake');
    assert.equal(logs[0].worker, 'bob');
    assert.equal(logs[0].status, 200);
  } finally {
    server.close();
  }
});

test('everything else is GET-only: POST to a list route is rejected before reaching controllerClient', async () => {
  const controllerClient = fakeControllerClient();
  const minioClient = fakeMinioClient();
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'POST', '/api/projects');
    assert.equal(res.statusCode, 405);
    assert.equal(controllerClient.calls.length, 0);
  } finally {
    server.close();
  }
});

test('MinIO object route returns the object body with a guessed content-type', async () => {
  const controllerClient = fakeControllerClient();
  const minioClient = fakeMinioClient({
    getObjectResult: { statusCode: 200, body: Buffer.from('{"task_id":"t1"}') },
  });
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'GET', '/api/tasks/t1/meta.json');
    assert.equal(res.statusCode, 200);
    assert.equal(res.headers['content-type'], 'application/json');
    assert.equal(res.body, '{"task_id":"t1"}');
  } finally {
    server.close();
  }
});

test('MinIO route falls back to a directory listing when the object 404s', async () => {
  const controllerClient = fakeControllerClient();
  const minioClient = fakeMinioClient({
    getObjectResult: { statusCode: 404, body: Buffer.alloc(0) },
    listObjectsResult: {
      prefixes: ['shared/projects/proj-1/'],
      objects: [{ key: 'shared/projects/readme.txt', size: 12, lastModified: '2026-01-01T00:00:00Z' }],
    },
  });
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'GET', '/api/files/shared/projects');
    assert.equal(res.statusCode, 200);
    const parsed = JSON.parse(res.body);
    assert.deepEqual(parsed.directories, ['proj-1']);
    assert.equal(parsed.files.length, 1);
    assert.equal(parsed.files[0].key, 'readme.txt');
  } finally {
    server.close();
  }
});

test('MinIO route 404s when neither an object nor a listing exists', async () => {
  const controllerClient = fakeControllerClient();
  const minioClient = fakeMinioClient({
    getObjectResult: { statusCode: 404, body: Buffer.alloc(0) },
    listObjectsResult: { prefixes: [], objects: [] },
  });
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'GET', '/api/files/shared/nothing-here');
    assert.equal(res.statusCode, 404);
  } finally {
    server.close();
  }
});

test('path traversal on /api/files is rejected before touching MinIO', async () => {
  const controllerClient = fakeControllerClient();
  let minioTouched = false;
  const minioClient = {
    async getObject() {
      minioTouched = true;
      return { statusCode: 404, body: Buffer.alloc(0) };
    },
    async listObjects() {
      minioTouched = true;
      return { prefixes: [], objects: [] };
    },
  };
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'GET', '/api/files/shared/../../etc/passwd');
    assert.equal(res.statusCode, 404);
    assert.equal(minioTouched, false);
  } finally {
    server.close();
  }
});

test('POST /api/managers/{name}/message forwards the body verbatim and logs an audit line', async () => {
  const controllerClient = fakeControllerClient({ statusCode: 200, body: '{"roomID":"!room:x","sent":true}' });
  const minioClient = fakeMinioClient();
  const logs = [];
  const server = await startServer({ controllerClient, minioClient, logWrite: (e) => logs.push(e) });
  try {
    const payload = JSON.stringify({ body: 'please pause new task intake' });
    const res = await request(server, 'POST', '/api/managers/alice/message', payload);
    assert.equal(res.statusCode, 200);
    assert.deepEqual(JSON.parse(res.body), { roomID: '!room:x', sent: true });
    assert.equal(controllerClient.calls[0].method, 'POST');
    assert.equal(controllerClient.calls[0].path, '/api/v1/managers/alice/message');
    assert.equal(controllerClient.calls[0].body, payload);

    assert.equal(logs.length, 1);
    assert.equal(logs[0].action, 'message');
    assert.equal(logs[0].kind, 'managers');
    assert.equal(logs[0].target, 'alice');
    assert.equal(logs[0].status, 200);
    assert.equal(logs[0].bodyLen, Buffer.byteLength(payload));
    assert.equal(logs[0].bodyPreview, payload);
    assert.ok(!('body' in logs[0]), 'the full body must never be logged');
  } finally {
    server.close();
  }
});

test('POST /api/teams/{name}/message works the same as managers', async () => {
  const controllerClient = fakeControllerClient({ statusCode: 200, body: '{"roomID":"!team:x","sent":true}' });
  const minioClient = fakeMinioClient();
  const logs = [];
  const server = await startServer({ controllerClient, minioClient, logWrite: (e) => logs.push(e) });
  try {
    const payload = JSON.stringify({ body: 'ship the hotfix first' });
    const res = await request(server, 'POST', '/api/teams/backend/message', payload);
    assert.equal(res.statusCode, 200);
    assert.equal(controllerClient.calls[0].path, '/api/v1/teams/backend/message');
    assert.equal(logs[0].kind, 'teams');
    assert.equal(logs[0].target, 'backend');
  } finally {
    server.close();
  }
});

test('message audit log preview is truncated to 120 chars, bodyLen reflects the full body', async () => {
  const controllerClient = fakeControllerClient({ statusCode: 200, body: '{"roomID":"!r","sent":true}' });
  const minioClient = fakeMinioClient();
  const logs = [];
  const server = await startServer({ controllerClient, minioClient, logWrite: (e) => logs.push(e) });
  try {
    const longBody = 'x'.repeat(500);
    const payload = JSON.stringify({ body: longBody });
    const res = await request(server, 'POST', '/api/managers/alice/message', payload);
    assert.equal(res.statusCode, 200);
    assert.equal(logs[0].bodyPreview.length, 120);
    assert.equal(logs[0].bodyLen, Buffer.byteLength(payload));
  } finally {
    server.close();
  }
});

test('409/400 from the controller relay through unchanged for message routes', async () => {
  const controllerClient = fakeControllerClient({
    statusCode: 409,
    body: '{"error":"manager admin DM room is not provisioned yet"}',
  });
  const minioClient = fakeMinioClient();
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'POST', '/api/managers/alice/message', JSON.stringify({ body: 'hi' }));
    assert.equal(res.statusCode, 409);
    assert.deepEqual(JSON.parse(res.body), { error: 'manager admin DM room is not provisioned yet' });
  } finally {
    server.close();
  }
});

test('GET on a message path is rejected before reaching controllerClient', async () => {
  const controllerClient = fakeControllerClient();
  const minioClient = fakeMinioClient();
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'GET', '/api/managers/alice/message');
    assert.equal(res.statusCode, 405);
    assert.equal(controllerClient.calls.length, 0);
  } finally {
    server.close();
  }
});

test('oversized message body is rejected with 413 and never reaches the controller', async () => {
  const controllerClient = fakeControllerClient();
  const minioClient = fakeMinioClient();
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const oversized = JSON.stringify({ body: 'x'.repeat(70 * 1024) });
    const res = await request(server, 'POST', '/api/managers/alice/message', oversized);
    assert.equal(res.statusCode, 413);
    assert.equal(controllerClient.calls.length, 0);
  } finally {
    server.close();
  }
});

test('token never appears in message-route response headers/body', async () => {
  const controllerClient = fakeControllerClient({
    statusCode: 200,
    body: '{"roomID":"!r","sent":true}',
    headers: { 'x-upstream': 'yes' },
  });
  const minioClient = fakeMinioClient();
  const server = await startServer({ controllerClient, minioClient, logWrite: () => {} });
  try {
    const res = await request(server, 'POST', '/api/managers/alice/message', JSON.stringify({ body: 'hi' }));
    assert.equal(res.headers.authorization, undefined);
    assert.ok(!res.body.includes('Bearer'));
  } finally {
    server.close();
  }
});

test('/docker/ is never proxied even with a static file resolver present', async () => {
  const controllerClient = fakeControllerClient();
  const minioClient = fakeMinioClient();
  const server = await startServer({
    controllerClient,
    minioClient,
    logWrite: () => {},
    staticFile: () => ({ contentType: 'text/html', body: Buffer.from('<html></html>') }),
  });
  try {
    const res = await request(server, 'GET', '/docker/containers/json');
    assert.equal(res.statusCode, 200); // falls through to staticFile since it's not under /api/
    assert.equal(res.headers['content-type'], 'text/html');
  } finally {
    server.close();
  }
});
