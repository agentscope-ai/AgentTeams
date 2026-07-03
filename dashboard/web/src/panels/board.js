import { api } from '../api.js';
import { startPolling } from '../poll.js';
import { escapeHtml, badgeClass } from '../ui.js';
import { openTaskDetail } from './task-detail.js';
import { KANBAN_COLUMNS, bucketTasks, capRecentIds } from '../plan-parse.js';

const POLL_MS = 15000;
const COMPLETED_CAP = 50;

/**
 * renderBoard mounts the v2 status kanban (Milestone 3, Step 3): four
 * columns -- Active, Blocked, Completed, Cancelled -- built from the exact
 * same data contracts the Manager Tasks table already uses, no new
 * endpoints. A 404 from /api/manager-tasks (embedded-only feature, or the
 * Manager hasn't run `init` yet) must render four empty columns rather than
 * an error -- the board is a *view* over the same optional data source.
 */
export function renderBoard(root) {
  root.innerHTML = `
    <div class="section-title">Board</div>
    <div id="board-body"><div class="empty-state">Loading...</div></div>
  `;
  const body = root.querySelector('#board-body');

  const stop = startPolling(
    async () => {
      let state = null;
      try {
        state = await api.managerTasks();
      } catch (err) {
        if (err.status !== 404) throw err;
        // 404 = manager-tasks state unavailable -- bucketTasks(undefined, ...)
        // degrades to four empty columns, which is exactly what we want to
        // render here (not an error state).
      }

      const completedIds = await resolveCompletedIds();
      const columns = bucketTasks(state, completedIds);
      renderColumns(body, columns);
    },
    POLL_MS,
    (err) => {
      body.innerHTML = `<div class="error-state">Failed to load: ${escapeHtml(err.message)}</div>`;
    },
  );

  return stop;
}

/**
 * resolveCompletedIds enumerates shared/tasks/ (the proxy's directory-listing
 * fallback, no new route) and joins each candidate id's meta.json status --
 * capped at the ~50 most recent ids (by task-id timestamp prefix) to bound
 * the N+1 meta.json fetches. Any failure here (listing 404s, individual
 * meta.json reads 404) degrades to an empty list rather than throwing --
 * the board still renders Active/Blocked/Cancelled from state.json alone.
 */
async function resolveCompletedIds() {
  let listing;
  try {
    listing = await api.taskList();
  } catch (err) {
    if (err.status === 404) return [];
    throw err;
  }

  const allIds = (listing && listing.directories) || [];
  const candidates = capRecentIds(allIds, COMPLETED_CAP);

  const metas = await Promise.allSettled(candidates.map((id) => api.taskMeta(id)));
  const completed = [];
  metas.forEach((result, i) => {
    if (result.status === 'fulfilled' && result.value && result.value.status === 'completed') {
      completed.push(candidates[i]);
    }
  });
  return completed;
}

function renderColumns(body, columns) {
  body.innerHTML = `
    <div class="board-columns">
      ${KANBAN_COLUMNS.map((col) => renderColumn(col, columns[col] || [])).join('')}
    </div>
  `;

  body.querySelectorAll('[data-task-card]').forEach((el) => {
    el.addEventListener('click', () => openTaskDetail(el.dataset.taskCard));
  });
}

function renderColumn(name, tasks) {
  return `
    <div class="board-column">
      <div class="board-column-header">${escapeHtml(name)} <span class="muted">(${tasks.length})</span></div>
      <div class="board-column-body">
        ${tasks.length ? tasks.map((t) => renderCard(name, t)).join('') : '<div class="empty-state">Nothing here.</div>'}
      </div>
    </div>
  `;
}

function renderCard(column, task) {
  const id = task.task_id || 'unknown';
  const title = task.title || '';
  const clickable = task.task_id ? ` data-task-card="${escapeHtml(task.task_id)}"` : '';
  const extra = [];

  if (column === 'Blocked') {
    if (task.blocked_since) extra.push(`Since: ${escapeHtml(task.blocked_since)}`);
    if (task.blocked_reason) extra.push(escapeHtml(task.blocked_reason));
  } else if (column === 'Cancelled') {
    if (task.cancelled_at) extra.push(`Cancelled: ${escapeHtml(task.cancelled_at)}`);
    if (task.cancel_reason) extra.push(escapeHtml(task.cancel_reason));
  } else if (column === 'Active' && task.status) {
    extra.push(`<span class="badge ${badgeClass(task.status)}">${escapeHtml(task.status)}</span>`);
  }

  return `
    <div class="board-card"${clickable}>
      <div class="board-card-id">${escapeHtml(id)}</div>
      ${title ? `<div class="board-card-title">${escapeHtml(title)}</div>` : ''}
      ${extra.length ? `<div class="board-card-meta">${extra.join('<br/>')}</div>` : ''}
    </div>
  `;
}
