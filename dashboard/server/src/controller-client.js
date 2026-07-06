'use strict';

// Thin client for the hiclaw-controller REST API. Injects the admin Bearer
// token server-side; the browser never sees it, and this module never
// echoes the Authorization header (or any token material) back out.

const http = require('node:http');
const https = require('node:https');
const { URL } = require('node:url');

class ControllerClient {
  /**
   * @param {Object} opts
   * @param {string} opts.baseUrl        e.g. "http://127.0.0.1:8080"
   * @param {() => string} opts.getToken function returning the current bearer token
   * @param {() => void} [opts.onUnauthorized] called when the upstream returns 401,
   *        so the caller can invalidate a cached token (e.g. after rotation)
   */
  constructor(opts) {
    this.baseUrl = new URL(opts.baseUrl);
    this.getToken = opts.getToken;
    this.onUnauthorized = opts.onUnauthorized;
  }

  _transport() {
    return this.baseUrl.protocol === 'https:' ? https : http;
  }

  /**
   * request performs an upstream call.
   *
   * @param {string} method
   * @param {string} path   path + query, e.g. "/api/v1/workers?foo=bar"
   * @param {Buffer} [body]
   * @returns {Promise<{statusCode: number, headers: Object, body: Buffer}>}
   */
  request(method, path, body) {
    const token = this.getToken();
    const transport = this._transport();
    const headers = {
      authorization: `Bearer ${token}`,
      accept: 'application/json',
    };
    if (body && body.length) {
      headers['content-type'] = 'application/json';
      headers['content-length'] = String(body.length);
    }

    return new Promise((resolve, reject) => {
      const req = transport.request(
        {
          protocol: this.baseUrl.protocol,
          hostname: this.baseUrl.hostname,
          port: this.baseUrl.port || (this.baseUrl.protocol === 'https:' ? 443 : 80),
          path,
          method,
          headers,
        },
        (res) => {
          const chunks = [];
          res.on('data', (c) => chunks.push(c));
          res.on('end', () => {
            if (res.statusCode === 401 && this.onUnauthorized) {
              this.onUnauthorized();
            }
            resolve({
              statusCode: res.statusCode,
              headers: sanitizeResponseHeaders(res.headers),
              body: Buffer.concat(chunks),
            });
          });
        },
      );
      req.on('error', reject);
      if (body && body.length) req.write(body);
      req.end();
    });
  }
}

// sanitizeResponseHeaders strips anything that could leak the admin token
// (or hop-by-hop headers we don't want forwarded) before the response is
// relayed to the browser. content-length and content-encoding are also
// stripped: the body we hand back has already been fully buffered/relayed
// as-is by the caller, and forwarding a stale content-length (or an encoding
// that doesn't match what's actually sent) can leave the browser hanging on
// a mismatched-length response -- Node recomputes content-length itself when
// it's omitted.
function sanitizeResponseHeaders(headers) {
  const out = {};
  for (const [k, v] of Object.entries(headers || {})) {
    const lower = k.toLowerCase();
    if (
      lower === 'authorization' ||
      lower === 'set-cookie' ||
      lower === 'connection' ||
      lower === 'transfer-encoding' ||
      lower === 'content-length' ||
      lower === 'content-encoding'
    ) {
      continue;
    }
    out[lower] = v;
  }
  return out;
}

module.exports = { ControllerClient, sanitizeResponseHeaders };
