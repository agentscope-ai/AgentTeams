import { api } from '../api.js';
import { escapeHtml } from '../ui.js';

/**
 * renderFiles mounts a minimal file browser over /api/files/{shared,agents}/...
 * (list-or-download; the proxy's MinIO route returns a JSON directory
 * listing when the key isn't itself an object, per handler.js). No polling
 * here -- it's navigated on demand.
 */
export function renderFiles(root) {
  let currentPath = ['shared'];

  root.innerHTML = `
    <div class="section-title">Files</div>
    <div class="breadcrumbs" id="file-roots">
      <button data-root="shared">shared/</button>
      <button data-root="agents">agents/</button>
    </div>
    <div class="breadcrumbs" id="file-crumbs"></div>
    <div id="file-view"><div class="empty-state">Loading...</div></div>
  `;

  const view = root.querySelector('#file-view');
  const crumbs = root.querySelector('#file-crumbs');

  root.querySelectorAll('#file-roots button').forEach((btn) => {
    btn.addEventListener('click', () => {
      currentPath = [btn.dataset.root];
      load();
    });
  });

  async function load() {
    renderCrumbs(crumbs, currentPath, (path) => {
      currentPath = path;
      load();
    });
    view.innerHTML = '<div class="empty-state">Loading...</div>';
    try {
      const result = await api.fileBrowse(currentPath);
      if (result && (Array.isArray(result.directories) || Array.isArray(result.files))) {
        renderDirectory(view, result, currentPath, (path) => {
          currentPath = path;
          load();
        });
      } else {
        renderFileContent(view, currentPath, result, (path) => {
          currentPath = path;
          load();
        });
      }
    } catch (err) {
      if (err.status === 404) {
        view.innerHTML = '<div class="empty-state">Nothing here.</div>';
      } else {
        view.innerHTML = `<div class="error-state">Failed to load: ${escapeHtml(err.message)}</div>`;
      }
    }
  }

  load();

  // No interval polling for the file browser (it's navigated on demand);
  // return a no-op cleanup so the tab-switch contract stays uniform.
  return () => {};
}

function renderCrumbs(el, path, onNavigate) {
  el.innerHTML = path
    .map((seg, i) => {
      const isLast = i === path.length - 1;
      return isLast
        ? `<span>${escapeHtml(seg)}</span>`
        : `<button data-idx="${i}">${escapeHtml(seg)}</button><span>/</span>`;
    })
    .join('');
  el.querySelectorAll('button[data-idx]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const idx = Number(btn.dataset.idx);
      onNavigate(path.slice(0, idx + 1));
    });
  });
}

function renderDirectory(view, listing, currentPath, onNavigate) {
  const dirs = listing.directories || [];
  const files = listing.files || [];

  if (dirs.length === 0 && files.length === 0) {
    view.innerHTML = '<div class="empty-state">Empty directory.</div>';
    return;
  }

  view.innerHTML = `
    <div class="file-list">
      ${dirs
        .map(
          (d) => `<div class="file-row"><button data-dir="${escapeHtml(d)}">📁 ${escapeHtml(d)}/</button></div>`,
        )
        .join('')}
      ${files
        .map(
          (f) =>
            `<div class="file-row"><button data-file="${escapeHtml(f.key)}">📄 ${escapeHtml(f.key)}</button><span class="file-meta">${f.size} bytes</span></div>`,
        )
        .join('')}
    </div>
  `;

  view.querySelectorAll('button[data-dir]').forEach((btn) => {
    btn.addEventListener('click', () => onNavigate([...currentPath, btn.dataset.dir]));
  });
  view.querySelectorAll('button[data-file]').forEach((btn) => {
    btn.addEventListener('click', () => onNavigate([...currentPath, btn.dataset.file]));
  });
}

function renderFileContent(view, path, content, onNavigate) {
  const text = typeof content === 'string' ? content : JSON.stringify(content, null, 2);
  view.innerHTML = `
    <div class="breadcrumbs"><button id="file-back">&larr; back</button></div>
    <pre class="file-preview">${escapeHtml(text)}</pre>
  `;
  view.querySelector('#file-back').addEventListener('click', () => {
    onNavigate(path.slice(0, -1).length ? path.slice(0, -1) : ['shared']);
  });
}
