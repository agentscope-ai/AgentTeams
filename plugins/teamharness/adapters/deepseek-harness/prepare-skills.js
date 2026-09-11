import { cp, mkdir, readFile, rm, stat, writeFile } from 'node:fs/promises'
import { basename, isAbsolute, join, relative, resolve } from 'node:path'
import { parse } from 'yaml'

function argumentMap(argv) {
  const values = new Map()
  for (let index = 0; index < argv.length; index += 2) values.set(argv[index], argv[index + 1])
  return values
}

function requiredArgument(values, name) {
  const value = String(values.get(name) ?? '').trim()
  if (value === '') throw new Error(`missing ${name}`)
  return resolve(value)
}

function adapterRole(value) {
  const role = String(value ?? '').toLowerCase().replaceAll('_', '-').trim()
  if (['team-leader', 'teamleader', 'leader'].includes(role)) return 'leader'
  if (['worker', 'standalone'].includes(role)) return 'worker'
  if (['remote', 'remote-member'].includes(role)) return 'remote-member'
  if (role === 'manager') return 'manager'
  throw new Error(`unsupported TeamHarness role: ${value}`)
}

function skillEntries(manifest) {
  return [...(manifest?.skills?.agent ?? []), ...(manifest?.skills?.team ?? [])]
}

function sourceUnderPlugin(pluginDir, configuredPath) {
  if (isAbsolute(configuredPath)) throw new Error(`skill path must be relative: ${configuredPath}`)
  const source = resolve(pluginDir, configuredPath)
  const remainder = relative(pluginDir, source)
  if (remainder.startsWith('..') || isAbsolute(remainder)) throw new Error(`skill path escapes plugin directory: ${configuredPath}`)
  return source
}

const values = argumentMap(process.argv.slice(2))
const pluginDir = requiredArgument(values, '--plugin-dir')
const runtimeConfigPath = requiredArgument(values, '--runtime-config')
const outputDir = requiredArgument(values, '--output')
const marker = join(outputDir, '.teamharness-dsh-skill-root')

const manifest = parse(await readFile(join(pluginDir, 'plugin.yaml'), 'utf8'))
const runtimeConfig = parse(await readFile(runtimeConfigPath, 'utf8'))
const role = adapterRole(runtimeConfig?.member?.role)
const selected = skillEntries(manifest).filter(skill => Array.isArray(skill.roles) && skill.roles.includes(role))
if (selected.length === 0) throw new Error(`TeamHarness has no skills for role ${role}`)

let outputExists = false
try {
  outputExists = (await stat(outputDir)).isDirectory()
} catch (error) {
  if (error?.code !== 'ENOENT') throw error
}
if (outputExists) {
  try {
    await stat(marker)
  } catch {
    throw new Error(`refusing to replace unmarked skill directory: ${outputDir}`)
  }
  await rm(outputDir, { recursive: true, force: true })
}
await mkdir(outputDir, { recursive: true })
await writeFile(marker, 'Generated TeamHarness role skill root for DeepSeek Harness\n', 'utf8')

for (const skill of selected) {
  const source = sourceUnderPlugin(pluginDir, String(skill.path ?? ''))
  await cp(source, join(outputDir, basename(source)), { recursive: true })
}

process.stdout.write(`${JSON.stringify({ role, skills: selected.map(skill => skill.id) })}\n`)
