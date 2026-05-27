# Team Lifecycle

## Team States

- **Active**: Leader and workers are running, team is operational
- **Degraded**: Some workers stopped or unavailable, Leader still running
- **Stopped**: All containers stopped (can be restarted)

Check status: `hiclaw get team <TEAM_NAME>`

## Adding a Worker to an Existing Team

1. Update the team via `hiclaw apply -f` with the new worker added to the workers list
2. Controller handles: creates Worker CR, joins Team Room, updates Leader's coordination context

## Removing a Worker from a Team

1. Update the team via `hiclaw apply -f` with the worker removed from the workers list
2. Controller handles: removes Worker CR, updates Leader's coordination context

## Deleting a Team

1. Delete the team: `hiclaw delete team <TEAM_NAME>`
2. Controller handles: deletes all worker containers, cleans up rooms, removes storage

## Task Delegation to Teams

When Manager receives a task that semantically matches a Team's name,
description, Leader, or Worker roster:

1. Use `manage-state.sh --action add-finite --delegated-to-team <TEAM>` to track
2. @mention the Team Leader in the Leader Room with the task
3. Team Leader handles decomposition and assignment internally
4. Manager only checks with Team Leader for progress (never team workers)

The Team registry and Team API do not expose structured domain/expertise fields
for automatic filtering.
