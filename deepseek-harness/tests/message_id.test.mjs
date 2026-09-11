import assert from 'node:assert/strict'
import test from 'node:test'

import { matrixEventMessageId } from '../../plugins/teamharness/adapters/deepseek-harness/message-id.js'
import { executeAttempt } from '../../plugins/teamharness/adapters/deepseek-harness/turn.js'


test('a retry gets a new stable DSH message id', () => {
  const firstAttempt = matrixEventMessageId('$matrix-event', 1)
  const secondAttempt = matrixEventMessageId('$matrix-event', 2)

  assert.equal(matrixEventMessageId('$matrix-event', 1), firstAttempt)
  assert.equal(matrixEventMessageId('$matrix-event', 2), secondAttempt)
  assert.notEqual(secondAttempt, firstAttempt)
})

test('a failed turn is followed by a real retry and the completed retry is reusable', async () => {
  const firstId = matrixEventMessageId('$matrix-event', 1)
  const retryId = matrixEventMessageId('$matrix-event', 2)
  let events = [
    { seq: 0, type: 'user/message', data: { id: firstId } },
    { seq: 1, type: 'turn/start', data: {} },
    { seq: 2, type: 'turn/end', data: { reason: { kind: 'error', error: { code: 'HTTP_503' } } } },
  ]
  let followups = 0
  let flushes = 0
  const options = {
    getEvents: () => events,
    id: retryId,
    firstSeq: 3,
    eventId: '$matrix-event',
    followup: () => {
      followups += 1
      events.push({ seq: 3, type: 'user/message', data: { id: retryId } })
    },
    whenIdle: async () => {
      // DSH exposes session events as a fresh snapshot after the turn.
      events = events.concat(
        { seq: 4, type: 'turn/start', data: {} },
        { seq: 5, type: 'assistant/message', data: { message: { content: [{ type: 'text', text: 'recovered' }] } } },
        { seq: 6, type: 'turn/end', data: { reason: { kind: 'completed' } } },
      )
    },
    flush: async () => { flushes += 1 },
  }

  const recovered = await executeAttempt(options)
  assert.equal(recovered.text, 'recovered')
  assert.equal(recovered.reason.kind, 'completed')
  assert.equal(followups, 1)
  assert.equal(flushes, 1)

  await executeAttempt({
    ...options,
    followup: () => { throw new Error('completed retry must not run twice') },
    whenIdle: async () => { throw new Error('completed retry must not wait again') },
    flush: async () => { throw new Error('completed retry must not flush again') },
  })
})
