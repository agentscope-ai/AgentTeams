'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const { ControllerClient } = require('../src/controller-client');

function startUpstream(handler) {
  const server = http.createServer(handler);
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server)));
}

test('ControllerClient injects the Bearer token and never forwards it back out', async () => {
  let seenAuth;
  const upstream = await startUpstream((req, res) => {
    seenAuth = req.headers.authorization;
    res.writeHead(200, { 'content-type': 'application/json', authorization: 'Bearer leaked-if-not-stripped' });
    res.end('{"ok":true}');
  });
  try {
    const { port } = upstream.address();
    const client = new ControllerClient({ baseUrl: `http://127.0.0.1:${port}`, getToken: () => 'admin-secret-token' });
    const res = await client.request('GET', '/api/v1/workers');
    assert.equal(seenAuth, 'Bearer admin-secret-token');
    assert.equal(res.headers.authorization, undefined);
    assert.equal(res.body.toString('utf8'), '{"ok":true}');
  } finally {
    upstream.close();
  }
});

test('ControllerClient forwards a request body for write calls', async () => {
  let receivedBody = '';
  const upstream = await startUpstream((req, res) => {
    req.on('data', (c) => (receivedBody += c));
    req.on('end', () => {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end('{"name":"bob","phase":"Running"}');
    });
  });
  try {
    const { port } = upstream.address();
    const client = new ControllerClient({ baseUrl: `http://127.0.0.1:${port}`, getToken: () => 'tok' });
    const res = await client.request('POST', '/api/v1/workers/bob/wake', Buffer.from('{}'));
    assert.equal(receivedBody, '{}');
    assert.equal(res.statusCode, 200);
  } finally {
    upstream.close();
  }
});
