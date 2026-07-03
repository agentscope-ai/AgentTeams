import { api } from '../api.js';
import { startPolling } from '../poll.js';
import { escapeHtml } from '../ui.js';
import { latestProgressFile } from '../plan-parse.js';

const REFRESH_MS = 15000;

// Tracks the teardown for whatever invocation of openTaskDetail is currently
// live -- module-level because the dialog itself is a singleton (reused
// across calls via getElementById), so a second openTaskDetail() call must
// tear down the first invocation's poll/listeners before wiring up its own,
// or the stale invocation keeps refreshing the (now-repurposed) body.
let closeCurrent = null;
let currentTaskId = null;

const OUTCOME_RE = /\*\*Status\*\*:\s*(SUCCESS_WITH_NOTES|SUCCESS|REVISION_NEEDED|BLOCKED)/;

/**
 * openTaskDetail renders a drawer (a plain <dialog>, matching the existing
 * confirm/message dialog idiom in index.html/ui.js -- no new DOM scaffolding
 * needed in index.html) showing meta.json + the result.md Outcome badge +
 * the latest progress note for one task id. All three fetches tolerate 404
 * (the tasks.js pattern) -- a task with no meta.json yet, no result.md yet,
 * or no progress/ directory yet all render as an empty sub-section rather
 * than an error.
 *
 * Refreshes every 15s while open (plan §10.2 (4) row 3); stops when closed.
 *
 * @param {string} taskId
 */
export function openTaskDetail(taskId) {
  // Guard against reopening while already open for the same task -- avoid
  // tearing down and re-wiring a poll that's already correctly running.
  if (closeCurrent && currentTaskId === taskId) return;

  // A prior invocation (for a different task, or one whose close() teardown
  // never ran) is still live -- tear it down first so its 15s timer and
  // listeners don't keep firing against this invocation's (repurposed) body.
  if (closeCurrent) {
    closeCurrent();
  }

  let dialog = document.getElementById('task-detail-dialog');
  if (!dialog) {
    dialog = document.createElement('dialog');
    dialog.id = 'task-detail-dialog';
    dialog.className = 'task-detail-dialog';
    dialog.innerHTML = `
      <div class="task-detail-header">
        <span id="task-detail-title"></span>
        <button type="button" id="task-detail-close" aria-label="Close">&times;</button>
      </div>
      <div id="task-detail-body"><div class="empty-state">Loading...</div></div>
    `;
    document.body.appendChild(dialog);
  }

  const body = dialog.querySelector('#task-detail-body');
  const title = dialog.querySelector('#task-detail-title');
  const closeBtn = dialog.querySelector('#task-detail-close');
  title.textContent = taskId;

  async function refresh() {
    try {
      const detail = await loadTaskDetail(taskId);
      renderDetail(body, detail);
    } catch (err) {
      body.innerHTML = `<div class="error-state">Failed to load: ${escapeHtml(err.message)}</div>`;
    }
  }

  const stopPolling = startPolling(refresh, REFRESH_MS, () => {
    /* refresh() already renders its own error-state on failure; nothing
     * further to do here -- startPolling still requires an onError so a
     * thrown error can't otherwise surface as an unhandled rejection. */
  });

  function close() {
    stopPolling();
    closeBtn.removeEventListener('click', close);
    dialog.removeEventListener('close', close);
    dialog.close();
    if (closeCurrent === close) {
      closeCurrent = null;
      currentTaskId = null;
    }
  }

  closeBtn.addEventListener('click', close);
  dialog.addEventListener('close', close);

  closeCurrent = close;
  currentTaskId = taskId;

  dialog.showModal();
}

/**
 * loadTaskDetail fetches meta.json, result.md, and the latest progress note
 * for one task id. Every sub-fetch is independent and 404-tolerant -- a
 * missing meta.json still lets result.md/progress render (and vice versa).
 */
async function loadTaskDetail(taskId) {
  const [meta, outcome, latestProgress] = await Promise.all([
    fetchOrNull(() => api.taskMeta(taskId)),
    fetchOutcome(taskId),
    fetchLatestProgress(taskId),
  ]);
  return { meta, outcome, latestProgress };
}

async function fetchOrNull(fn) {
  try {
    return await fn();
  } catch (err) {
    if (err && err.status === 404) return null;
    throw err;
  }
}

async function fetchOutcome(taskId) {
  const text = await fetchOrNull(() => api.taskResult(taskId));
  if (typeof text !== 'string') return null;
  const m = text.match(OUTCOME_RE);
  return { status: m ? m[1] : null, text };
}

async function fetchLatestProgress(taskId) {
  const listing = await fetchOrNull(() => api.taskProgressList(taskId));
  if (!listing) return null;
  const filename = latestProgressFile(listing.files || []);
  if (!filename) return null;
  const text = await fetchOrNull(() => api.taskProgressFile(taskId, filename));
  return { filename, text };
}

function renderDetail(body, { meta, outcome, latestProgress }) {
  body.innerHTML = `
    <div class="section-title">Meta</div>
    ${meta ? renderMeta(meta) : '<div class="empty-state">meta.json not available.</div>'}
    <div class="section-title">Outcome</div>
    ${renderOutcome(outcome)}
    <div class="section-title">Latest progress</div>
    ${renderProgress(latestProgress)}
  `;
}

function renderMeta(meta) {
  const depends = Array.isArray(meta.depends_on) && meta.depends_on.length ? meta.depends_on.join(', ') : 'none';
  return `
    <div class="card-meta">
      Status: <span class="badge ${outcomeBadgeClass(meta.status)}">${escapeHtml(meta.status || 'unknown')}</span><br/>
      Project: ${escapeHtml(meta.project_id || '-')}<br/>
      Assigned to: ${escapeHtml(meta.assigned_to || '-')}<br/>
      Depends on: ${escapeHtml(depends)}<br/>
      ${meta.assigned_at ? `Assigned at: ${escapeHtml(meta.assigned_at)}<br/>` : ''}
      ${meta.completed_at ? `Completed at: ${escapeHtml(meta.completed_at)}<br/>` : ''}
    </div>
  `;
}

function renderOutcome(outcome) {
  if (!outcome || !outcome.text) {
    return '<div class="empty-state">result.md not available yet.</div>';
  }
  const badge = outcome.status
    ? `<span class="badge ${badgeClassForOutcome(outcome.status)}">${escapeHtml(outcome.status)}</span>`
    : '<span class="badge badge-unknown">unknown</span>';
  return `<div class="card-meta">${badge}</div>`;
}

function renderProgress(latestProgress) {
  if (!latestProgress || typeof latestProgress.text !== 'string') {
    return '<div class="empty-state">No progress notes yet.</div>';
  }
  return `
    <div class="muted">${escapeHtml(latestProgress.filename)}</div>
    <pre class="file-preview">${escapeHtml(latestProgress.text)}</pre>
  `;
}

function outcomeBadgeClass(status) {
  if (status === 'completed') return 'badge-ready';
  if (status === 'assigned') return 'badge-pending';
  return 'badge-unknown';
}

function badgeClassForOutcome(status) {
  if (status === 'SUCCESS' || status === 'SUCCESS_WITH_NOTES') return 'badge-ready';
  if (status === 'BLOCKED') return 'badge-degraded';
  if (status === 'REVISION_NEEDED') return 'badge-degraded';
  return 'badge-unknown';
}
