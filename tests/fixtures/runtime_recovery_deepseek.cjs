// Read native DSH rc.2 session files without starting a turn.
const {createHash} = require('node:crypto')
const fs = require('node:fs')
const path = require('node:path')
const {zstdDecompressSync} = require('node:zlib')

const [home, task, generation] = process.argv.slice(2)
const sha = value => createHash('sha256').update(value).digest('hex')
const input = 'matrix-' + sha('recovery:recovery-team/' + task + ':' + generation)
const sessions = path.join(home, '.dsh', 'sessions')
const matches = []
for (const file of fs.existsSync(sessions) ? fs.readdirSync(sessions, {recursive: true}) : []) {
  if (!/^.+\/session-task-[a-f0-9]{32}\/session\.jsonl(?:\.zstd)?$/.test(file)) continue
  const bytes = fs.readFileSync(path.join(sessions, file))
  const raw = file.endsWith('.zstd') ? zstdDecompressSync(bytes) : bytes
  const rows = raw.toString().trimEnd().split('\n').map(JSON.parse)
  const [header] = rows
  if (header.type !== 'session' || header.id !== file.split('/').at(-2)) continue
  const index = rows.findIndex(row => row.type === 'user/message' && row.data.id === input)
  if (index < 0) continue
  // Native DSH can open its turn before the incoming message is spliced in.
  const turn = rows.slice(0, index).findLast(row => row.type === 'turn/start')?.data.turn
  const events = rows.slice(index).filter(row => row.data?.turn === turn)
  matches.push({
    session_id: header.id, native_digest: sha(bytes), turn, input_id: input,
    task_mentioned: JSON.stringify(rows[index].data.content).includes(task),
    tool_calls: events.filter(row => row.type === 'tool/call').length,
    tool_results: events.filter(row => row.type === 'tool/result').length,
    completed: events.some(row => row.type === 'turn/end' && row.data.reason?.kind === 'completed'),
  })
}
if (matches.length > 1) throw new Error('Recovery input occurs in multiple native DSH sessions')
console.log(JSON.stringify(matches[0] ?? null))
