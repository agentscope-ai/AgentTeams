// Pure parsers for the v2 board/DAG views (plan Milestone 3, Step 3 --
// docs/implementation-milestone-3.md "Step 3"). Nothing here touches the
// DOM or the network -- every function takes plain data in and returns
// plain data out, which is what makes it unit-testable with `node --test`
// and no browser.
//
// Data sources these parsers cover (all read-only, no new endpoints):
//   - plan.md      (manager/agent/skills/project-management/references/plan-format.md)
//   - meta.json    (manager/agent/skills/project-management/references/task-lifecycle.md)
//   - state.json   (manager/agent/skills/task-management/scripts/manage-state.sh)
//
// Every parser here is defensive by construction: malformed/unexpected
// input degrades to an empty-but-valid result rather than throwing (ledger
// #3 -- live plan.md files are LLM-written and may drift from the
// documented format; ledger #4 -- meta.json status has only two confirmed
// literals, `assigned`/`completed`).

const TASK_LINE_RE = /^\s*-\s*\[([ x~!→])\]\s*(\S+)\s*—\s*(.*)$/;
const PHASE_HEADING_RE = /^\s*###\s+(.*)$/;
const ASSIGNED_RE = /assigned:\s*(@[^,)]+)/i;
const DEPENDS_ON_RE = /depends on:\s*([^,)]+)/i;

/**
 * countTaskMarkers scans plan.md lines for the task-status markers documented
 * in plan-format.md. Kept here (moved from panels/projects.js, v1's home for
 * it) so both the v1 progress-counts view and the v2 DAG/board share one
 * implementation -- projects.js re-exports it for backward compatibility.
 */
export function countTaskMarkers(markdown) {
  const counts = { pending: 0, inProgress: 0, done: 0, blocked: 0 };
  const lines = String(markdown || '').split('\n');
  for (const line of lines) {
    const m = line.match(/^\s*-\s*\[([ x~!→])\]/);
    if (!m) continue;
    switch (m[1]) {
      case ' ':
        counts.pending++;
        break;
      case '~':
        counts.inProgress++;
        break;
      case 'x':
        counts.done++;
        break;
      case '!':
        counts.blocked++;
        break;
      default:
        break;
    }
  }
  return counts;
}

/**
 * parsePlanTasks parses a plan.md document into an ordered list of phases,
 * each holding the task lines found under its `### Phase N: ...` heading.
 * Non-conforming lines (anything that isn't a `- [marker] task-id — title`
 * line) are simply skipped, never thrown on -- plan.md is LLM-authored and
 * real files may not conform byte-for-byte to plan-format.md (ledger #3).
 *
 * Tasks that appear before any `###` heading are collected into a synthetic
 * phase named "" (empty title) so nothing is silently dropped.
 *
 * @param {string} markdown
 * @returns {{phases: Array<{title: string, tasks: Array<PlanTask>}>}}
 *
 * @typedef {Object} PlanTask
 * @property {string} id
 * @property {string} title
 * @property {' '|'x'|'~'|'!'|'→'} marker
 * @property {string|null} assignee
 * @property {string|null} dependsOn
 */
export function parsePlanTasks(markdown) {
  const phases = [];
  let current = null;

  const ensureCurrent = () => {
    if (!current) {
      current = { title: '', tasks: [] };
      phases.push(current);
    }
    return current;
  };

  const lines = String(markdown || '').split('\n');
  for (const rawLine of lines) {
    const headingMatch = rawLine.match(PHASE_HEADING_RE);
    if (headingMatch) {
      current = { title: headingMatch[1].trim(), tasks: [] };
      phases.push(current);
      continue;
    }

    const taskMatch = rawLine.match(TASK_LINE_RE);
    if (!taskMatch) continue;

    const [, marker, id, rest] = taskMatch;
    const assignedMatch = rest.match(ASSIGNED_RE);
    const dependsMatch = rest.match(DEPENDS_ON_RE);
    // Title is everything before the trailing " (assigned: ...)" annotation,
    // if present; otherwise the whole remainder.
    const parenIdx = rest.indexOf(' (');
    const title = (parenIdx >= 0 ? rest.slice(0, parenIdx) : rest).trim();

    ensureCurrent().tasks.push({
      id: id.trim(),
      title,
      marker,
      assignee: assignedMatch ? assignedMatch[1].trim() : null,
      dependsOn: dependsMatch ? dependsMatch[1].trim() : null,
    });
  }

  // Drop a leading synthetic empty phase if it collected nothing (the
  // common case: every plan.md starts with a heading before any task line).
  if (phases.length > 0 && phases[0].title === '' && phases[0].tasks.length === 0) {
    phases.shift();
  }

  return { phases };
}

