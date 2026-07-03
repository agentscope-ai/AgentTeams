import { api } from '../api.js';
import { startPolling } from '../poll.js';
import { escapeHtml, badgeClass } from '../ui.js';
import { countTaskMarkers, parsePlanTasks } from '../plan-parse.js';

const POLL_MS = 15000;

// Re-exported for any external caller that imported countTaskMarkers from
// here pre-v2 -- the implementation itself now lives in plan-parse.js
// (Milestone 3, Step 3) so the DAG view and the progress-counts view above
// share one parser instead of forking it.
export { countTaskMarkers };

/**
 * renderProjects mounts the Project browser: one card per Project CRD
 * (from GET /api/projects, Step 1), joined by id with the chat-flow layer
 * under shared/projects/{id}/meta.json + plan.md (decision #16 -- federated,
 * never schema-merged; we only ever render them side by side here). Task
 * progress is summarized as [ ]/[~]/[x] counts parsed out of plan.md, plus
 * (v2, Milestone 3 Step 3) an expandable dependency-ordered phase/DAG view
 * of the same plan.md, parsed by plan-parse.js. Unparseable plan.md falls
 * back to the marker-count view only (ledger #3 -- never throws).
 */
export function renderProjects(root) {
  root.innerHTML = `
    <div class="section-title">Projects</div>
    <div id="projects-body"><div class="empty-state">Loading...</div></div>
  `;
  const body = root.querySelector('#projects-body');

  const stop = startPolling(
    async () => {
      const data = await api.listProjects();
      await renderList(body, data.projects || []);
    },
    POLL_MS,
    (err) => {
      body.innerHTML = `<div class="error-state">Failed to load: ${escapeHtml(err.message)}</div>`;
    },
  );

  return stop;
}

async function renderList(body, projects) {
  if (projects.length === 0) {
    body.innerHTML = '<div class="empty-state">No projects</div>';
    return;
  }

  const cards = await Promise.all(projects.map((p) => buildCard(p)));
  body.innerHTML = `<div class="card-grid">${cards.join('')}</div>`;
}

async function buildCard(project) {
  const id = project.name;
  let planCounts = null;
  let planText = null;
  let chatStatus = null;

  const [metaResult, planResult] = await Promise.allSettled([
    api.fileGetJson(['shared', 'projects', id, 'meta.json']),
    api.fileGetText(['shared', 'projects', id, 'plan.md']),
  ]);

  if (metaResult.status === 'fulfilled') {
    chatStatus = metaResult.value.status;
  }
  if (planResult.status === 'fulfilled' && typeof planResult.value === 'string') {
    planText = planResult.value;
    planCounts = countTaskMarkers(planText);
  }

  const repoLines = (project.repos || [])
    .map((r) => `<li>${escapeHtml(r.name || r.url)} <span class="muted">(${escapeHtml(r.access)})</span></li>`)
    .join('');

  return `
    <div class="card" data-project="${escapeHtml(id)}">
      <div class="card-header">
        <span class="card-name">${escapeHtml(project.projectName || id)}</span>
        <span class="badge ${badgeClass(project.phase)}">${escapeHtml(project.phase)}</span>
      </div>
      <div class="card-meta">
        Team: ${escapeHtml(project.team)}<br/>
        Repos: ${project.repoCount ?? (project.repos || []).length}<br/>
        ${chatStatus ? `Chat-flow status: ${escapeHtml(chatStatus)}<br/>` : ''}
        ${planCounts ? renderProgress(planCounts) : '<span class="muted">plan.md not yet available</span>'}
      </div>
      ${repoLines ? `<ul class="card-meta">${repoLines}</ul>` : ''}
      ${planText !== null ? renderPlanExpander(id, planText) : ''}
    </div>
  `;
}

/**
 * renderPlanExpander builds a collapsible <details> holding the
 * dependency-ordered phase/task list parsed out of plan.md (v2 DAG view).
 * A plan.md that fails to parse into any recognizable task line (empty
 * `phases`) falls back silently to nothing here -- the marker-count view
 * above already covers that case, so this never shows an error state.
 */
function renderPlanExpander(id, planText) {
  const { phases } = parsePlanTasks(planText);
  if (phases.length === 0) return '';

  const phaseBlocks = phases
    .map((phase) => {
      const rows = phase.tasks
        .map((t) => {
          const marker = markerLabel(t.marker);
          const dep = t.dependsOn
            ? `<span class="muted"> &larr; depends on ${escapeHtml(t.dependsOn)}</span>`
            : '';
          const who = t.assignee ? `<span class="muted"> (${escapeHtml(t.assignee)})</span>` : '';
          return `<li class="dag-task"><span class="badge ${marker.cls}">${marker.label}</span> <span class="muted">${escapeHtml(t.id)}</span> — ${escapeHtml(t.title)}${who}${dep}</li>`;
        })
        .join('');
      const title = phase.title ? escapeHtml(phase.title) : '(no phase heading)';
      return `<div class="dag-phase"><div class="dag-phase-title">${title}</div><ul class="dag-task-list">${rows}</ul></div>`;
    })
    .join('');

  return `
    <details class="plan-expander" data-project-plan="${escapeHtml(id)}">
      <summary>Plan (${phases.reduce((n, p) => n + p.tasks.length, 0)} tasks)</summary>
      <div class="dag-view">${phaseBlocks}</div>
    </details>
  `;
}

function markerLabel(marker) {
  switch (marker) {
    case 'x':
      return { label: 'done', cls: 'badge-ready' };
    case '~':
      return { label: 'in progress', cls: 'badge-pending' };
    case '!':
      return { label: 'blocked', cls: 'badge-degraded' };
    case '→':
      return { label: 'revision', cls: 'badge-degraded' };
    default:
      return { label: 'pending', cls: 'badge-unknown' };
  }
}

function renderProgress(counts) {
  return `
    <div class="progress-counts">
      <span>[ ] ${counts.pending}</span>
      <span>[~] ${counts.inProgress}</span>
      <span>[x] ${counts.done}</span>
      ${counts.blocked ? `<span>[!] ${counts.blocked}</span>` : ''}
    </div>
  `;
}
