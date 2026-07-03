// Tiny DOM helpers shared across panels: toast notifications and a native
// <dialog>-backed confirm prompt for the wake/sleep/ensure-ready actions.

export function escapeHtml(str) {
  return String(str).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  })[c]);
}

export function showToast(message, { error = false } = {}) {
  const toast = document.getElementById('toast');
  toast.textContent = message;
  toast.hidden = false;
  toast.classList.toggle('error', error);
  clearTimeout(showToast._t);
  showToast._t = setTimeout(() => {
    toast.hidden = true;
  }, 4000);
}

export function confirmAction(message) {
  const dialog = document.getElementById('confirm-dialog');
  const msgEl = document.getElementById('confirm-message');
  const cancelBtn = document.getElementById('confirm-cancel');
  msgEl.textContent = message;

  return new Promise((resolve) => {
    function onCancel() {
      cleanup();
      dialog.close();
      resolve(false);
    }
    function onSubmit(ev) {
      ev.preventDefault();
      cleanup();
      dialog.close();
      resolve(true);
    }
    function cleanup() {
      cancelBtn.removeEventListener('click', onCancel);
      dialog.querySelector('#confirm-form').removeEventListener('submit', onSubmit);
    }
    cancelBtn.addEventListener('click', onCancel);
    dialog.querySelector('#confirm-form').addEventListener('submit', onSubmit);
    dialog.showModal();
  });
}

/**
 * promptMessage opens a textarea dialog and resolves with the entered text,
 * or null if the operator cancelled or submitted empty text.
 */
export function promptMessage(title) {
  const dialog = document.getElementById('message-dialog');
  const titleEl = document.getElementById('message-dialog-title');
  const textarea = document.getElementById('message-body');
  const cancelBtn = document.getElementById('message-cancel');
  titleEl.textContent = title;
  textarea.value = '';

  return new Promise((resolve) => {
    function onCancel() {
      cleanup();
      dialog.close();
      resolve(null);
    }
    function onSubmit(ev) {
      ev.preventDefault();
      cleanup();
      dialog.close();
      const value = textarea.value.trim();
      resolve(value.length > 0 ? value : null);
    }
    function cleanup() {
      cancelBtn.removeEventListener('click', onCancel);
      dialog.querySelector('#message-form').removeEventListener('submit', onSubmit);
    }
    cancelBtn.addEventListener('click', onCancel);
    dialog.querySelector('#message-form').addEventListener('submit', onSubmit);
    dialog.showModal();
    textarea.focus();
  });
}

export function badgeClass(phaseOrState) {
  const v = String(phaseOrState || '').toLowerCase();
  if (['ready', 'running', 'completed', 'active'].includes(v)) return 'badge-ready';
  if (['degraded', 'sleeping', 'blocked'].includes(v)) return 'badge-degraded';
  if (['failed', 'stopped'].includes(v)) return 'badge-failed';
  if (['pending', 'provisioning', 'assigned', 'planning'].includes(v)) return 'badge-pending';
  return 'badge-unknown';
}
