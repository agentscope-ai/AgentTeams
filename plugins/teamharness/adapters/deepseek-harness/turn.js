export function summarize(events, firstSeq) {
  let started = false
  let text = ''
  let reason
  for (const event of events) {
    if (event.seq < firstSeq) continue
    if (event.type === 'turn/start') {
      started = true
      continue
    }
    if (!started) continue
    if (event.type === 'assistant/message') {
      const joined = event.data.message.content
        .filter(block => block.type === 'text')
        .map(block => block.text)
        .join('')
      if (joined !== '') text = joined
    }
    if (event.type === 'turn/end') {
      reason = event.data.reason
      break
    }
  }
  return { text, reason }
}

export function existingOutcome(events, id) {
  const prompt = events.find(event => event.type === 'user/message' && event.data.id === id)
  if (prompt === undefined) return undefined
  return summarize(events, prompt.seq)
}

export async function executeAttempt({ getEvents, id, firstSeq, eventId, followup, whenIdle, flush }) {
  let outcome = existingOutcome(getEvents(), id)
  if (outcome === undefined) {
    followup()
    await whenIdle()
    await flush()
    outcome = summarize(getEvents(), firstSeq)
  } else if (outcome.reason === undefined) {
    throw new Error(`Matrix event ${eventId} exists in the DSH session without a completed turn`)
  }
  return outcome
}
