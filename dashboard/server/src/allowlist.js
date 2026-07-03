'use strict';

// Route classification for the dashboard proxy (plan Milestone 2, Step 3 —
// docs/implementation-milestone-2.md "Scope" #1). This module holds NO I/O:
// it only decides, for a given method + path, whether the request is
// allowed and where it should go. Keeping this pure makes it trivially
// unit-testable without spinning up sockets or mocking HTTP frameworks.
//
// Allowlist (exactly, per the brief):
//   GET  /api/managers                     -> controller GET /api/v1/managers
//   GET  /api/managers/<rest>               -> controller GET /api/v1/managers/<rest>
//   GET  /api/teams[...]                    -> controller GET /api/v1/teams[...]
//   GET  /api/workers[...]                  -> controller GET /api/v1/workers[...]
//   GET  /api/manager-tasks                 -> controller GET /api/v1/manager-tasks
//   GET  /api/projects[...]                 -> controller GET /api/v1/projects[...]
//   GET  /api/tasks/<key>                   -> MinIO GET  (shared/tasks/<key>)
//   GET  /api/files/<key>                   -> MinIO GET  (list-or-object, under shared/ + agents/)
//   POST /api/workers/{name}/wake           -> controller POST /api/v1/workers/{name}/wake
//   POST /api/workers/{name}/sleep          -> controller POST /api/v1/workers/{name}/sleep
//   POST /api/workers/{name}/ensure-ready   -> controller POST /api/v1/workers/{name}/ensure-ready
//   POST /api/managers/{name}/message       -> controller POST /api/v1/managers/{name}/message
//   POST /api/teams/{name}/message          -> controller POST /api/v1/teams/{name}/message
// Everything else -> not allowed (404 for unknown path shapes, 405 for a
// known path with a disallowed method).
//
// `/docker/` is never proxied -- it simply has no route here, ever.

const CONTROLLER_LIST_KINDS = new Set(['managers', 'teams', 'workers', 'manager-tasks', 'projects']);

const WRITE_ACTIONS = new Set(['wake', 'sleep', 'ensure-ready']);

// Kinds that support the message-injection write (plan Milestone 3, Step 1 --
// docs/implementation-milestone-3.md "Step 1"). Path shape only:
// /api/<managers|teams>/{name}/message.
const MESSAGE_KINDS = new Set(['managers', 'teams']);

// MinIO-backed prefixes the file/task browsers may read under.
const MINIO_ALLOWED_ROOTS = new Set(['shared', 'agents']);

/**
 * @typedef {Object} RouteDecision
 * @property {'controller'|'minio'} target
 * @property {'get'|'write'} kind
 * @property {string} [controllerPath]   full path to call on the controller, e.g. "/api/v1/managers"
 * @property {string} [minioKey]         object-storage key (no leading slash), e.g. "shared/tasks/xyz/meta.json"
 * @property {string} [workerName]
 * @property {string} [action]
 */

/**
 * classify decides how (or whether) to proxy a request.
 *
 * @param {string} method  HTTP method, upper-case
 * @param {string} pathname  URL pathname only (no query string), e.g. "/api/workers"
 * @returns {{ ok: true, route: RouteDecision } | { ok: false, status: 404|405 }}
 */
