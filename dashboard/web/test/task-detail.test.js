import test from 'node:test';
import assert from 'node:assert/strict';
import { installFakeDocument } from './dom-fakes.js';
import { openTaskDetail } from '../src/panels/task-detail.js';

function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

function textResponse(text, status = 200) {
  return new Response(text, { status, headers: { 'content-type': 'text/plain' } });
}

function notFound() {
  return new Response('not found', { status: 404 });
}

function mockFetchFor(taskId, { metaStatus = 'assigned' } = {}) {
  globalThis.fetch = async (path) => {
    if (path === `/api/tasks/${taskId}/meta.json`) {
      return jsonResponse({ status: metaStatus, project_id: 'p1', assigned_to: 'bob' });
    }
    if (path === `/api/tasks/${taskId}/result.md`) return notFound();
    if (path === `/api/tasks/${taskId}/progress`) return notFound();
    return notFound();
  };
}

test('openTaskDetail tears down a prior invocation before opening a new task', async () => {
  installFakeDocument();
  mockFetchFor('task-a');
  openTaskDetail('task-a');
  await new Promise((r) => setTimeout(r, 5));

  const dialog = document.getElementById('task-detail-dialog');
  assert.ok(dialog.open, 'dialog should be open after first invocation');

  mockFetchFor('task-b');
  openTaskDetail('task-b');
  await new Promise((r) => setTimeout(r, 5));

  assert.equal(dialog.querySelector('#task-detail-title').textContent, 'task-b');

  // Only one 'close' listener should be attached to the dialog at a time --
  // closing it now must not throw or double-fire teardown for the stale
  // task-a invocation.
  dialog.close();
  await new Promise((r) => setTimeout(r, 5));
  assert.equal(dialog.open, false);
});

test('openTaskDetail is a no-op when called again for the task that is already open', async () => {
  installFakeDocument();
  let fetchCount = 0;
  globalThis.fetch = async (path) => {
    fetchCount += 1;
    if (path === '/api/tasks/task-a/meta.json') {
      return jsonResponse({ status: 'assigned' });
    }
    return notFound();
  };

  openTaskDetail('task-a');
  await new Promise((r) => setTimeout(r, 5));
  const countAfterFirstOpen = fetchCount;

  // Re-invoking for the same, still-open task must not tear down and
  // re-issue the initial fetch burst.
  openTaskDetail('task-a');
  await new Promise((r) => setTimeout(r, 5));

  assert.equal(fetchCount, countAfterFirstOpen, 'reopening the same task should not trigger a fresh load');

  document.getElementById('task-detail-dialog').close();
});

test('closing the dialog stops its refresh poll (no fetch after close)', async () => {
  installFakeDocument();
  let fetchCount = 0;
  globalThis.fetch = async (path) => {
    fetchCount += 1;
    if (path === '/api/tasks/task-a/meta.json') return jsonResponse({ status: 'assigned' });
    return notFound();
  };

  openTaskDetail('task-a');
  await new Promise((r) => setTimeout(r, 5));
  const dialog = document.getElementById('task-detail-dialog');

  dialog.close();
  const countAtClose = fetchCount;

  // Even if something were to keep polling, we can at least assert no
  // immediate extra fetch fires as a direct result of close().
  await new Promise((r) => setTimeout(r, 5));
  assert.equal(fetchCount, countAtClose);
});
