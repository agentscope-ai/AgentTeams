'use strict';

const { classify } = require('./allowlist');

// Cap on request bodies read for allow-listed writes (plan Milestone 3,
// Step 1). Message bodies are operator-typed chat instructions -- there is
// no legitimate reason for one to approach this, and an unbounded read is a
// memory-exhaustion vector for the proxy process.
const MAX_BODY_BYTES = 64 * 1024;

// Audit-log body preview cap (plan Milestone 3, Step 1, decision #17): the
// preview exists to help an operator recognize *which* message a log line
// refers to without ever persisting the full (possibly sensitive) body.
const BODY_PREVIEW_MAX_CHARS = 120;

/**
 * createRequestHandler builds a Node http(s) request listener.
 *
 * All upstream I/O is via the injected `controllerClient` / `minioClient`
 * so this module (and its tests) never make a real network call.
 *
 * @param {Object} deps
 * @param {import('./controller-client').ControllerClient} deps.controllerClient
 * @param {import('./minio-client').MinioClient} deps.minioClient
 * @param {(entry: object) => void} deps.logWrite  called once per allow-listed write
 * @param {Buffer} [deps.spaIndex]   optional static SPA index.html to serve for non-/api paths
 * @param {(pathname: string) => {contentType: string, body: Buffer} | null} [deps.staticFile]
 *        optional resolver for serving the built SPA's static assets
 */
function createRequestHandler(deps) {
  const { controllerClient, minioClient, logWrite, staticFile } = deps;

  return async function handleRequest(req, res) {
    let url;
    try {
      url = new URL(req.url, 'http://internal');
    } catch {
      sendJson(res, 400, { error: 'bad request' });
      return;
    }

    if (!url.pathname.startsWith('/api/')) {
      if (staticFile) {
        const asset = staticFile(url.pathname);
        if (asset) {
          res.writeHead(200, { 'content-type': asset.contentType });
          res.end(asset.body);
          return;
        }
      }
      sendJson(res, 404, { error: 'not found' });
      return;
    }

    const method = (req.method || 'GET').toUpperCase();
    const decision = classify(method, url.pathname);

    if (!decision.ok) {
      sendJson(res, decision.status, { error: decision.status === 405 ? 'method not allowed' : 'not found' });
      return;
    }

    const route = decision.route;

    try {
      if (route.target === 'controller') {
        let body;
        if (route.kind === 'write') {
          try {
            body = await readBody(req, MAX_BODY_BYTES);
          } catch (err) {
            if (err && err.code === 'BODY_TOO_LARGE') {
              sendJson(res, 413, { error: 'request body too large' });
              return;
            }
            throw err;
          }
        }
        const controllerPath = route.controllerPath + (url.search || '');
        const upstream = await controllerClient.request(method, controllerPath, body);

        if (route.kind === 'write') {
          if (route.action === 'message') {
            logWrite({
              action: 'message',
              kind: route.targetKind,
              target: route.targetName,
              status: upstream.statusCode,
              remoteAddr: req.socket ? req.socket.remoteAddress : undefined,
              bodyLen: body ? body.length : 0,
              bodyPreview: bodyPreview(body),
            });
          } else {
            logWrite({
              action: route.action,
              worker: route.workerName,
              status: upstream.statusCode,
              remoteAddr: req.socket ? req.socket.remoteAddress : undefined,
            });
          }
        }

        relayJson(res, upstream);
        return;
      }

      if (route.target === 'minio') {
        await handleMinio(res, minioClient, route.minioKey, url.pathname);
        return;
      }

      sendJson(res, 500, { error: 'unhandled route target' });
    } catch (err) {
      sendJson(res, 502, { error: 'upstream request failed', detail: String((err && err.message) || err) });
    }
  };
}

async function handleMinio(res, minioClient, key, requestedPath) {
  // /api/files/<root>/ (trailing slash implied by no extension AND the
  // caller wanting a directory listing) is ambiguous from the path alone,
  // so we try GetObject first (covers meta.json/plan.md/state files) and
  // fall back to a prefix listing on 404 -- this covers both the file
  // browser (directories) and direct file reads (objects) through one route.
  const getRes = await minioClient.getObject(key);
  if (getRes.statusCode === 200) {
    res.writeHead(200, { 'content-type': guessContentType(key) });
    res.end(getRes.body);
    return;
  }
  if (getRes.statusCode !== 404) {
    sendJson(res, 502, { error: 'minio object read failed', status: getRes.statusCode });
    return;
  }

  // Fall back to a listing under key + '/'.
  const prefix = key.endsWith('/') ? key : key + '/';
  try {
    const listing = await minioClient.listObjects(prefix);
    if (listing.prefixes.length === 0 && listing.objects.length === 0) {
      sendJson(res, 404, { error: 'not found', path: requestedPath });
      return;
    }
    sendJson(res, 200, {
      prefix,
      directories: listing.prefixes.map((p) => p.slice(prefix.length).replace(/\/$/, '')),
      files: listing.objects
        .filter((o) => o.key !== prefix)
        .map((o) => ({ key: o.key.slice(prefix.length), size: o.size, lastModified: o.lastModified })),
    });
  } catch (err) {
    sendJson(res, 502, { error: 'minio list failed', detail: String((err && err.message) || err) });
  }
}

function guessContentType(key) {
  if (key.endsWith('.json')) return 'application/json';
  if (key.endsWith('.md')) return 'text/markdown; charset=utf-8';
  return 'application/octet-stream';
}

/**
 * readBody buffers the request body, rejecting with a `BODY_TOO_LARGE`-coded
 * error (never resolving, never calling the upstream) once more than
 * `maxBytes` have been received.
 */
function readBody(req, maxBytes) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let total = 0;
    let rejected = false;
    req.on('data', (c) => {
      if (rejected) return;
      total += c.length;
      if (typeof maxBytes === 'number' && total > maxBytes) {
        rejected = true;
        const err = new Error('request body too large');
        err.code = 'BODY_TOO_LARGE';
        reject(err);
        return;
      }
      chunks.push(c);
    });
    req.on('end', () => {
      if (rejected) return;
      resolve(Buffer.concat(chunks));
    });
    req.on('error', (err) => {
      if (rejected) return;
      reject(err);
    });
  });
}

/**
 * bodyPreview renders a truncated, log-safe preview of a request body for
 * the audit trail. Never returns the full body -- callers must not log
 * `body` itself (plan #17: message bodies may carry sensitive instructions).
 */
function bodyPreview(body) {
  if (!body || body.length === 0) return '';
  const text = body.toString('utf8');
  if (text.length <= BODY_PREVIEW_MAX_CHARS) return text;
  return text.slice(0, BODY_PREVIEW_MAX_CHARS);
}

function relayJson(res, upstream) {
  const headers = { ...upstream.headers };
  if (!headers['content-type']) headers['content-type'] = 'application/json';
  res.writeHead(upstream.statusCode, headers);
  res.end(upstream.body);
}

function sendJson(res, status, obj) {
  const body = Buffer.from(JSON.stringify(obj));
  res.writeHead(status, { 'content-type': 'application/json', 'content-length': String(body.length) });
  res.end(body);
}

module.exports = { createRequestHandler };
