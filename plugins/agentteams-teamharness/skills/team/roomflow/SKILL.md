---
name: teamharness-roomflow
description: "Use for TeamHarness room classification and Matrix task-room setup: roomflow describe_room, roomflow create_task_room, source/requester room vs task room detection, project-scoped task-room reuse, and invite handling. Do not use for projectflow, taskflow, or message delivery."
---

# Roomflow

Use this skill when a Leader needs to identify the current Matrix room or
prepare the Matrix task room before Quick Task or Project Work state exists.

## Describe Room

Call `roomflow describe_room` when a Matrix room's purpose is unclear.

Classify from Matrix state:

- Source channel / requester room: external channel, or Matrix room whose name
  indicates DM or Team room.
- Task room: Matrix room whose name starts with `TASK：<projectId>`, or
  whose topic/tags carry a Project/Task marker.

Do not infer room purpose from joined-room IDs alone.

## Create Or Reuse Task Room

From a Source channel / requester room, call `roomflow create_task_room` before
creating project or task state.

Pass a stable `projectId`. The tool normalizes the Matrix room name to
`TASK：<projectId>` and reuses task rooms only by that project id.

Pass the current source metadata exactly as received, such as DingTalk
`sourceRoomId` and `sender`, so project state can keep the requester route.
Do not use source room or sender identity to decide task-room reuse. Different
projects from the same DingTalk group or same person still get different task
rooms.

Pass the complete Matrix user IDs for Workers who must receive or observe work
in the task room. Use the returned Matrix room as the task room for the
handoff, project state, task delegation, and Worker completion reports.
