'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { createRequestLogger } = require('../src/request-log');

test('createRequestLogger writes one JSON line per call, including a timestamp', () => {
  const lines = [];
  const log = createRequestLogger((line) => lines.push(line));
  log({ action: 'wake', worker: 'bob', status: 200 });
  log({ action: 'sleep', worker: 'alice', status: 200 });

  assert.equal(lines.length, 2);
  const first = JSON.parse(lines[0]);
  assert.equal(first.action, 'wake');
  assert.equal(first.worker, 'bob');
  assert.equal(first.status, 200);
  assert.ok(typeof first.ts === 'string' && first.ts.length > 0);
  assert.ok(lines[0].endsWith('\n'));
});
