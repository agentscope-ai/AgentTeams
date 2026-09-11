import { writeFile } from 'node:fs/promises'
import { resolve } from 'node:path'

export const name = 'teamharness-smoke-probe'
export const inject = ['systemPrompt', 'skills', 'tools', 'agentDefaultModel']

const delay = milliseconds => new Promise(done => setTimeout(done, milliseconds))

function jsonText(result, toolName) {
  const block = result.content.find(candidate => candidate.type === 'text')
  if (block === undefined) throw new Error(`${toolName} returned no text block`)
  return JSON.parse(block.text)
}

async function observedState(ctx, workspace) {
  const prompt = (await ctx.systemPrompt.assemble()).sections.map(section => section.text).join('\n')
  const skillNames = (await ctx.skills.list({ cwd: workspace })).map(skill => skill.name).sort()
  const toolNames = ctx.tools.schemas().map(tool => tool.name).sort()
  return { prompt, skillNames, toolNames }
}

async function waitUntilReady(ctx, workspace) {
  const deadline = Date.now() + 15_000
  let state
  while (Date.now() < deadline) {
    state = await observedState(ctx, workspace)
    if (
      state.prompt.includes('member.runtimeName: dsh-worker-a') &&
      state.skillNames.includes('teamharness-task-execution') &&
      state.toolNames.includes('mcp__teamharness__health') &&
      state.toolNames.includes('mcp__teamharness__filesync')
    ) return state
    await delay(100)
  }
  return state ?? await observedState(ctx, workspace)
}

export async function apply(ctx, config) {
  const reportPath = resolve(String(config.reportPath ?? ''))
  const workspace = resolve(String(config.workspace ?? ''))
  const expectedRuntimeRole = String(config.expectedRuntimeRole ?? 'worker')
  const expectMessageTool = config.expectMessageTool === true
  const expectTeamContract = config.expectTeamContract !== false
  const expectedModel = String(config.expectedModel ?? 'deepseek-v4-flash')
  let exitCode = 1
  try {
    const { prompt, skillNames, toolNames } = await waitUntilReady(ctx, workspace)
    const healthResult = await ctx.tools.execute({
      signal: new AbortController().signal,
      callId: 'teamharness-dsh-health',
      name: 'mcp__teamharness__health',
      arguments: {},
    })
    const health = jsonText(healthResult, 'health')

    const filesyncResult = await ctx.tools.execute({
      signal: new AbortController().signal,
      callId: 'teamharness-dsh-filesync',
      name: 'mcp__teamharness__filesync',
      arguments: {
        action: 'push',
        path: 'shared/deepseek-harness/artifacts/smoke.txt',
        workspaceDir: workspace,
        dryRun: true,
      },
    })
    const filesync = jsonText(filesyncResult, 'filesync')

    const selectedModel = ctx.get('agentDefaultModel').currentSelection()
    const checks = {
      promptHasTeamContract: prompt.includes('# Team Contract') === expectTeamContract,
      promptHasWorkerRole: prompt.includes('# Worker Role'),
      promptHasRuntimeIdentity: prompt.includes('member.runtimeName: dsh-worker-a'),
      promptHasRuntimeRole: prompt.includes(`member.role: ${expectedRuntimeRole}`),
      skillDiscovered: skillNames.includes('teamharness-task-execution'),
      leaderSkillsHidden: !skillNames.some(name => [
        'teamharness-roomflow',
        'teamharness-team-coordination',
        'teamharness-project-management',
        'teamharness-task-delegation',
      ].includes(name)),
      unregisteredSkillHidden: !skillNames.includes('teamharness-organization'),
      healthToolRegistered: toolNames.includes('mcp__teamharness__health'),
      filesyncToolRegistered: toolNames.includes('mcp__teamharness__filesync'),
      messageToolVisibilityMatchesRole: toolNames.includes('mcp__teamharness__message') === expectMessageTool,
      defaultModelMatchesRuntime: selectedModel.provider === 'deepseek-official' && selectedModel.model === expectedModel,
      healthCallPassed: health.ok === true && health.status === 'ok',
      filesyncDryRunPassed: filesync.ok === true && filesync.dryRun === true,
    }
    const failed = Object.entries(checks).filter(([, passed]) => !passed).map(([check]) => check)
    await writeFile(reportPath, `${JSON.stringify({ checks, failed, skillNames, toolNames, selectedModel, health, filesync }, null, 2)}\n`, 'utf8')
    exitCode = failed.length === 0 ? 0 : 1
  } catch (error) {
    await writeFile(reportPath, `${JSON.stringify({ fatal: error instanceof Error ? error.stack : String(error) }, null, 2)}\n`, 'utf8')
  } finally {
    setTimeout(() => process.exit(exitCode), 25)
  }
}
