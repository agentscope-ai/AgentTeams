# TeamHarness Session Wakeups And Self-Triggers

This document is a reference for plugin integrators: which events can start a
session turn, how a session may deliberately wake itself or a sibling session,
and why plain self-messages cannot.

## What wakes a session

A turn starts when a room event from another participant passes the channel's
inbound checks (allow policy, room rules, mention gating). By design, a
message sent by the session's own Matrix user id does **not** wake it: the
channel evaluates a self-skip early and returns before enqueueing a turn. An
agent therefore cannot wake itself by posting into its own room.

## The supported self-cross-session trigger

One sanctioned exception exists: a structured TeamHarness trigger carried in
the message content under the key `m.teamharness.trigger`, with:

- `kind` = `self_cross_session`
- `type` = `PROJECT_REQUESTED`
- `targetRoomId` / `targetSession` — the room or session to wake (the trigger
  only applies when the delivering room matches the target)

The bypass is evaluated **before** the self-skip, so a matching trigger wakes
the target session even though the sender is the same user id.

Sending side: the TeamHarness `message` tool accepts `type:
"PROJECT_REQUESTED"` with a structured `replyRoute` (`channel` plus
`targetSession`). Validation requires:

- `replyRoute.channel` and `replyRoute.targetSession` present (a message
  without a structured reply route is rejected);
- the sender's `agent` / `agentId` equal to the runtime's **current Matrix
  user id** (role or workspace names are rejected — the trigger must be a
  same-agent self-trigger).

Use this mechanism when a session must wake a specific room/session
deliberately — for example a leader scheduling a cross-session follow-up.

## Do not rely on HEARTBEAT for room wakeups

The runtime heartbeat facility is not a room wakeup channel. One rule holds
for every runtime: a heartbeat that surfaces into a room is a plain
self-message from the agent's own Matrix user id, so it never passes the
self-skip above unless it carries the structured trigger.

The specifics below are **QwenPaw runtime** settings (verified against
QwenPaw v2.2.x; file paths refer to the QwenPaw repository as of this
writing), not a shared TeamHarness contract — other runtimes may differ:

- **Disabled by default** — `HeartbeatConfig.enabled` defaults to `False`
  (`src/qwenpaw/config/config.py`); nothing runs until the operator turns
  it on.
- **Default period is hours** — `heartbeat.every` defaults to `6h`
  (`HEARTBEAT_DEFAULT_EVERY`, `src/qwenpaw/constant.py`).
- **A missed tick is silently dropped** — the heartbeat job runs with a
  60-second misfire grace (`HEARTBEAT_MISFIRE_GRACE_SECONDS`,
  `src/qwenpaw/app/crons/manager.py`); a tick that comes in late beyond
  the grace is dropped without catch-up. This is a shorter window than the
  600-second grace used by regular scheduled jobs.
- **Ticks run in the main session by default** — `heartbeat.target`
  defaults to `main` (`HEARTBEAT_DEFAULT_TARGET`,
  `src/qwenpaw/constant.py`); a tick executes as a request against that
  session rather than as a room event. The target is configurable
  (`main` / `last` / `inbox`).

For time-based activation use a scheduled task; for cross-session activation
use the `PROJECT_REQUESTED` trigger; for same-session continuation simply
reply in the session.

## Quick reference

| Goal | Mechanism |
| --- | --- |
| wake a room/session from an external timer | scheduled task (e.g., cron `run_at`) |
| wake a specific room/session from agent code | `message` with `type: PROJECT_REQUESTED` and structured `replyRoute` |
| plain self-message (no trigger) | never wakes — self-skip by design |
