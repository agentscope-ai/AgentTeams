import { api } from '../api.js';
import { startPolling } from '../poll.js';
import { escapeHtml } from '../ui.js';

const POLL_HEALTH_MS = 30000;

const HEALTH_COLUMNS = [
  { state: 'healthy', label: 'Healthy', color: 'var(--ok)' },
  { state: 'idle', label: 'Idle', color: 'var(--text-dim)' },
  { state: 'stalled', label: 'Stalled', color: 'var(--warn)' },
  { state: 'zombie', label: 'Zombie', color: 'var(--bad)' },
];

/**
 * renderHealth mounts a Kanban-style health board into `root`
 * and returns a cleanup function that stops polling.
 */
export function renderHealth(root) {
  root.innerHTML = `
    <div class="health-board">
      ${HEALTH_COLUMNS.map(
        (col) => `
        <div class="health-column" data-state="${col.state}">
          <div class="health-column-header" style="border-top-color: ${col.color}">
            <span class="health-column-title">${col.label}</span>
            <span class="health-column-count" id="count-${col.state}">0</span>
          </div>
          <div class="health-column-body" id="col-${col.state}">
            <div class="empty-state">Loading...</div>
          </div>
        </div>`,
      ).join('')}
    </div>
    <div class="health-uncategorized" id="health-uncategorized" hidden>
      <div class="section-title">Uncategorized</div>
      <div id="col-uncategorized" class="card-grid"></div>
    </div>
  `;

  const stop = startPolling(
    async () => {
      const data = await api.listWorkers();
      renderHealthBoard(root, data.workers || []);
    },
    POLL_HEALTH_MS,
    (err) => {
      root.innerHTML = `<div class="error-state">Failed to load: ${escapeHtml(err.message)}</div>`;
    },
  );

  return stop;
}

function renderHealthBoard(root, workers) {
  const grouped = { healthy: [], idle: [], stalled: [], zombie: [], uncategorized: [] };

  for (const w of workers) {
    const state = w.healthState || '';
    if (grouped[state]) {
      grouped[state].push(w);
    } else {
      grouped.uncategorized.push(w);
    }
  }

  for (const col of HEALTH_COLUMNS) {
    const bodyEl = root.querySelector(`#col-${col.state}`);
    const countEl = root.querySelector(`#count-${col.state}`);
    const items = grouped[col.state];
    countEl.textContent = items.length;

    if (items.length === 0) {
      bodyEl.innerHTML = '<div class="empty-state">None</div>';
    } else {
      bodyEl.innerHTML = items.map((w) => workerCard(w)).join('');
    }
  }

  // Uncategorized (workers with no health state yet)
  const uncategorizedEl = root.querySelector('#health-uncategorized');
  const uncategorizedBody = root.querySelector('#col-uncategorized');
  if (grouped.uncategorized.length > 0) {
    uncategorizedEl.hidden = false;
    uncategorizedBody.innerHTML = grouped.uncategorized.map((w) => workerCard(w)).join('');
  } else {
    uncategorizedEl.hidden = true;
  }
}

function workerCard(w) {
  return `
    <div class="card health-card">
      <div class="card-header">
        <span class="card-name">${escapeHtml(w.name)}</span>
        <span class="badge ${w.phase === 'Running' ? 'badge-running' : 'badge-stopped'}">${escapeHtml(w.phase)}</span>
      </div>
      <div class="card-meta">
        ${w.team ? `Team: ${escapeHtml(w.team)}<br/>` : ''}
        ${w.runtime ? `Runtime: ${escapeHtml(w.runtime)}<br/>` : ''}
        Container: ${escapeHtml(w.containerState || 'unknown')}
      </div>
    </div>`;
}
