// Minimal DOM fakes shared by the ui.js / poll.js tests -- there's no jsdom
// in this project's devDependencies, so these hand-roll just enough of the
// <dialog>/EventTarget/document surface that ui.js and poll.js touch.
// Built on Node's built-in EventTarget/CustomEvent (both are globals).

export class FakeElement extends EventTarget {
  constructor(tag) {
    super();
    this.tagName = tag;
    this.children = new Map();
    this.textContent = '';
    this.value = '';
    this.hidden = false;
    this.classList = {
      _set: new Set(),
      toggle(name, on) {
        if (on) this._set.add(name);
        else this._set.delete(name);
      },
      contains(name) {
        return this._set.has(name);
      },
    };
  }

  querySelector(sel) {
    const id = sel.replace(/^#/, '');
    return this.children.get(id) || null;
  }

  appendChild(child) {
    this.children.set(child.id, child);
    return child;
  }

  // Only used by task-detail.js, which assigns a fixed template string
  // (`dialog.innerHTML = \`...\`` / `body.innerHTML = \`...\``) and then reads
  // it back solely via querySelector('#some-id') or by checking substrings
  // for rendered content -- so a real HTML parser isn't needed here. This
  // scans for every `id="..."` in the string and registers a fresh
  // FakeElement per id (any elements from a previous assignment are
  // discarded, matching how a real innerHTML re-render replaces its
  // subtree), while still keeping the raw string for substring assertions.
  set innerHTML(html) {
    this._html = html;
    this.children = new Map();
    const re = /id="([^"]+)"/g;
    let m;
    // eslint-disable-next-line no-cond-assign
    while ((m = re.exec(html))) {
      const el = new FakeElement('div');
      el.id = m[1];
      if (m[1] === 'task-detail-body') el.querySelectorAll = () => [];
      this.children.set(m[1], el);
    }
  }

  get innerHTML() {
    return this._html || '';
  }

  click() {
    this.dispatchEvent(new CustomEvent('click'));
  }

  submit() {
    // Mirrors a real <form> submit event: cancelable, so preventDefault()
    // matters the way ui.js expects.
    const ev = new CustomEvent('submit', { cancelable: true });
    this.dispatchEvent(ev);
    return ev;
  }
}

/**
 * FakeDialog models the bits of the native <dialog> element ui.js relies on:
 * showModal()/close() and the 'close'/'cancel' events. Calling close() when
 * already closed is a no-op (matches real <dialog> semantics) and dispatchCancel()
 * simulates what the browser fires internally when the user presses Escape
 * (a 'cancel' event immediately followed by a 'close' event, per the HTML spec).
 */
export class FakeDialog extends FakeElement {
  constructor() {
    super('dialog');
    this.open = false;
  }

  showModal() {
    this.open = true;
  }

  close() {
    if (!this.open) return;
    this.open = false;
    this.dispatchEvent(new CustomEvent('close'));
  }

  pressEscape() {
    if (!this.open) return;
    const cancelEv = new CustomEvent('cancel', { cancelable: true });
    this.dispatchEvent(cancelEv);
    this.open = false;
    this.dispatchEvent(new CustomEvent('close'));
  }
}

/**
 * Builds the fake confirm-dialog / message-dialog DOM trees that ui.js's
 * confirmAction()/promptMessage() look up via getElementById, and installs
 * a global `document` stand-in with just enough surface (getElementById,
 * hidden, add/removeEventListener) for both ui.js and poll.js.
 */
export function installFakeDocument() {
  const registry = new Map();

  function makeConfirmDialog() {
    const dialog = new FakeDialog();
    dialog.id = 'confirm-dialog';
    const form = new FakeElement('form');
    form.id = 'confirm-form';
    const msg = new FakeElement('span');
    msg.id = 'confirm-message';
    const cancelBtn = new FakeElement('button');
    cancelBtn.id = 'confirm-cancel';
    dialog.children.set('confirm-form', form);
    registry.set('confirm-dialog', dialog);
    registry.set('confirm-message', msg);
    registry.set('confirm-cancel', cancelBtn);
    return dialog;
  }

  function makeMessageDialog() {
    const dialog = new FakeDialog();
    dialog.id = 'message-dialog';
    const form = new FakeElement('form');
    form.id = 'message-form';
    const titleEl = new FakeElement('span');
    titleEl.id = 'message-dialog-title';
    const textarea = new FakeElement('textarea');
    textarea.id = 'message-body';
    textarea.focus = () => {};
    const cancelBtn = new FakeElement('button');
    cancelBtn.id = 'message-cancel';
    dialog.children.set('message-form', form);
    registry.set('message-dialog', dialog);
    registry.set('message-dialog-title', titleEl);
    registry.set('message-body', textarea);
    registry.set('message-cancel', cancelBtn);
    return dialog;
  }

  const visibilityTarget = new EventTarget();
  const fakeBody = {
    appendChild: (el) => {
      // Mirrors document.body.appendChild(dialog) followed by later
      // document.getElementById(dialog.id) lookups (task-detail.js's
      // singleton-dialog pattern).
      if (el.id) registry.set(el.id, el);
      return el;
    },
  };
  const fakeDocument = {
    hidden: false,
    body: fakeBody,
    getElementById: (id) => registry.get(id) || null,
    createElement: (tag) => (tag === 'dialog' ? new FakeDialog() : new FakeElement(tag)),
    addEventListener: (type, listener) => visibilityTarget.addEventListener(type, listener),
    removeEventListener: (type, listener) => visibilityTarget.removeEventListener(type, listener),
    dispatchEvent: (ev) => visibilityTarget.dispatchEvent(ev),
  };

  globalThis.document = fakeDocument;

  return {
    confirmDialog: makeConfirmDialog(),
    messageDialog: makeMessageDialog(),
    setHidden(v) {
      fakeDocument.hidden = v;
      fakeDocument.dispatchEvent(new CustomEvent('visibilitychange'));
    },
  };
}
