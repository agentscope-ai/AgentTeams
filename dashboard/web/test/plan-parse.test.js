import test from 'node:test';
import assert from 'node:assert/strict';
import {
  countTaskMarkers,
  parsePlanTasks,
  bucketTasks,
  capRecentIds,
  latestProgressFile,
  KANBAN_COLUMNS,
} from '../src/plan-parse.js';

test('countTaskMarkers tallies each marker type', () => {
  const md = [
    '- [ ] task-20260101-000001 — pending one',
    '- [~] task-20260101-000002 — in progress',
    '- [x] task-20260101-000003 — done',
    '- [!] task-20260101-000004 — blocked',
    'not a task line',
  ].join('\n');
  assert.deepEqual(countTaskMarkers(md), { pending: 1, inProgress: 1, done: 1, blocked: 1 });
});

test('countTaskMarkers handles empty/undefined input', () => {
  assert.deepEqual(countTaskMarkers(''), { pending: 0, inProgress: 0, done: 0, blocked: 0 });
  assert.deepEqual(countTaskMarkers(undefined), { pending: 0, inProgress: 0, done: 0, blocked: 0 });
});

test('parsePlanTasks groups tasks by ### Phase heading', () => {
  const md = [
    '# Project: Demo',
    '',
    '## Task Plan',
    '',
    '### Phase 1: Build',
    '',
    '- [ ] task-20260101-000001 — Do the thing (assigned: @alice:example.org)',
    '- [x] task-20260101-000002 — Do another thing (assigned: @bob:example.org, depends on: task-20260101-000001)',
    '',
    '### Phase 2: Ship',
    '',
    '- [!] task-20260101-000003 — Ship it (assigned: @carol:example.org)',
  ].join('\n');

  const { phases } = parsePlanTasks(md);
  assert.equal(phases.length, 2);
  assert.equal(phases[0].title, 'Phase 1: Build');
  assert.equal(phases[0].tasks.length, 2);
  assert.deepEqual(phases[0].tasks[0], {
    id: 'task-20260101-000001',
    title: 'Do the thing',
    marker: ' ',
    assignee: '@alice:example.org',
    dependsOn: null,
  });
  assert.equal(phases[0].tasks[1].dependsOn, 'task-20260101-000001');
  assert.equal(phases[1].title, 'Phase 2: Ship');
  assert.equal(phases[1].tasks[0].marker, '!');
});

test('parsePlanTasks: missing depends-on is null, never throws', () => {
  const md = '### Phase 1: X\n- [ ] task-20260101-000001 — no deps here (assigned: @a:x)';
  const { phases } = parsePlanTasks(md);
  assert.equal(phases[0].tasks[0].dependsOn, null);
});

test('parsePlanTasks: non-conforming lines are skipped, not thrown', () => {
  const md = [
    '### Phase 1: X',
    '- [ ] missing-em-dash-so-skip',
    '  - Spec: /root/hiclaw-fs/shared/tasks/foo/spec.md',
    '- [ ] task-20260101-000001 — Valid line (assigned: @a:x)',
    'random prose that is not a task line at all',
  ].join('\n');
  const { phases } = parsePlanTasks(md);
  assert.equal(phases.length, 1);
  assert.equal(phases[0].tasks.length, 1);
  assert.equal(phases[0].tasks[0].id, 'task-20260101-000001');
});

test('parsePlanTasks: tasks before any heading land in a synthetic phase', () => {
  const md = '- [ ] task-20260101-000001 — orphan task (assigned: @a:x)\n### Phase 1: X\n- [x] task-20260101-000002 — real (assigned: @b:x)';
  const { phases } = parsePlanTasks(md);
  assert.equal(phases.length, 2);
  assert.equal(phases[0].title, '');
  assert.equal(phases[0].tasks[0].id, 'task-20260101-000001');
});

test('parsePlanTasks: empty/garbage input never throws, returns empty phases', () => {
  assert.deepEqual(parsePlanTasks(''), { phases: [] });
  assert.deepEqual(parsePlanTasks(undefined), { phases: [] });
  assert.deepEqual(parsePlanTasks('just some\nrandom\ntext'), { phases: [] });
});

