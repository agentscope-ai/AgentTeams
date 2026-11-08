import { createHash } from 'node:crypto'


export function matrixEventMessageId(eventId, attempt = 1) {
  const key = attempt > 1 ? `${eventId}:attempt:${attempt}` : eventId
  return `matrix-${createHash('sha256').update(key).digest('hex')}`
}
