import { api } from '../api.js';
import { startPolling } from '../poll.js';
import { escapeHtml, badgeClass } from '../ui.js';
import { openTaskDetail } from './task-detail.js';

const POLL_MS = 15000;

/**
 * renderTasks mounts the manager-task table into `root`. The table's
 * primary source is /api/manager-tasks (the Manager's state.json,
 * passed through opaque-ish, per plan §10.2); for each active task we
 * additionally try to join MinIO's shared/tasks/{id}/meta.json for extra
 * detail (assigned room, project id, etc). A missing meta.json (404) is
 * expected and non-fatal -- the row still renders from state.json alone.
 */
export function renderTasks(root) {
  root.innerHTML = `
    <div class="section-title">Manager Tasks</div>
    <div id="tasks-body"><div class="empty-state">Loading...</div></div>
  `;
  const body = root.querySelector('#tasks-body');

  const stop = startPolling(
    async () => {
      let state;
      try {
        state = await api.managerTasks();
      } catch (err) {
        if (err.status === 404) {
          body.innerHTML =
            '<div class="empty-state">Manager task state is not available (embedded mode only, or the Manager has not run <code>init</code> yet).</div>';
          return;
        }
        throw err;
      }
      await renderTable(body, state);
    },
    POLL_MS,
    (err) => {
      body.innerHTML = `<div class="error-state">Failed to load: ${escapeHtml(err.message)}</div>`;
    },
  );

  return stop;
}

async function renderTable(body, state) {
  const active = state.active_tasks || [];
  const cancelled = state.cancelled_tasks || [];

  if (active.length === 0 && cancelled.length === 0) {
    body.innerHTML = '<div class="empty-state">No tasks tracked yet.</div>';
    return;
  }

  const rows = await Promise.all(active.map((t) => buildRow(t)));

  body.innerHTML = `
    <table>
      <thead>
        <tr>
          <th>Task</th>
          <th>Type</th>
          <th>Assigned To</th>
          <th>Status</th>
          <th>Project</th>
          <th>Blocked Since</th>
        </tr>
      </thead>
      <tbody>
        ${rows.join('')}
      </tbody>
    </table>
    ${
      active.length
        ? '<p class="muted">Click a row to open the task detail panel.</p>'
        : ''
    }
    ${
      cancelled.length
        ? `<div class="section-title">Cancelled (${cancelled.length})</div>
           <div class="file-list">${cancelled
             .map(
               (c) =>
                 `<div class="file-row"><span>${escapeHtml(c.task_id || 'unknown')}</span><span class="file-meta">${escapeHtml(
                   c.cancel_reason || '',
                 )}</span></div>`,
             )
             .join('')}</div>`
        : ''
    }
    ${
      state.last_digest_sent_at
        ? `<p class="muted">Last digest sent: ${escapeHtml(state.last_digest_sent_at)}</p>`
        : ''
    }
  `;

  body.querySelectorAll('tr[data-task-id]').forEach((row) => {
    row.addEventListener('click', () => openTaskDetail(row.dataset.taskId));
  });
}

async function buildRow(task) {
  let projectId = task.project_id || '';
  if (!projectId && task.task_id) {
    try {
      const meta = await api.taskMeta(task.task_id);
      projectId = meta.project_id || '';
    } catch {
      // 404 / unreadable meta.json is expected for tasks that predate the
      // MinIO projection or haven't synced yet -- render from state.json alone.
    }
  }

  const hasId = Boolean(task.task_id);
  return `
    <tr${hasId ? ` data-task-id="${escapeHtml(task.task_id)}" class="row-clickable"` : ''}>
      <td>${escapeHtml(task.task_id || '-')}<br/><span class="muted">${escapeHtml(task.title || '')}</span></td>
      <td>${escapeHtml(task.type || '-')}</td>
      <td>${escapeHtml(task.assigned_to || '-')}</td>
      <td><span class="badge ${badgeClass(task.status)}">${escapeHtml(task.status || 'unknown')}</span></td>
      <td>${projectId ? escapeHtml(projectId) : '<span class="muted">-</span>'}</td>
      <td>${task.blocked_since ? escapeHtml(task.blocked_since) : '<span class="muted">-</span>'}</td>
    </tr>
  `;
}
