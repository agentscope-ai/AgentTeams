English | [中文](../zh-cn/usage/single-and-multi-user.md)

# Single-user and multi-user deployments

AgentTeams uses **one deployment model** whether you are the only person using the instance or you are onboarding a whole organisation. There is no "single-user mode" or "multi-user mode" to choose at install time: you install the platform once, and the number of humans you add afterwards changes who can do what — not how the system is deployed, upgraded, or operated.

This page explains what stays identical, what changes as you add people, and where each role fits.

## The short answer

- **Same installation, same upgrade path, same storage layout, same APIs.** One person or fifty people use the same deployment.
- **Each Team requires exactly one Leader.** The controller rejects a Team whose `workerMembers` has zero or more than one `team_leader`, so team coordination, team-memory broadcasting and event relaying always have an owner. This is a per-team invariant, not an installation requirement: an installation can contain no Team at all, and standalone Workers are valid. The recommended default topology — Leader + Workers, optionally with a Manager — is a recommendation, not a constraint.
- **A Manager is optional.** It turns controller API and CLI operations into a conversation on its admin DM, and it is available to **L1 (administrator)** users only. A deployment can run without one: the dashboard and the workbench plugin cover point-and-click operations (premises and gaps: see the Manager section below).
- **Personal memory belongs to the person, not to the Manager.** It is stored per human and reached through the controller API, so it works the same whether or not a Manager exists (design proposal — #1412; not available today).
- **Adding a person is a resource change, not a deployment change**: create a Human, grant a permission level, add the person to teams.

## Proposed, not yet available

The capabilities below are **design proposals, not available today**; they are not part of this release's behaviour:

- Per-human file area, file API, trash and quota — issue #1414 (implementation PR #1426, open).
- Per-human memory anchor — issue #1412 (V16 pending).
- Team-facts write path — issue #1412, Phase 2.

These are designs, not behaviour in this release; the rest of this document describes only what is available today. Until they land, treat personal memory as not available today rather than as something a Manager owns.

## Roles

| Role | Required? | Responsibility |
|---|---|---|
| **Controller** | Yes | Platform layer: resources, containers, storage, permissions, gateway configuration |
| **Leader** | **Yes — exactly one Leader per Team** | Team-facing coordination: task breakdown, assignment, team-memory broadcast, event relay. Required inside every Team; not an installation-level requirement |
| **Worker** | As needed | Executes focused tasks with its own model, runtime, skills and MCP configuration |
| **Human** | At least one | Sets goals, observes work, intervenes; each human has a permission level and their own scope |
| **Manager** | **Optional** | Conversational operations for L1: creates and manages resources by talking to the platform |

> "Required" describes the scope where the controller enforces it, not the shape of the deployment: the Leader requirement applies **inside each Team** (exactly one `team_leader` in `workerMembers`). Teams themselves are optional and standalone Workers are allowed. "Optional" (the Manager) is neither required nor forbidden. See "Enforced today" below.

The Controller is the platform layer and Workers are what you instantiate; a Leader is required *inside each Team*. Humans and the Manager are additions you make to a running deployment.

## Using the instance with one person

1. Install AgentTeams as usual (local or Kubernetes) and keep the recommended default topology (Leader + Workers).
2. Create the person:
   - the **administrator (L1)** identity administers the deployment. L1 humans have Matrix accounts like everyone else; their administrative API operations use the admin credential, while L2/L3 humans authenticate with their own Matrix token;
   - a **team member (L2)** identity is scoped to the teams listed in `accessibleTeams`.
3. Add the person to the team that should do the work (team membership and `humanMembers`).
4. Work through the team room: the human states a goal, the Leader decomposes and assigns it, Workers execute, and the human follows and intervenes in the same room.
5. Optionally install a Manager if the administrator wants conversational operations on the admin DM.

Nothing in this flow is "the single-user variant": it is the same sequence you would follow with ten people.

## Using the instance with several people

Add people one at a time; nothing is reinstalled and no mode is switched.

| Permission level | Who it is for | What it can do |
|---|---|---|
| **L1** (administrator) | Deployment owner / platform operator | Administers the instance, all resources and all humans; the only level that can use a Manager's conversational operations |
| **L2** (team-scoped) | Engineers, analysts, reviewers | Works inside the teams listed in `accessibleTeams`; optional capabilities extend specific surfaces |
| **L3** (worker-scoped) | People who only need to read a specific Worker's output | Read-only access to the workers listed in `accessibleWorkers` |

Practical rules:

- **One Human resource per person.** Display name, identity source and permission level live on that resource; the person's scope follows them.
- **Access follows membership.** A person's work area lives in their own namespace and is shared by default with the teams they belong to; memory stays personal (both proposed — see "Proposed, not yet available"). Team content stays inside the team's prefixes, people do not read another person's area, and a person only sees the teams and workers they were granted.
- **Teams scale independently of people.** Add a team when a group needs its own Leader and Workers; a person can belong to several teams.
- **Rooms mirror membership.** Each team has its own room; a person joins the rooms of the teams they belong to.
- **Removing a person** removes their membership and access. Deleting their stored data is a separate, explicit action — retention is not implied by removal.

## What stays the same

| Area | Same for one person and for many |
|---|---|
| Installation | One install path: local or Kubernetes, same values and same CRDs |
| Upgrades and runtime switching | Same upgrade flow, same runtime adapters, same rollback |
| Storage layout | Same object-storage prefixes; per-human scope is additive, not a separate deployment |
| APIs | Same controller API; per-human resources simply have more instances |
| Permissions | Same L1/L2/L3 model, evaluated per request |
| Operations | Same logs, audit trail, backup and migration procedures |

## What changes as you add people

| Area | What changes |
|---|---|
| Human resources | More `Human` objects, each with its own level, teams and capabilities |
| Team membership | More team rooms and more memberships; `humanMembers` and `accessibleTeams` grow |
| Permission matrix | More principals to review; capabilities are granted per person |
| Personal data | More per-human work areas (shared with their teams) and memory anchors (personal) — proposed, not available today (see "Proposed, not yet available") |

## The Manager in one-person and multi-person deployments

**Intended model.**

- **Each Team requires exactly one Leader** (enforced per team — see "Enforced today"); the **Manager is optional** — a convenience layer for L1 operations, not an execution dependency. The recommended default topology (Leader + Workers, optionally a Manager) is a recommendation, not a deployment constraint: an installation can run with no Team, or with standalone Workers.
- **L1 only.** Only administrators use its conversational surface; L2 and L3 humans work through the dashboard, the workbench plugin and their own scope.
- **Its memory is operational memory.** What was created, incidents, operational history — stored in its own agent prefix and `manager/*`. It never owns or writes a person's memory.

**Enforced today, and what covers its absence.**

- **Leader — enforced per Team.** When a Team is created or updated, the controller validates that `workerMembers` contains exactly one member with role `team_leader` and rejects zero or multiple. It does not require an installation to contain a Team, and it does not restrict standalone Workers.
- **Manager — not enforced.** Nothing in the controller requires or forbids a Manager; a deployment simply runs without one.
- **Covered without a Manager.** The dashboard and the workbench plugin cover **declarative resources and the read-only operational surface**: creating and editing resources, inspecting state, following team work. This assumes the installed controller version and the workbench plugin provide those surfaces.
- **Not covered.** Conversational operations, some cross-resource flows, and upgrade/migration orchestration still need a Manager or direct controller API/CLI. These substitute paths have not been verified item by item in this document.
- If you remove the Manager, review the operational flows you relied on before treating the point-and-click surfaces as sufficient.

## Personal work area and personal memory (proposed design)

Nothing in this section is available today.

These follow the person, not the Manager:

- Layout: `humans/<human>/files/` (the **work area**), `humans/<human>/memory/` and `humans/<human>/digest/` (long-term memory).
- Default access: the person and **the teams they belong to** (Leader and Workers) can read and write `files/**` — it is a shared work area, not a personal cloud drive. `memory/` and `digest/` stay personal.
- Writes: an agent serving the person submits entries through the **controller API**, which attributes them to the person's scope; the person reads and edits their own scope through the same API, and teams write the work area with their team identity.
- Isolation: no agent — including the Manager — is granted storage access to `humans/*`; scope is enforced by the API, and non-member teams get 404.
- Deletions go to a **remote recycle bin** (restorable, default retention 30 days); sync is switched on per device and scoped to the folders the user selects.

## FAQ

**Do I choose a deployment mode for one person?**
**Today:** available. No deployment mode to choose — install once; the number of humans is a resource decision, not an installation option.

**Can I run without a Leader?**
**Today:** yes, as long as you do not run a Team. An installation can contain no Team at all, and standalone Workers are allowed. What is not possible is a Team without exactly one Leader: the controller rejects a Team whose `workerMembers` has zero or more than one `team_leader` (see the Manager section).

**Can I run without a Manager?**
**Today:** available. The dashboard and the workbench plugin cover declarative resources and the read-only operational surface (premises and gaps: see the Manager section); the Manager adds a conversational surface for L1.

**Where does personal memory live if I do not install a Manager?**
**Today:** not available — per-human memory is a design proposal (#1412). Once it lands: the per-human namespace, reachable through the controller API — it does not depend on the Manager.

**Can one person belong to several teams?**
**Today:** available. Team membership and `accessibleTeams` define the scope; a person can work in more than one team.

**Do L2 and L3 humans do the same things?**
**Today:** available. L2 is team-scoped and can work inside its teams; L3 is read-only and limited to the workers assigned to it.

**Does adding people change the upgrade or backup procedure?**
**Today:** available. The same procedures apply; there are simply more human-scoped resources to include.

## See also

- [Overview](../overview.md)
- [Quickstart](../quickstart.md)
- [Manager guide](manager-guide.md)
- [Worker guide](worker-guide.md)
- [Resource management](resource-management.md)
- [Architecture](../design/architecture.md)
