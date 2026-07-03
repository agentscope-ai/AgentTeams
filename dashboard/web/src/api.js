// Thin fetch wrapper for the same-origin dashboard proxy. Every call is a
// relative /api/... path -- the SPA never talks to the controller or MinIO
// directly, and never sees the admin token (the proxy injects it).

async function request(path, opts) {
  const res = await fetch(path, opts);
  if (res.status === 404) {
    const err = new Error(`not found: ${path}`);
    err.status = 404;
    throw err;
  }
  if (!res.ok) {
    let detail = '';
    try {
      detail = await res.text();
    } catch {
      /* ignore */
    }
    const err = new Error(`request failed (${res.status}): ${path} ${detail}`);
    err.status = res.status;
    throw err;
  }
  const contentType = res.headers.get('content-type') || '';
  if (contentType.includes('application/json')) {
    return res.json();
  }
  return res.text();
}

export const api = {
  listManagers: () => request('/api/managers'),
  listTeams: () => request('/api/teams'),
  listWorkers: () => request('/api/workers'),
  managerTasks: () => request('/api/manager-tasks'),
  listProjects: () => request('/api/projects'),

  // MinIO-backed reads. Callers should catch 404 (missing file) themselves --
  // it's an expected condition (e.g. a project with no plan.md yet).
  taskMeta: (taskId) => request(`/api/tasks/${encodeURIComponent(taskId)}/meta.json`),
  taskResult: (taskId) => request(`/api/tasks/${encodeURIComponent(taskId)}/result.md`),
  taskProgressList: (taskId) => request(`/api/tasks/${encodeURIComponent(taskId)}/progress`),
  taskProgressFile: (taskId, filename) =>
    request(`/api/tasks/${encodeURIComponent(taskId)}/progress/${encodeURIComponent(filename)}`),
  // Directory listing of shared/tasks/ itself -- no rest segment, per
  // allowlist.js `/api/tasks/<...rest>` (rest may be empty), falls back to
  // the proxy's list-on-404 behavior (handler.js) and returns
  // {directories:[taskIds], files:[]}. Used by the v2 board's Completed
  // column to enumerate known task ids (Milestone 3, Step 3).
  taskList: () => request('/api/tasks/'),
  fileBrowse: (pathSegments) => request(`/api/files/${pathSegments.map(encodeURIComponent).join('/')}`),
  fileGetJson: (pathSegments) => request(`/api/files/${pathSegments.map(encodeURIComponent).join('/')}`),
  fileGetText: (pathSegments) => request(`/api/files/${pathSegments.map(encodeURIComponent).join('/')}`),

  // Lifecycle writes (v1.1). Confirmation is handled by the caller (UI layer)
  // before invoking these.
  wake: (name) => request(`/api/workers/${encodeURIComponent(name)}/wake`, { method: 'POST' }),
  sleep: (name) => request(`/api/workers/${encodeURIComponent(name)}/sleep`, { method: 'POST' }),
  ensureReady: (name) => request(`/api/workers/${encodeURIComponent(name)}/ensure-ready`, { method: 'POST' }),

  // Message injection (v1.5). Posts a system-level message into the
  // manager's admin DM room, or the team's leader room. 409 means the room
  // isn't provisioned yet -- callers should surface that distinctly.
  messageManager: (name, body) =>
    request(`/api/managers/${encodeURIComponent(name)}/message`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ body }),
    }),
  messageTeam: (name, body) =>
    request(`/api/teams/${encodeURIComponent(name)}/message`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ body }),
    }),
};
