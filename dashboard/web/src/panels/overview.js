import { api } from '../api.js';
import { startPolling } from '../poll.js';
import { escapeHtml, showToast, confirmAction, promptMessage, badgeClass } from '../ui.js';

const POLL_MANAGERS_TEAMS_MS = 15000;
const POLL_WORKERS_MS = 30000; // workers list does a live backend Status() call per member -- slower cadence

/**
 * renderOverview mounts the Managers/Teams/Workers card grids into `root`
 * and returns a cleanup function that stops all polling (call it when the
 * tab is switched away from).
 */
export function renderOverview(root) {
  root.innerHTML = `
    <div id="managers-section">
      <div class="section-title">Managers</div>
      <div id="managers-cards" class="card-grid"><div class="empty-state">Loading...</div></div>
    </div>
    <div id="teams-section">
      <div class="section-title">Teams</div>
      <div id="teams-cards" class="card-grid"><div class="empty-state">Loading...</div></div>
    </div>
    <div id="workers-section">
      <div class="section-title">Workers</div>
      <div id="workers-cards" class="card-grid"><div class="empty-state">Loading...</div></div>
    </div>
  `;

  const managersEl = root.querySelector('#managers-cards');
  const teamsEl = root.querySelector('#teams-cards');
  const workersEl = root.querySelector('#workers-cards');

  const stopManagers = startPolling(
    async () => {
      const data = await api.listManagers();
      renderManagerCards(managersEl, data.managers || []);
    },
    POLL_MANAGERS_TEAMS_MS,
    (err) => renderError(managersEl, err),
  );

  const stopTeams = startPolling(
    async () => {
      const data = await api.listTeams();
      renderTeamCards(teamsEl, data.teams || []);
    },
    POLL_MANAGERS_TEAMS_MS,
    (err) => renderError(teamsEl, err),
  );

  const stopWorkers = startPolling(
    async () => {
      const data = await api.listWorkers();
      renderWorkerCards(workersEl, data.workers || []);
    },
    POLL_WORKERS_MS,
    (err) => renderError(workersEl, err),
  );

  return () => {
    stopManagers();
    stopTeams();
    stopWorkers();
  };
}

function renderError(el, err) {
  el.innerHTML = `<div class="error-state">Failed to load: ${escapeHtml(err.message)}</div>`;
}

function renderManagerCards(el, managers) {
  if (managers.length === 0) {
    el.innerHTML = '<div class="empty-state">No managers</div>';
    return;
  }
  el.innerHTML = managers
    .map(
      (m) => `
    <div class="card" data-manager="${escapeHtml(m.name)}">
      <div class="card-header">
        <span class="card-name">${escapeHtml(m.name)}</span>
        <span class="badge ${badgeClass(m.phase)}">${escapeHtml(m.phase)}</span>
      </div>
      <div class="card-meta">
        ${m.model ? `Model: ${escapeHtml(m.model)}<br/>` : ''}
        ${m.runtime ? `Runtime: ${escapeHtml(m.runtime)}<br/>` : ''}
        State: ${escapeHtml(m.state || 'unknown')}<br/>
        Welcome sent: ${m.welcomeSent ? 'yes' : 'no'}
      </div>
      <div class="card-actions">
        <button class="action-btn" data-message-kind="manager" data-name="${escapeHtml(m.name)}">Message</button>
      </div>
    </div>`,
    )
    .join('');

  el.querySelectorAll('button[data-message-kind]').forEach((btn) => {
    btn.addEventListener('click', () => onMessageAction(btn));
  });
}

function renderTeamCards(el, teams) {
  if (teams.length === 0) {
    el.innerHTML = '<div class="empty-state">No teams</div>';
    return;
  }
  el.innerHTML = teams
    .map(
      (t) => `
    <div class="card" data-team="${escapeHtml(t.name)}">
      <div class="card-header">
        <span class="card-name">${escapeHtml(t.name)}</span>
        <span class="badge ${badgeClass(t.phase)}">${escapeHtml(t.phase)}</span>
      </div>
      <div class="card-meta">
        Leader: ${escapeHtml(t.leaderName || 'n/a')}<br/>
        Workers ready: ${t.readyWorkers ?? 0} / ${t.totalWorkers ?? 0}<br/>
        Leader ready: ${t.leaderReady ? 'yes' : 'no'}
      </div>
      <div class="card-actions">
        <button class="action-btn" data-message-kind="team" data-name="${escapeHtml(t.name)}">Message</button>
      </div>
    </div>`,
    )
    .join('');

  el.querySelectorAll('button[data-message-kind]').forEach((btn) => {
    btn.addEventListener('click', () => onMessageAction(btn));
  });
}

function renderWorkerCards(el, workers) {
  if (workers.length === 0) {
    el.innerHTML = '<div class="empty-state">No workers</div>';
    return;
  }
  el.innerHTML = workers
    .map(
      (w) => `
    <div class="card" data-worker="${escapeHtml(w.name)}">
      <div class="card-header">
        <span class="card-name">${escapeHtml(w.name)}</span>
        <span class="badge ${badgeClass(w.phase)}">${escapeHtml(w.phase)}</span>
      </div>
      <div class="card-meta">
        ${w.team ? `Team: ${escapeHtml(w.team)}<br/>` : ''}
        State: ${escapeHtml(w.state || 'unknown')}<br/>
        Container: ${escapeHtml(w.containerState || 'unknown')}
      </div>
      <div class="card-actions">
        <button class="action-btn" data-action="wake" data-name="${escapeHtml(w.name)}">Wake</button>
        <button class="action-btn" data-action="sleep" data-name="${escapeHtml(w.name)}">Sleep</button>
        <button class="action-btn" data-action="ensure-ready" data-name="${escapeHtml(w.name)}">Ensure Ready</button>
      </div>
    </div>`,
    )
    .join('');

  el.querySelectorAll('button.action-btn').forEach((btn) => {
    btn.addEventListener('click', () => onWorkerAction(btn));
  });
}

async function onMessageAction(btn) {
  const kind = btn.dataset.messageKind; // 'manager' | 'team'
  const name = btn.dataset.name;
  const body = await promptMessage(`Send a message to ${kind} "${name}":`);
  if (!body) return;

  btn.disabled = true;
  try {
    const res = kind === 'manager' ? await api.messageManager(name, body) : await api.messageTeam(name, body);
    showToast(`${name}: message sent to ${res.roomID}`);
  } catch (err) {
    if (err.status === 409) {
      showToast(`${name}: room not provisioned yet`, { error: true });
    } else {
      showToast(`${name}: message failed -- ${err.message}`, { error: true });
    }
  } finally {
    btn.disabled = false;
  }
}

async function onWorkerAction(btn) {
  const action = btn.dataset.action;
  const name = btn.dataset.name;
  const verbs = { wake: 'wake', sleep: 'sleep', 'ensure-ready': 'ensure-ready for' };
  const confirmed = await confirmAction(`${verbs[action]} worker "${name}"?`);
  if (!confirmed) return;

  const allButtons = btn.closest('.card-actions').querySelectorAll('button');
  allButtons.forEach((b) => (b.disabled = true));
  try {
    if (action === 'wake') await api.wake(name);
    else if (action === 'sleep') await api.sleep(name);
    else if (action === 'ensure-ready') await api.ensureReady(name);
    showToast(`${name}: ${action} sent`);
  } catch (err) {
    showToast(`${name}: ${action} failed -- ${err.message}`, { error: true });
  } finally {
    allButtons.forEach((b) => (b.disabled = false));
  }
}
