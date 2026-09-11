import { writeFile } from 'node:fs/promises'
import { resolve } from 'node:path'

export const name = 'teamharness-live-probe'
export const inject = ['tools']

const delay = milliseconds => new Promise(done => setTimeout(done, milliseconds))

function jsonText(result, toolName) {
  const block = result.content.find(candidate => candidate.type === 'text')
  if (block === undefined) throw new Error(`${toolName} returned no text block`)
  return JSON.parse(block.text)
}

async function waitForTools(ctx) {
  const deadline = Date.now() + 15_000
  while (Date.now() < deadline) {
    const names = ctx.tools.schemas().map(tool => tool.name)
    if (names.includes('mcp__teamharness__message') && names.includes('mcp__teamharness__filesync')) return
    await delay(100)
  }
  throw new Error('TeamHarness live tools did not register before the deadline')
}

async function callTool(ctx, name, args) {
  return jsonText(await ctx.tools.execute({
    signal: new AbortController().signal,
    callId: `teamharness-live-${name}-${Date.now()}`,
    name: `mcp__teamharness__${name}`,
    arguments: args,
  }), name)
}

export async function apply(ctx, config) {
  const reportPath = resolve(String(config.reportPath ?? ''))
  const workspace = resolve(String(config.workspace ?? ''))
  const marker = String(config.marker ?? '')
  const roomId = String(config.roomId ?? '')
  const artifactPath = `shared/deepseek-harness/live/${marker}.txt`
  let exitCode = 1
  try {
    await waitForTools(ctx)
    const message = await callTool(ctx, 'message', {
      action: 'send',
      channel: 'matrix',
      target: `room:${roomId}`,
      text: marker,
      agentId: 'ui-lead-01',
    })
    const push = await callTool(ctx, 'filesync', {
      action: 'push',
      path: artifactPath,
      workspaceDir: workspace,
    })
    const stat = await callTool(ctx, 'filesync', {
      action: 'stat',
      path: artifactPath,
      workspaceDir: workspace,
    })
    const checks = {
      matrixSendPassed: message.ok === true && typeof message.messageId === 'string' && message.messageId !== '',
      storagePushPassed: push.ok === true,
      storageStatPassed: stat.ok === true && stat.exists === true,
    }
    const failed = Object.entries(checks).filter(([, passed]) => !passed).map(([name]) => name)
    await writeFile(reportPath, `${JSON.stringify({ checks, failed, marker, roomId, artifactPath, message, push, stat }, null, 2)}\n`, 'utf8')
    exitCode = failed.length === 0 ? 0 : 1
  } catch (error) {
    await writeFile(reportPath, `${JSON.stringify({ fatal: error instanceof Error ? error.stack : String(error) }, null, 2)}\n`, 'utf8')
  } finally {
    setTimeout(() => process.exit(exitCode), 25)
  }
}