function classify(method, pathname) {
  const segments = splitPath(pathname);

  if (segments.length === 0 || segments[0] !== 'api') {
    return { ok: false, status: 404 };
  }

  // /api/tasks/<...rest>  -> MinIO, rooted at shared/tasks/<...rest>
  if (segments[1] === 'tasks') {
    if (method !== 'GET') return { ok: false, status: 405 };
    const rest = segments.slice(2);
    const key = normalizeMinioKey(['shared', 'tasks', ...rest]);
    if (key === null) return { ok: false, status: 404 };
    return { ok: true, route: { target: 'minio', kind: 'get', minioKey: key } };
  }

  // /api/files/<...rest>  -> MinIO, rooted at whichever of shared/ or agents/
  // the caller asks for (rest[0] must be one of those roots).
  if (segments[1] === 'files') {
    if (method !== 'GET') return { ok: false, status: 405 };
    const rest = segments.slice(2);
    if (rest.length === 0 || !MINIO_ALLOWED_ROOTS.has(rest[0])) {
      return { ok: false, status: 404 };
    }
    const key = normalizeMinioKey(rest);
    if (key === null) return { ok: false, status: 404 };
    return { ok: true, route: { target: 'minio', kind: 'get', minioKey: key } };
  }

  // /api/workers/{name}/wake|sleep|ensure-ready -> controller POST (write, logged)
  if (segments[1] === 'workers' && segments.length === 4 && WRITE_ACTIONS.has(segments[3])) {
    if (method !== 'POST') return { ok: false, status: 405 };
    const workerName = segments[2];
    if (!workerName) return { ok: false, status: 404 };
    return {
      ok: true,
      route: {
        target: 'controller',
        kind: 'write',
        controllerPath: `/api/v1/workers/${encodeURIComponent(workerName)}/${segments[3]}`,
        workerName,
        action: segments[3],
      },
    };
  }

  // /api/managers|teams/{name}/message -> controller POST (write, logged).
  // The OUTER condition checks ONLY the path shape (never the method) so a
  // GET to this exact shape falls through to the branch below it and is
  // rejected there (405, since GET IS a valid method for the shorter
  // "/api/managers|teams/..." GET-passthrough shape) rather than being
  // mis-routed as a controller GET passthrough for a path the controller
  // doesn't actually serve as GET. This exactly mirrors the WRITE_ACTIONS
  // branch above: shape-match the outer condition, method-check inside.
  if (
    MESSAGE_KINDS.has(segments[1]) &&
    segments.length === 4 &&
    segments[3] === 'message'
  ) {
    if (method !== 'POST') return { ok: false, status: 405 };
    const targetKind = segments[1];
    const targetName = segments[2];
    if (!targetName) return { ok: false, status: 404 };
    return {
      ok: true,
      route: {
        target: 'controller',
        kind: 'write',
        controllerPath: `/api/v1/${targetKind}/${encodeURIComponent(targetName)}/message`,
        action: 'message',
        targetKind,
        targetName,
      },
    };
  }

  // /api/managers|teams|workers|manager-tasks|projects[/...]  -> controller GET passthrough
  if (CONTROLLER_LIST_KINDS.has(segments[1])) {
    if (method !== 'GET') return { ok: false, status: 405 };
    const controllerPath = '/api/v1/' + segments.slice(1).map(encodeURIComponent).join('/');
    return { ok: true, route: { target: 'controller', kind: 'get', controllerPath } };
  }

  return { ok: false, status: 404 };
}

/** splitPath splits and decodes a URL pathname into non-empty segments. */
function splitPath(pathname) {
  return pathname
    .split('/')
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
    .map((s) => {
      try {
        return decodeURIComponent(s);
      } catch {
        return s;
      }
    });
}

/**
 * normalizeMinioKey joins segments into a "shared/..." or "agents/..." style
 * key and rejects any path-traversal attempt (".." segments, or a decoded
 * segment that still contains a "/" after decoding, e.g. "%2e%2e").
 * Returns null if the key is unsafe or empty.
 */
function normalizeMinioKey(segments) {
  if (segments.length === 0) return null;
  for (const seg of segments) {
    if (seg === '..' || seg === '.' || seg.includes('\\') || seg.includes('\0')) {
      return null;
    }
  }
  const key = segments.join('/');
  // Defense in depth: reject if the joined key, once split again on '/',
  // contains a traversal segment (guards a segment that decoded into
  // something containing an embedded slash, e.g. "a%2f..").
  if (key.split('/').some((p) => p === '..' || p === '.')) {
    return null;
  }
  return key;
}

module.exports = { classify, MINIO_ALLOWED_ROOTS, WRITE_ACTIONS, MESSAGE_KINDS };
