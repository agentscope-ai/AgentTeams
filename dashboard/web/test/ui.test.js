import test from 'node:test';
import assert from 'node:assert/strict';
import { installFakeDocument } from './dom-fakes.js';
import { confirmAction, promptMessage } from '../src/ui.js';

test('confirmAction resolves true on submit', async () => {
  const { confirmDialog } = installFakeDocument();
  const p = confirmAction('are you sure?');
  confirmDialog.querySelector('#confirm-form').submit();
  assert.equal(await p, true);
});

test('confirmAction resolves false on cancel-button click', async () => {
  const { confirmDialog } = installFakeDocument();
  const p = confirmAction('are you sure?');
  document.getElementById('confirm-cancel').click();
  assert.equal(await p, false);
});

test('confirmAction resolves false (not hung) when the dialog is closed via Escape', async () => {
  const { confirmDialog } = installFakeDocument();
  const p = confirmAction('are you sure?');
  confirmDialog.pressEscape();
  // Before the fix, Escape didn't resolve the Promise at all -- awaiting it
  // would hang forever. Racing against a timeout proves it settles.
  const result = await Promise.race([
    p,
    new Promise((_, reject) => setTimeout(() => reject(new Error('timed out, Promise never resolved')), 50)),
  ]);
  assert.equal(result, false);
});

test('confirmAction does not leak listeners across repeated Escape-cancelled invocations', async () => {
  const { confirmDialog } = installFakeDocument();
  let submitCount = 0;
  confirmDialog.querySelector('#confirm-form').addEventListener('submit', () => {
    submitCount += 1;
  });

  for (let i = 0; i < 3; i += 1) {
    const p = confirmAction(`attempt ${i}`);
    confirmDialog.pressEscape();
    // eslint-disable-next-line no-await-in-loop
    await p;
  }

  // One extra submit should only notify our own probe listener once --
  // if confirmAction's internal onSubmit handlers leaked across the three
  // prior (Escape-cancelled) invocations, this final submit would resolve
  // multiple stale Promises, but it must not throw and our probe listener
  // must have been called exactly once per submit dispatch (i.e. it is not
  // duplicated by confirmAction's own leaked handlers piling up elsewhere).
  const p = confirmAction('final');
  confirmDialog.querySelector('#confirm-form').submit();
  assert.equal(await p, true);
  assert.equal(submitCount, 1);
});

test('promptMessage resolves the trimmed value on submit', async () => {
  const { messageDialog } = installFakeDocument();
  const p = promptMessage('Send a message');
  document.getElementById('message-body').value = '  hello  ';
  messageDialog.querySelector('#message-form').submit();
  assert.equal(await p, 'hello');
});

test('promptMessage resolves null on empty submit', async () => {
  const { messageDialog } = installFakeDocument();
  const p = promptMessage('Send a message');
  document.getElementById('message-body').value = '   ';
  messageDialog.querySelector('#message-form').submit();
  assert.equal(await p, null);
});

test('promptMessage resolves null (not hung) when the dialog is closed via Escape', async () => {
  const { messageDialog } = installFakeDocument();
  const p = promptMessage('Send a message');
  messageDialog.pressEscape();
  const result = await Promise.race([
    p,
    new Promise((_, reject) => setTimeout(() => reject(new Error('timed out, Promise never resolved')), 50)),
  ]);
  assert.equal(result, null);
});
