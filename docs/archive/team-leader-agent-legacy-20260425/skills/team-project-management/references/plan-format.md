# DAG plan.md Format

```markdown
# Team Project: {title}

**ID**: {project-id}
**Parent Task**: {parent-task-id or "(none — Team Admin initiated)"}
**Status**: planning | active | completed
**Team**: {team-name}
**Created**: {ISO date}

## Workers

- @{worker1}:{domain} — {role description}
- @{worker2}:{domain} — {role description}

## DAG Task Plan

- [ ] st-01 — {task title} (assigned: @{worker}:{domain})
- [ ] st-02 — {task title} (assigned: @{worker}:{domain})
- [ ] st-03 — {task title} (assigned: @{worker}:{domain}, depends: st-01, st-02)
- [ ] st-04 — {task title} (assigned: @{worker}:{domain}, depends: st-02)
- [ ] st-05 — {task title} (assigned: @{worker}:{domain}, depends: st-03, st-04)

## Change Log

- {ISO datetime}: Project initiated
- {ISO datetime}: Plan confirmed
```

## Task Line Format

```
- [{marker}] {task-id} — {title} (assigned: @{worker}:{domain}[, depends: {id1}, {id2}])
```

### Fields

| Field | Required | Description |
|-------|----------|-------------|
| `{marker}` | Yes | Status: ` ` pending, `~` in-progress, `x` completed, `!` blocked, `→` revision |
| `{task-id}` | Yes | Unique ID, format: `st-NN` (e.g., `st-01`, `st-02`) |
| `{title}` | Yes | Brief task description |
| `assigned` | Yes | Worker Matrix ID (`@name:domain`) |
| `depends` | No | Comma-separated list of task IDs this task depends on |

### Status Markers

| Marker | Meaning |
|--------|---------|
| `[ ]` | Pending — not yet started |
| `[~]` | In-progress — worker is working |
| `[x]` | Completed |
| `[!]` | Blocked — needs attention |
| `[→]` | Revision in progress |

### Dependency Rules

- `depends: st-01, st-02` means this task can only start when **both** st-01 AND st-02 are `[x]`
- Tasks with **no** `depends:` clause can start immediately (parallel with other root tasks)
- The DAG must be **acyclic** — validate with `resolve-dag.sh --action validate`
- A task should only depend on tasks defined earlier in the list (convention, not enforced)

### task-id Format

Use `st-NN` (sequential numbering): `st-01`, `st-02`, ..., `st-15`.

For large projects, use descriptive suffixes: `st-01-schema`, `st-02-api-design`.

## Example: Web App Project

```markdown
## DAG Task Plan

- [ ] st-01 — Design database schema (assigned: @alice:domain)
- [ ] st-02 — Design API specification (assigned: @alice:domain)
- [ ] st-03 — Implement backend API (assigned: @alice:domain, depends: st-01, st-02)
- [ ] st-04 — Implement frontend pages (assigned: @bob:domain, depends: st-02)
- [ ] st-05 — Write unit tests (assigned: @charlie:domain, depends: st-03)
- [ ] st-06 — Integration testing (assigned: @charlie:domain, depends: st-03, st-04)
```

DAG visualization:
```
st-01 ──┐
        ├──→ st-03 ──→ st-05
st-02 ──┤         └──→ st-06
        └──→ st-04 ──────┘
```

Execution order:
1. **Wave 1** (parallel): st-01, st-02
2. **Wave 2** (after st-01+st-02): st-03; (after st-02): st-04
3. **Wave 3** (after st-03): st-05; (after st-03+st-04): st-06
