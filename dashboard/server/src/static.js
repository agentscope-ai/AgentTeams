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
 * createStaticFileServer returns a resolver(pathname) -> {contentType, body}
 * for files under `rootDir` (the built Vite SPA's dist/). Falls back to
 * index.html for any path without a file extension, so client-side routing
 * (if added later) works; unknown extensioned paths return null (404).
 *
 * @param {string} rootDir
 */
function createStaticFileServer(rootDir) {
  const indexPath = path.join(rootDir, 'index.html');

  return function resolve(pathname) {
    const safe = path.normalize(pathname).replace(/^([.][.][/\\])+/, '');
    let filePath = path.join(rootDir, safe);

    if (filePath !== rootDir && !filePath.startsWith(rootDir + path.sep)) {
      return null; // traversal guard (separator-suffixed so a sibling like <rootDir>-evil can't pass)
    }

    if (!fs.existsSync(filePath) || fs.statSync(filePath).isDirectory()) {
      if (path.extname(pathname)) return null;
      filePath = indexPath;
    }

    if (!fs.existsSync(filePath)) return null;

    const ext = path.extname(filePath);
    const contentType = CONTENT_TYPES[ext] || 'application/octet-stream';
    return { contentType, body: fs.readFileSync(filePath) };
  };
}

module.exports = { createStaticFileServer };