test('KANBAN_COLUMNS is the exact four buckets in display order', () => {
  assert.deepEqual(KANBAN_COLUMNS, ['Active', 'Blocked', 'Completed', 'Cancelled']);
});

test('bucketTasks: empty/undefined state degrades to four empty columns (404 manager-tasks case)', () => {
  const columns = bucketTasks(undefined, undefined);
  assert.deepEqual(columns, { Active: [], Blocked: [], Completed: [], Cancelled: [] });
});

test('bucketTasks: null state and empty completedIds also degrade cleanly', () => {
  const columns = bucketTasks(null, []);
  assert.deepEqual(columns, { Active: [], Blocked: [], Completed: [], Cancelled: [] });
});

test('bucketTasks: status=blocked goes to Blocked, everything else to Active', () => {
  const state = {
    active_tasks: [
      { task_id: 't1', status: 'blocked', blocked_reason: 'waiting on review' },
      { task_id: 't2' },
      { task_id: 't3', status: 'some-unrecognized-status' },
    ],
  };
  const columns = bucketTasks(state, []);
  assert.equal(columns.Blocked.length, 1);
  assert.equal(columns.Blocked[0].task_id, 't1');
  assert.equal(columns.Active.length, 2);
  // Unknown status string is preserved on the task, not normalized away.
  assert.equal(columns.Active.find((t) => t.task_id === 't3').status, 'some-unrecognized-status');
});

test('bucketTasks: cancelled_tasks map straight to Cancelled', () => {
  const state = { cancelled_tasks: [{ task_id: 'c1', cancel_reason: 'no longer needed' }] };
  const columns = bucketTasks(state, []);
  assert.equal(columns.Cancelled.length, 1);
  assert.equal(columns.Cancelled[0].task_id, 'c1');
});

test('bucketTasks: Completed = completedIds minus active ids', () => {
  const state = { active_tasks: [{ task_id: 't1' }] };
  const columns = bucketTasks(state, ['t1', 't2', 't3']);
  const ids = columns.Completed.map((t) => t.task_id);
  assert.deepEqual(ids, ['t2', 't3']);
});

test('bucketTasks: malformed active_tasks entries (null/falsy) are skipped', () => {
  const state = { active_tasks: [null, undefined, { task_id: 't1' }] };
  const columns = bucketTasks(state, []);
  assert.equal(columns.Active.length, 1);
});

test('capRecentIds: sorts task-YYYYMMDD-HHMMSS ids descending and caps at limit', () => {
  const ids = ['task-20260101-000001', 'task-20260301-120000', 'task-20260201-060000'];
  const capped = capRecentIds(ids, 2);
  assert.deepEqual(capped, ['task-20260301-120000', 'task-20260201-060000']);
});

test('capRecentIds: default limit is 50', () => {
  const ids = Array.from({ length: 60 }, (_, i) => `task-2026010${String(i).padStart(2, '0').slice(0, 2)}-000000`);
  const capped = capRecentIds(ids);
  assert.equal(capped.length, 50);
});

test('capRecentIds: non-conforming ids still sort (never thrown), timestamped ids come first', () => {
  const ids = ['weird-id', 'task-20260101-000001', 'another-weird-one'];
  const capped = capRecentIds(ids, 10);
  assert.equal(capped[0], 'task-20260101-000001');
  assert.equal(capped.length, 3);
});

test('capRecentIds: empty/undefined input returns empty array', () => {
  assert.deepEqual(capRecentIds(undefined), []);
  assert.deepEqual(capRecentIds([]), []);
});

test('latestProgressFile: picks the highest filename (YYYY-MM-DD.md sorts chronologically)', () => {
  const files = [{ key: '2026-01-01.md' }, { key: '2026-03-15.md' }, { key: '2026-02-10.md' }];
  assert.equal(latestProgressFile(files), '2026-03-15.md');
});

test('latestProgressFile: accepts plain string arrays too', () => {
  assert.equal(latestProgressFile(['2026-01-01.md', '2026-01-02.md']), '2026-01-02.md');
});

test('latestProgressFile: empty/missing listing returns null, never throws', () => {
  assert.equal(latestProgressFile([]), null);
  assert.equal(latestProgressFile(undefined), null);
  assert.equal(latestProgressFile(null), null);
});