/**
 * KANBAN_STATUS buckets, in display order. "Active" is the catch-all: any
 * state.json active_tasks entry whose status isn't recognized as blocked
 * lands here with its raw status string shown as a badge (ledger #4 -- the
 * meta.json/state.json status vocabulary isn't fully enumerated, so unknown
 * strings must never be dropped or mis-classified as failure).
 */
export const KANBAN_COLUMNS = ['Active', 'Blocked', 'Completed', 'Cancelled'];

/**
 * bucketTasks builds the four kanban columns from the two data sources v2
 * is scoped to: the Manager's state.json (`active_tasks`/`cancelled_tasks`)
 * and a set of completed MinIO task ids (already resolved by the caller --
 * this function does no I/O). Never throws: every input defaults to `[]`/
 * `undefined`-safe, so a 404'd manager-tasks state (empty `stateTasks`) and
 * an empty `completedIds` degrade to four empty columns.
 *
 * @param {Object} [state] raw state.json-shaped object (may be undefined/null)
 * @param {string[]} [state.active_tasks]
 * @param {string[]} [state.cancelled_tasks]
 * @param {Iterable<string>} [completedIds] MinIO task ids with meta.status === 'completed'
 * @returns {{Active: object[], Blocked: object[], Completed: object[], Cancelled: object[]}}
 */
export function bucketTasks(state, completedIds) {
  const columns = { Active: [], Blocked: [], Completed: [], Cancelled: [] };

  const active = (state && Array.isArray(state.active_tasks) && state.active_tasks) || [];
  const cancelled = (state && Array.isArray(state.cancelled_tasks) && state.cancelled_tasks) || [];
  const activeIds = new Set(active.map((t) => t && t.task_id).filter(Boolean));

  for (const task of active) {
    if (!task) continue;
    if (task.status === 'blocked') {
      columns.Blocked.push(task);
    } else {
      // Any other status (including missing/unknown) is Active; the raw
      // status string, if present, is preserved on the task for the UI to
      // show as a badge rather than guessing at a known bucket.
      columns.Active.push(task);
    }
  }

  for (const task of cancelled) {
    if (!task) continue;
    columns.Cancelled.push(task);
  }

  const ids = completedIds ? Array.from(completedIds) : [];
  for (const id of ids) {
    if (!id || activeIds.has(id)) continue;
    columns.Completed.push({ task_id: id });
  }

  return columns;
}

/**
 * capRecentIds sorts task ids by their `task-YYYYMMDD-HHMMSS` timestamp
 * prefix (descending -- most recent first) and caps the result at `limit`.
 * Ids that don't match the expected id format still sort (lexicographically,
 * after the timestamped ones) rather than being dropped -- this bounds the
 * N+1 meta.json fetches the Completed column would otherwise need for every
 * task ever run, per the Step-3 brief ("cap ~50 recent").
 *
 * @param {string[]} ids
 * @param {number} [limit]
 */
export function capRecentIds(ids, limit = 50) {
  const list = Array.isArray(ids) ? ids.slice() : [];
  list.sort((a, b) => {
    const ta = taskTimestamp(a);
    const tb = taskTimestamp(b);
    if (ta !== null && tb !== null) return tb - ta;
    if (ta !== null) return -1;
    if (tb !== null) return 1;
    return String(a).localeCompare(String(b));
  });
  return list.slice(0, limit);
}

function taskTimestamp(id) {
  const m = String(id || '').match(/^task-(\d{8})-(\d{6})$/);
  if (!m) return null;
  const n = Number(m[1] + m[2]);
  return Number.isNaN(n) ? null : n;
}

/**
 * latestProgressFile picks the "current" progress note out of a
 * `progress/` directory listing (files named `YYYY-MM-DD.md`, per the
 * Step-3 brief: "latest = highest filename"). Plain lexicographic max works
 * because the filename format sorts chronologically. Returns null for an
 * empty/missing list rather than throwing.
 *
 * @param {Array<{key: string}|string>} files
 * @returns {string|null}
 */
export function latestProgressFile(files) {
  if (!Array.isArray(files) || files.length === 0) return null;
  const names = files.map((f) => (typeof f === 'string' ? f : f && f.key)).filter(Boolean);
  if (names.length === 0) return null;
  return names.reduce((max, cur) => (cur > max ? cur : max));
}
