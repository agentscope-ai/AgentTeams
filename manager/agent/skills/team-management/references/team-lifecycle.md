# Team Lifecycle

## Team States

- **Active**: Leader and workers are running, team is operational
- **Degraded**: Some workers stopped or unavailable, Leader still running
- **Pending**: Team references are valid but required Worker identities or rooms are not ready yet

Check status: `agt get team <TEAM_NAME>`

## Adding a Worker to an Existing Team

1. Create the Worker CR first if it does not already exist
2. Update `Team.spec.workerMembers` via `agt update team` or `agt apply -f`
3. Controller joins the referenced Worker to the Team Room and updates coordination context

## Removing a Worker from a Team

1. Remove the Worker reference from `Team.spec.workerMembers`
2. Controller removes Team-owned communication policy and updates coordination context
3. The Worker CR, container, credentials, and Worker-owned configuration remain intact

## Deleting a Team

1. Delete the team: `agt delete team <TEAM_NAME>`
2. Controller cleans up Team rooms, Team storage, and Team-owned coordination context
3. Referenced Worker CRs and runtimes remain intact

## Task Delegation to Teams

When Manager receives a task that semantically matches a Team's name,
description, Leader, or Worker roster:

1. Use `manage-state.sh --action add-finite --delegated-to-team <TEAM>` to track
2. @mention the Team Leader in the Leader Room with the task
3. Team Leader handles decomposition and assignment internally
4. Manager only checks with Team Leader for progress (never team workers)

The Team API does not expose structured team-level domain/expertise/capability
fields for automatic filtering. Worker-level skills may describe individual
members, but Manager delegation is not backed by a structured Team filter.
