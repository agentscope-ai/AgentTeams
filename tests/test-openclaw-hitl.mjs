import { strict as assert } from 'node:assert';
import { readFile, writeFile, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const patch = await readFile(new URL('../openclaw-base/patches/unified-hitl.patch', import.meta.url), 'utf8');
const source = patch.split('+++ b/extensions/matrix/src/dashboard-hitl.ts\n')[1].split('--- a/')[0]
  .split('\n').filter(line => line.startsWith('+')).map(line => line.slice(1)).join('\n');
const temporary = await mkdtemp(join(tmpdir(), 'openclaw-hitl-'));
try {
  const path = join(temporary, 'dashboard-hitl.ts');
  await writeFile(path, source);
  const { dashboardApprovalCommand: command } = await import(pathToFileURL(path));
  const answer = { kind: 'tool_approval', agent_id: '@worker:s', confirmation_id: 'one', context_id: 'one', answer: 'approved' };
  assert.equal(command(answer, '@worker:s'), '/approve one allow-once');
  assert.equal(command({ ...answer, answer: 'rejected' }, '@worker:s'), '/approve one deny');
  for (const changes of [{ agent_id: '@other:s' }, { context_id: 'stale' }, { confirmation_id: 'one\n/approve two' }, { answer: 'allow-always' }]) {
    assert.equal(command({ ...answer, ...changes }, '@worker:s'), '');
  }
  assert.equal(command(undefined, '@worker:s'), undefined);
} finally {
  await rm(temporary, { recursive: true });
}
