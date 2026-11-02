import { readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { parse } from 'yaml'

export const name = 'teamharness-context'
export const inject = ['systemPrompt']

function requiredString(value, field) {
  const text = String(value ?? '').trim()
  if (text === '') throw new Error(`TeamHarness runtime config requires ${field}`)
  return text
}

function rolePromptName(role) {
  const normalized = role.toLowerCase().replaceAll('_', '-').trim()
  if (['team-leader', 'teamleader', 'leader'].includes(normalized)) return 'leader.md'
  if (normalized === 'worker') return 'worker.md'
  if (['remote', 'remote-member'].includes(normalized)) return 'remote-member.md'
  throw new Error(`unsupported TeamHarness role for DeepSeek Harness: ${role}`)
}

function inlineAgentConfig(runtimeConfig) {
  const inline = runtimeConfig?.desired?.inlineConfig ?? {}
  return [
    inline.identity ? `# Agent Identity\n\n${String(inline.identity).trim()}` : '',
    inline.soul ? `# Agent Soul\n\n${String(inline.soul).trim()}` : '',
    inline.agents ? `# Agent Instructions\n\n${String(inline.agents).trim()}` : '',
  ].filter(Boolean).join('\n\n')
}

const fileExchangePrompt = [
  '# Matrix File Exchange',
  '',
  'Files received from Matrix are placed under the Workspace `inbox/` directory.',
  'When the user asks you to create or return a file, write only the intended result under the Workspace `outbox/` directory.',
  'The channel bridge sends files created or changed in `outbox/` during the current turn back to the same Matrix room.',
].join('\n')

export function apply(ctx, config) {
  const pluginDir = resolve(requiredString(config.pluginDir, 'pluginDir'))
  const runtimeConfigPath = resolve(requiredString(config.runtimeConfigPath, 'runtimeConfigPath'))
  const runtimeConfig = parse(readFileSync(runtimeConfigPath, 'utf8'))
  const team = runtimeConfig?.team ?? {}
  const member = runtimeConfig?.member ?? {}
  const role = requiredString(member.role, 'member.role')
  const normalizedRole = role.toLowerCase().replaceAll('_', '-').trim()
  const configuredAgent = inlineAgentConfig(runtimeConfig)

  if (normalizedRole === 'standalone') {
    const standalonePrompt = [
      '# Worker Role — Standalone',
      '',
      'You are an independent AgentTeams Worker coordinated by Manager and Admin.',
      'Answer direct questions in the current room. Use task-execution only when a concrete task is assigned.',
      'Do not invent a Team, Team Leader, team room, project, or delegation workflow.',
      '',
      '# Current Agent Context',
      '',
      `member.name: ${requiredString(member.name, 'member.name')}`,
      `member.runtimeName: ${requiredString(member.runtimeName, 'member.runtimeName')}`,
      `member.role: ${role}`,
      `member.matrixUserId: ${requiredString(member.matrixUserId, 'member.matrixUserId')}`,
      `member.personalRoomId: ${requiredString(member.personalRoomId, 'member.personalRoomId')}`,
      fileExchangePrompt,
      configuredAgent,
    ].filter(Boolean).join('\n')
    ctx.effect(() => ctx.systemPrompt.section({
      name: 'teamharness:collaboration',
      order: 20,
      text: standalonePrompt,
    }), 'teamharness-context.section()')
    return
  }

  const teamPrompt = readFileSync(join(pluginDir, 'prompts', 'team', 'TEAMS.md'), 'utf8').trim()
  const rolePrompt = readFileSync(
    join(pluginDir, 'prompts', 'agent', rolePromptName(role)),
    'utf8',
  ).trim()
  const currentContext = [
    '# Current Team Context',
    '',
    `team.name: ${requiredString(team.name, 'team.name')}`,
    `team.teamRoomId: ${requiredString(team.teamRoomId, 'team.teamRoomId')}`,
    `team.leaderRuntimeName: ${requiredString(team.leaderRuntimeName, 'team.leaderRuntimeName')}`,
    `member.name: ${requiredString(member.name, 'member.name')}`,
    `member.runtimeName: ${requiredString(member.runtimeName, 'member.runtimeName')}`,
    `member.role: ${role}`,
    `member.matrixUserId: ${requiredString(member.matrixUserId, 'member.matrixUserId')}`,
  ].join('\n')

  const text = [teamPrompt, rolePrompt, currentContext, fileExchangePrompt, configuredAgent].filter(Boolean).join('\n\n')
  ctx.effect(() => ctx.systemPrompt.section({
    name: 'teamharness:collaboration',
    order: 20,
    text,
  }), 'teamharness-context.section()')
}
