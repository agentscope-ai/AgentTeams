'use strict';

const fs = require('node:fs');
const path = require('node:path');

const CONTENT_TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2',
};

/**
 * walkFiles recursively lists every regular file under `dir`, returning
 * absolute paths (path.join(dir, ...)). The static cache is keyed by these
 * absolute paths and looked up via path.join(rootDir, safe) in resolve().
 *
 * @param {string} dir
 * @returns {string[]}
 */
function walkFiles(dir) {
  const out = [];
  let entries;
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return out;
  }
  for (const entry of entries) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...walkFiles(full));
    } else if (entry.isFile()) {
      out.push(full);
    }
  }
  return out;
}

/**
 * createStaticFileServer returns a resolver(pathname) -> {contentType, body}
 * for files under `rootDir` (the built Vite SPA's dist/). Falls back to
 * index.html for any path without a file extension, so client-side routing
 * (if added later) works; unknown extensioned paths return null (404).
 *
 * The dist/ directory is built once and treated as immutable for the life
 * of the process, so every file under it is read into memory a single time
 * here (at server startup) rather than doing existsSync/statSync/
 * readFileSync on every request.
 *
 * @param {string} rootDir
 */
function createStaticFileServer(rootDir) {
  const indexPath = path.join(rootDir, 'index.html');
  const cache = new Map(); // absolute filePath -> {contentType, body}

  for (const filePath of walkFiles(rootDir)) {
    const ext = path.extname(filePath);
    const contentType = CONTENT_TYPES[ext] || 'application/octet-stream';
    cache.set(filePath, { contentType, body: fs.readFileSync(filePath) });
  }

  const indexAsset = cache.get(indexPath) || null;

  return function resolve(pathname) {
    const safe = path.normalize(pathname).replace(/^([.][.][/\\])+/, '');
    let filePath = path.join(rootDir, safe);

    if (filePath !== rootDir && !filePath.startsWith(rootDir + path.sep)) {
      return null; // traversal guard (separator-suffixed so a sibling like <rootDir>-evil can't pass)
    }

    let asset = cache.get(filePath);
    if (!asset) {
      if (path.extname(pathname)) return null;
      asset = indexAsset;
    }

    return asset || null;
  };
}

module.exports = { createStaticFileServer };
