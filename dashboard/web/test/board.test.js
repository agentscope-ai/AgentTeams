import test from 'node:test';
import assert from 'node:assert/strict';
import { installFakeDocument } from './dom-fakes.js';
import { renderBoard } from '../src/panels/board.js';

// renderBoard's #board-body is only ever touched via innerHTML (write) and
// querySelectorAll('[data-task-card]') (read, over the just-assigned HTML,
// to wire up click handlers) -- this fake covers exactly that surface
// without pulling in a full DOM.
class FakeBody {
  constructor() {
    this._html = '';
  }
  set innerHTML(v) {
    this._html = v;
  }
  get innerHTML() {
    return this._html;
  }
  querySelectorAll() {
    return [];
  }
}

class FakeRoot {
  constructor() {
    this.body = new FakeBody();
  }
  set innerHTML(_v) {
    // renderBoard() sets root.innerHTML once up front to seed #board-body;
    // the panel then only ever touches the #board-body element it captured.
  }
  querySelector(sel) {
    if (sel === '#board-body') return this.body;
    return null;
  }
}

function mockFetch(handlers) {
  globalThis.fetch = async (path) => {
    for (const [pattern, respond] of handlers) {
      const match = typeof pattern === 'string' ? path === pattern : pattern.test(path);
      if (match) return respond(path);
    }
    throw new Error(`unmocked fetch: ${path}`);
  };
}

function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

test('renderBoard fetches manager-tasks and the task listing concurrently, not serially', async () => {
  installFakeDocument();
  const callOrder = [];
  let resolveManagerTasks;
  let resolveTaskList;

  mockFetch([
    [
      '/api/manager-tasks',
      () =>
        new Promise((resolve) => {
          callOrder.push('managerTasks-start');
          resolveManagerTasks = () => {
            callOrder.push('managerTasks-end');
            resolve(jsonResponse({ active_tasks: [], cancelled_tasks: [] }));
          };
        }),
    ],
    [
      '/api/tasks/',
      () =>
        new Promise((resolve) => {
          callOrder.push('taskList-start');
          resolveTaskList = () => {
            callOrder.push('taskList-end');
            resolve(jsonResponse({ directories: [], files: [] }));
          };
        }),
    ],
  ]);

  const root = new FakeRoot();
  const stop = renderBoard(root);

  // Give the initial tick a chance to issue both fetches before either
  // resolves -- if they were serial, taskList-start would not appear until
  // after managerTasks-end.
  await new Promise((r) => setTimeout(r, 5));
  assert.deepEqual(callOrder, ['managerTasks-start', 'taskList-start']);

  resolveManagerTasks();
  resolveTaskList();
  await new Promise((r) => setTimeout(r, 5));

  stop();
});

test('renderBoard degrades a 404 from /api/manager-tasks to empty columns instead of an error state', async () => {
  installFakeDocument();
  mockFetch([
    ['/api/manager-tasks', () => new Response('not found', { status: 404 })],
    ['/api/tasks/', () => jsonResponse({ directories: [], files: [] })],
  ]);

  const root = new FakeRoot();
  const stop = renderBoard(root);
  await new Promise((r) => setTimeout(r, 5));

  assert.ok(!root.body.innerHTML.includes('error-state'), `expected no error-state, got: ${root.body.innerHTML}`);
  assert.ok(root.body.innerHTML.includes('board-columns'));
  stop();
});

test('renderBoard surfaces a non-404 /api/manager-tasks failure as an error state', async () => {
  installFakeDocument();
  mockFetch([
    ['/api/manager-tasks', () => new Response('boom', { status: 500 })],
    ['/api/tasks/', () => jsonResponse({ directories: [], files: [] })],
  ]);

  const root = new FakeRoot();
  const stop = renderBoard(root);
  await new Promise((r) => setTimeout(r, 5));

  assert.ok(root.body.innerHTML.includes('error-state'), `expected error-state, got: ${root.body.innerHTML}`);
  stop();
});
