import test from 'node:test';
import assert from 'node:assert/strict';
import { installFakeDocument } from './dom-fakes.js';
import { startPolling } from '../src/poll.js';

test('startPolling ticks immediately and again after intervalMs', async () => {
  installFakeDocument();
  let calls = 0;
  const stop = startPolling(
    async () => {
      calls += 1;
    },
    10,
    () => {},
  );
  await new Promise((r) => setTimeout(r, 5));
  assert.equal(calls, 1);
  await new Promise((r) => setTimeout(r, 20));
  assert.ok(calls >= 2, `expected >=2 calls, got ${calls}`);
  stop();
});

test('startPolling forwards fn errors to onError instead of throwing', async () => {
  installFakeDocument();
  const errors = [];
  const stop = startPolling(
    async () => {
      throw new Error('boom');
    },
    10,
    (err) => errors.push(err.message),
  );
  await new Promise((r) => setTimeout(r, 5));
  assert.deepEqual(errors, ['boom']);
  stop();
});

test('startPolling pauses while document.hidden and resumes with an immediate tick', async () => {
  const fake = installFakeDocument();
  let calls = 0;
  const stop = startPolling(
    async () => {
      calls += 1;
    },
    1000, // long interval so we can tell background scheduling apart from a resume tick
    () => {},
  );
  await new Promise((r) => setTimeout(r, 5));
  assert.equal(calls, 1);

  fake.setHidden(true);
  // While hidden, no further ticks should be scheduled even if we wait past
  // what would have been the next interval.
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(calls, 1);

  fake.setHidden(false);
  await new Promise((r) => setTimeout(r, 5));
  assert.equal(calls, 2, 'expected an immediate tick on becoming visible again');

  stop();
});

test('startPolling.stop() removes the visibilitychange listener (no further ticks fire)', async () => {
  const fake = installFakeDocument();
  let calls = 0;
  const stop = startPolling(
    async () => {
      calls += 1;
    },
    10,
    () => {},
  );
  await new Promise((r) => setTimeout(r, 5));
  const callsAtStop = calls;
  stop();

  fake.setHidden(true);
  fake.setHidden(false);
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(calls, callsAtStop, 'no ticks should fire after stop(), even on visibilitychange');
});
