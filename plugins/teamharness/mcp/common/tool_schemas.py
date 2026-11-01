# TOOL_SCHEMAS registration table for the 7 MCP tools (W1-a2: real module)
from __future__ import annotations

from typing import Any

TOOL_SCHEMAS: dict[str, dict[str, Any]] = {
    "health": {
        "description": (
            "Check TeamHarness MCP server availability and basic tool wiring. "
            "This is not runtime worker health, QwenPaw process health, storage "
            "status, or controller readiness."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {},
            "additionalProperties": True,
        },
    },
    "message": {
        "description": (
            "Send a TeamHarness message only when the output must leave the "
            "current runtime conversation: Matrix cross-room sends, external "
            "cross-channel sends, or requester replyRoute/cross-session reports. "
            "For same-agent Project Work handoff from any requester/source "
            "session into a Matrix task room, use the PROJECT_REQUESTED "
            "self-trigger payload with sender.session and target.session; the "
            "tool sends a Matrix event with a TeamHarness trigger marker so "
            "the target task room receives it as the current event. "
            "Do not use this tool for normal replies in the current room/session; "
            "answer directly instead."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["send"],
                    "description": "Only send is supported.",
                },
                "channel": {
                    "type": "string",
                    "description": "Delivery target channel: matrix for Matrix room sends and PROJECT_REQUESTED task-room triggers, or an external channel such as dingtalk for external sends.",
                },
                "sender": {
                    "type": "object",
                    "description": (
                        "Source Matrix account and requester/source session for "
                        "same-agent self-trigger handoff. For PROJECT_REQUESTED, "
                        "sender.agent must be the current runtime Matrix user id "
                        "such as @leader:matrix.local, not a role name like "
                        "leader or a workspace name like default."
                    ),
                    "additionalProperties": True,
                    "properties": {
                        "agent": {
                            "type": "string",
                            "description": (
                                "Current runtime Matrix user id for the sender, "
                                "for example @leader:matrix.local. Must match "
                                "agentId for PROJECT_REQUESTED self-trigger."
                            ),
                        },
                        "session": {
                            "type": "object",
                            "additionalProperties": True,
                            "properties": {
                                "channel": {"type": "string"},
                                "id": {"type": "string"},
                            },
                        },
                    },
                },
                "target": {
                    "type": "string",
                    "description": (
                        "Matrix room target for cross-room sends, for example "
                        "room:!room:domain. For PROJECT_REQUESTED self-trigger, "
                        "pass the target task room as a room:!room:domain string."
                    ),
                },
                "replyRoute": {
                    "type": "object",
                    "description": (
                        "Structured requester route for cross-session reports. "
                        "Required for PROJECT_REQUESTED; do not put the route "
                        "only in message text."
                    ),
                    "additionalProperties": True,
                    "properties": {
                        "channel": {"type": "string"},
                        "target": {"type": "string"},
                        "targetUser": {"type": "string"},
                        "targetSession": {"type": "string"},
                        "mentionSender": {"type": "boolean"},
                    },
                },
                "targetUser": {
                    "type": "string",
                    "description": "External-channel recipient user id; required for non-Matrix sends.",
                },
                "targetSession": {
                    "type": "string",
                    "description": "External-channel session id; required for non-Matrix sends.",
                },
                "agentId": {
                    "type": "string",
                    "description": (
                        "Current runtime Matrix user id, for example "
                        "@leader:matrix.local. For PROJECT_REQUESTED, this "
                        "must match sender.agent; do not pass role names such "
                        "as leader or workspace names such as default."
                    ),
                },
                "type": {
                    "type": "string",
                    "enum": ["PROJECT_REQUESTED"],
                    "description": "Top-level message type alias for trigger messages. PROJECT_REQUESTED is the v1 allowlisted self-trigger type.",
                },
                "message": {
                    "oneOf": [
                        {
                            "type": "object",
                            "required": ["type", "text"],
                            "additionalProperties": True,
                            "properties": {
                                "type": {
                                    "type": "string",
                                    "enum": ["PROJECT_REQUESTED"],
                                    "description": "Trigger type. v1 only allows PROJECT_REQUESTED.",
                                },
                                "text": {
                                    "type": "string",
                                    "description": "Synthetic current-event body to enqueue in the target Matrix task room.",
                                },
                            },
                        },
                        {"type": "string"},
                    ],
                    "description": (
                        "Message body. For PROJECT_REQUESTED handoff, pass a "
                        "JSON object with type and text, not a serialized JSON "
                        "string. Serialized JSON object strings are accepted "
                        "only for compatibility."
                    ),
                },
                "text": {
                    "type": "string",
                    "description": "Plain message text for ordinary cross-room or external sends. message and body aliases are also accepted.",
                },
                "dryRun": {
                    "type": "boolean",
                    "description": "Return the resolved payload without sending.",
                },
                "mentionSender": {
                    "type": "boolean",
                    "description": "For DingTalk requester reports, mention the original sender when session metadata has sender_staff_id.",
                },
            },
            "additionalProperties": True,
        },
    },
    "roomflow": {
        "description": (
            "Manage Matrix task rooms for TeamHarness execution-channel "
            "isolation: create a dedicated room for a project or quick task, "
            "list joined rooms, describe one Matrix room by room name/topic/tags, "
            "or archive a task room. Task rooms are internal Leader/Worker "
            "execution channels, not requester reply channels."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["create_task_room", "list_rooms", "describe_room", "archive_room"],
                    "description": "Room operation to perform.",
                },
                "taskId": {
                    "type": "string",
                    "description": "Safe task id or compatibility project id used when projectId is omitted.",
                },
                "projectId": {
                    "type": "string",
                    "description": "Safe project id. Project task rooms are named TASK：<projectId> and reused only by this id.",
                },
                "name": {
                    "type": "string",
                    "description": "Human-readable title for create_task_room. When projectId is present, Matrix room name uses TASK：<projectId>.",
                },
                "source": {
                    "type": "string",
                    "description": "Optional requester/source label such as matrix, dingtalk, or wechat. Source metadata is kept for context only and does not decide task-room reuse.",
                },
                "sourceRoomId": {
                    "type": "string",
                    "description": "Optional original requester room/conversation id kept as metadata for project/requester routing.",
                },
                "sender": {
                    "type": "string",
                    "description": "Optional original external sender identity kept as metadata. It does not decide task-room reuse.",
                },
                "topic": {
                    "type": "string",
                    "description": "Optional Matrix room topic. Defaults to a task-room topic.",
                },
                "invite": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Matrix user ids to invite. A comma-separated string is also accepted.",
                },
                "admin": {
                    "type": "string",
                    "description": "Optional Team Admin Matrix user id to invite and grant power level 100.",
                },
                "roomId": {
                    "type": "string",
                    "description": "Matrix room id for describe_room or archive_room, with or without room: prefix.",
                },
                "sessionId": {
                    "type": "string",
                    "description": "Matrix session id accepted by describe_room, for example matrix:!room:domain.",
                },
                "payload": {
                    "type": "object",
                    "description": "Room payload; flat arguments are also accepted.",
                    "additionalProperties": True,
                },
                "dryRun": {
                    "type": "boolean",
                    "description": "Return the resolved room operation without calling Matrix.",
                },
            },
            "additionalProperties": True,
        },
    },
    "filesync": {
        "description": (
            "Explicitly list, stat, pull, or push TeamHarness shared artifacts "
            "under shared/ or read-only global-shared. "
            "Use this for deliberate shared file operations, not periodic "
            "workspace sync or runtime package updates."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["list", "stat", "pull", "push"],
                    "description": "Shared artifact operation to perform.",
                },
                "path": {
                    "type": "string",
                    "description": "Relative path beginning with shared/ or global-shared/.",
                },
                "workspaceDir": {
                    "type": "string",
                    "description": "Runtime workspace containing the shared/ tree; usually inferred.",
                },
                "storage": {
                    "type": "object",
                    "description": "Optional storage prefixes such as sharedPrefix or globalSharedPrefix.",
                    "additionalProperties": True,
                },
                "exclude": {
                    "type": "array",
                    "description": "Optional patterns excluded during push.",
                    "items": {"type": "string"},
                },
                "dryRun": {
                    "type": "boolean",
                    "description": "Return the mc command without executing it.",
                },
            },
            "additionalProperties": True,
        },
    },
    "artifact": {
        "description": (
            "Publish a workspace file to a Matrix room as a standard m.file event. "
            "Use this for explicit user-visible room files, not shared storage sync. "
            "The file path must be relative to workspaceDir and must not escape it."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["publish_file"],
                    "description": "Publish one local workspace file to Matrix media and send an m.file event.",
                },
                "path": {
                    "type": "string",
                    "description": "Workspace-relative file path, for example reports/summary.md or shared/tasks/task-001/result.md.",
                },
                "filename": {
                    "type": "string",
                    "description": "Optional display filename for the Matrix room file. Defaults to the source basename.",
                },
                "target": {
                    "type": "string",
                    "description": "Matrix room target, for example room:!room:domain.",
                },
                "roomId": {
                    "type": "string",
                    "description": "Matrix room id target, with or without room: prefix.",
                },
                "parentEventId": {
                    "type": "string",
                    "description": "Optional Matrix text event id that this file should attach to.",
                },
                "attachmentParentEventId": {
                    "type": "string",
                    "description": "Alias for parentEventId.",
                },
                "matrixAttachmentParentEventId": {
                    "type": "string",
                    "description": "Alias for parentEventId.",
                },
                "replyRoute": {
                    "type": "object",
                    "description": "Optional Matrix replyRoute whose targetSession identifies the target room.",
                    "additionalProperties": True,
                },
                "workspaceDir": {
                    "type": "string",
                    "description": "Runtime workspace containing the file.",
                },
                "taskId": {
                    "type": "string",
                    "description": "Optional task id whose requester/leader the publish summary should mention when no parent event sender is resolvable.",
                },
                "mentions": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Optional full Matrix user ids (@localpart:domain) to mention in the publish summary event. Invalid values are dropped; short names are not expanded.",
                },
                "publishId": {
                    "type": "string",
                    "description": "Optional caller-chosen id for this publish. Retries of a failed or lost publish must reuse the same value (or omit it) so the homeserver deduplicates the same room+filename+content; pass a fresh value to deliberately re-notify on unchanged content.",
                },
            },
            "additionalProperties": True,
        },
    },
    "projectflow": {
        "description": (
            "Manage durable TeamHarness project state only after Quick Task "
            "or Project Work mode is selected: create quick projects, create "
            "projects, plan or update DAG and Loop work, query ready nodes, "
            "and record loop iterations. Do not use for ordinary direct "
            "replies or one-off checks. complete_project persists the "
            "terminal state locally and syncs it to shared storage before "
            "the PROJECT_COMPLETED event is emitted; if that sync fails the "
            "action returns a retryable failure with the event withheld, so "
            "retry it once storage recovers."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": [
                        "create_project",
                        "create_quick_project",
                        "resolve_project",
                        "plan_dag",
                        "plan_loop",
                        "ready_nodes",
                        "ready_loop_nodes",
                        "record_loop_iteration",
                        "accept_task_result",
                        "mark_requester_report_sent",
                        "pause_project",
                        "resume_project",
                        "complete_project",
                        "list_events",
                    ],
                    "description": "Project operation to perform.",
                },
                "team": {
                    "type": "string",
                    "description": "For list_events: team whose S2 event stream to read (default: the current runtime team).",
                },
                "since": {
                    "type": "string",
                    "description": "For list_events: RFC3339 timestamp; only events with occurred_at >= since.",
                },
                "types": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "For list_events: filter by event_type (the closed SIG-* markers).",
                },
                "taskId": {
                    "type": "string",
                    "description": "For list_events: only events for this task_id.",
                },
                "cursor": {
                    "type": "string",
                    "description": "For list_events: opaque pagination cursor from a previous next_cursor.",
                },
                "limit": {
                    "type": "integer",
                    "description": "For list_events: page size (default 50, max 200).",
                },
                "projectId": {
                    "type": "string",
                    "description": "Safe project id used under shared/projects/{projectId}.",
                },
                "payload": {
                    "type": "object",
                    "description": "Project payload; flat arguments are also accepted.",
                    "additionalProperties": True,
                },
                "tasks": {
                    "type": "array",
                    "description": "DAG or Loop task nodes for planning actions.",
                    "items": {"type": "object", "additionalProperties": True},
                },
                "taskId": {
                    "type": "string",
                    "description": "For plan_loop, the task the loop iterates; recorded in the loop dict as task_id and checked against the task meta.",
                },
                "loopId": {
                    "type": "string",
                    "description": "For plan_loop, the Loop CRD identity recorded as the consistency anchor in the loop dict; the CRD itself is not read by plan_loop. When omitted, the anchor of the existing loop is preserved on re-plans.",
                },
                "replyRoute": {
                    "type": "object",
                    "description": "Requester route for accepted outcome reports from external or cross-session requests.",
                    "additionalProperties": True,
                },
                "sourceRoomId": {
                    "type": "string",
                    "description": "Stable external requester room/conversation id to persist with external project and task state.",
                },
                "accepted": {
                    "type": "boolean",
                    "description": (
                        "For accept_task_result, false records revision for a "
                        "SUCCESS result. REVISION_NEEDED remains revision, while "
                        "BLOCKED and INTERRUPTED remain blocked."
                    ),
                },
                "submissionId": {
                    "type": "string",
                    "description": (
                        "Current submit_task identity required by normal "
                        "accept_task_result requests. Only migration of a "
                        "persisted legacy submission without an identity may omit it."
                    ),
                },
                "publishArtifacts": {
                    "type": "boolean",
                    "description": "For accept_task_result or complete_project, true explicitly publishes project artifacts immediately. Default is false so callers can publish after the requester report message exists.",
                },
                "workspaceDir": {
                    "type": "string",
                    "description": "Runtime workspace containing shared/projects.",
                },
            },
            "additionalProperties": True,
        },
    },
    "taskflow": {
        "description": (
            "Coordinate bounded TeamHarness tasks after a project node is ready: "
            "leader delegates and checks tasks; worker or remote-member "
            "acknowledges and submits results; worker or leader raises in-flight "
            "attention (approval / decision / escalation) with request_attention. "
            "Do not use for direct questions, readiness checks, or ordinary "
            "conversation."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "role": {
                    "type": "string",
                    "enum": ["leader", "worker", "remote-member"],
                    "description": "Caller TeamHarness role; inferred from runtime config when omitted.",
                },
                "action": {
                    "type": "string",
                    "enum": [
                        "delegate_task",
                        "ack_task",
                        "submit_task",
                        "check_task",
                        "cancel_task",
                        "report_progress",
                        "request_attention",
                    ],
                    "description": (
                        "Task lifecycle operation. report_progress records a "
                        "non-terminal progress note on an in-flight task (state "
                        "unchanged, no leader notification). request_attention "
                        "pulls a human decision (kind: approval / decision / "
                        "escalation / other) out of the group chat while the task "
                        "is still in flight. Pass payload.resolved=true to close "
                        "the open record of that kind (no new ping); the call is "
                        "rejected when no record of that kind exists."
                    ),
                },
                "projectId": {
                    "type": "string",
                    "description": "Safe project id associated with a delegated task.",
                },
                "taskId": {
                    "type": "string",
                    "description": "Safe task id used under shared/tasks/{taskId}.",
                },
                "submissionId": {
                    "type": "string",
                    "description": (
                        "Current submit_task identity required when cancel_task "
                        "resolves a normal submitted result."
                    ),
                },
                "payload": {
                    "type": "object",
                    "description": "Task payload; flat arguments are also accepted.",
                    "additionalProperties": True,
                },
                "spec": {
                    "type": "string",
                    "description": "Task execution contract for delegate_task.",
                },
                "summary": {
                    "type": "string",
                    "description": "Worker result summary for submit_task.",
                },
                "deliverables": {
                    "type": "array",
                    "description": "Shared deliverable paths included in submit_task.",
                    "items": {"type": "string"},
                },
                "reason": {
                    "type": "string",
                    "description": "Required cancellation reason for cancel_task.",
                },
                "replacementTaskId": {
                    "type": "string",
                    "description": "Optional replacement task id for cancel_task.",
                },
                "assignedTo": {
                    "type": "string",
                    "description": "Worker Matrix id or stable member name assigned to delegate_task.",
                },
                "workspaceDir": {
                    "type": "string",
                    "description": "Runtime workspace containing shared/tasks.",
                },
            },
            "additionalProperties": True,
        },
    },
}


