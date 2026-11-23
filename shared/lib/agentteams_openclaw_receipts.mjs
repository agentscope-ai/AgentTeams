// Read committed native entries, including OpenClaw's compressed SQLite rows.
import { DatabaseSync } from 'node:sqlite';
import { zstdDecompressSync } from 'node:zlib';

const database = new DatabaseSync(process.argv[2], { readOnly: true });
const ids = new Set();
try {
  for (const row of database.prepare('SELECT event_json,event_zstd FROM transcript_events').iterate()) {
    const entry = JSON.parse(row.event_json ?? zstdDecompressSync(row.event_zstd).toString());
    const message = entry.message;
    const transport = message?.__openclaw?.transport;
    if (message?.role === 'user' && !message.excludeFromContext &&
        ['matrix', 'agentteams-matrix'].includes(transport?.channel) &&
        typeof transport.messageId === 'string' && transport.messageId.startsWith('$')) {
      ids.add(transport.messageId);
    }
  }
  process.stdout.write(JSON.stringify([...ids]));
} finally {
  database.close();
}
