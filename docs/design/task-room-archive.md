# Task room archive

Archive completed, accepted task rooms after their artifacts are uploaded to
the workspace. Archived rooms disappear from every local member's Matrix sync,
while messages, memberships and workspace artifacts remain available. Restore
before continuing collaboration. Private chats, groups and Team rooms are not
eligible.

## Request flow and permissions

Dashboard -> Dashboard API -> Controller -> Tuwunel.

The Controller authenticates the existing Human Matrix token or agent Service
Account token. It resolves the current Team CR using `room.meta.teamName` and
the Team's effective storage name. It does not trust browser role parameters,
room power levels or old `teamAdmin` metadata for write authorization.

| Caller | Archive / restore | Delete |
| --- | --- | --- |
| Admin / L1 (ALE) | Yes | Yes |
| Manager | Yes | Yes |
| Current owning Team admin | Yes | Yes |
| Current owning Team Leader | Yes | No |
| Other users | No | No |

Deletion additionally requires an already archived task room. Lifecycle writes
use the Controller's homeserver administrator credential, which stays on the
server. Tuwunel rejects direct lifecycle writes by ordinary Matrix identities.
Successful Controller writes record an audit event with the authenticated caller.

## API and CLI

```text
GET  /api/v1/task-rooms/archived
GET  /api/v1/task-rooms/{roomId}/archive
POST /api/v1/task-rooms/{roomId}/archive
POST /api/v1/task-rooms/{roomId}/unarchive
POST /api/v1/task-rooms/{roomId}/delete

agt task-room list --team <storage-name> --team-admin <matrix-user-id>
agt task-room list --human-member <matrix-user-id>
agt task-room status <room-id>
agt task-room archive <room-id>
agt task-room unarchive <room-id>
agt task-room delete <room-id>
```

List filters `team`, `team_admin` and `human_member` are ANDed. They narrow the
caller's joined archived rooms and accessible Team scope; they never impersonate
the filtered account. Existing Matrix APIs and history permissions serve messages
and attachments. Status returns `is_task_room`, `archived`, `can_archive` and
`can_delete`; Dashboard controls use these server capabilities.

## Packaging and upgrade

The matching Tuwunel source is `agentteams-group/tuwunel-agentteams`, branch
`feat/room-archive`, based on upstream stable v2.0.0. Its server-owned archive
flag uses existing room account-data storage, without a feature-specific schema
migration. Upstream version upgrades can still run their own migrations; retain
a pre-upgrade database backup for rollback.

AgentTeams currently supplies a creator power-level override incompatible with
the upstream room-version-12 default. Embedded startup and Helm retain room
version 10 for newly created rooms. Existing rooms are not rewritten. Helm's
startup probe allows first-boot migrations to finish before liveness checks.

Tuwunel 2.0 logs out devices by default on an admin password reset. Initial
password assignment preserves the newly issued session. Its successful-password
login limit also remains enabled; integration tests reuse one Admin session
across cases rather than logging in repeatedly.

The embedded Controller image copies Tuwunel's static `/usr/bin/tuwunel`; rebuild
it after integrating the image pin. The standalone Helm deployment uses that
Tuwunel image directly. Rebuild Dashboard for its Controller relay and archive UI.
Archive does not delete workspace artifacts. Permanent room purge follows the
homeserver's existing local deletion behavior and does not purge other servers.
