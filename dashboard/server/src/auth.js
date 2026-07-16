'use strict';

const crypto = require('node:crypto');

const REALM = 'agentteams-dashboard';

/**
 * Constant-time comparison of two secret strings. Returns true only when the
 * decoded values are byte-for-byte equal AND of equal length (timingSafeEqual
 * throws on length mismatch, so we guard it and return false in that case --
 * an unequal length is already a definitive mismatch, never a match).
 */
function safeEqual(a, b) {
  const bufA = Buffer.from(String(a), 'utf8');
  const bufB = Buffer.from(String(b), 'utf8');
  if (bufA.length !== bufB.length) return false;
  if (bufA.length === 0) return true; // both empty -> equal
  return crypto.timingSafeEqual(bufA, bufB);
}

/**
 * createBasicAuth builds a request-authentication middleware for HTTP Basic
 * auth. It is dependency-free (Node builtins only) and constant-time in its
 * credential comparison.
 *
 * @param {Object} auth  the `config.auth` object from loadConfig()
 * @param {boolean} [auth.enabled]  when false (loopback mode) every request is allowed
 * @param {string}  [auth.username] expected username
 * @param {string}  [auth.password] expected password
 * @returns {(req: object) => { ok: boolean }} a function that inspects the
 *          incoming request's Authorization header and returns whether the
 *          request is authenticated.
 */
function createBasicAuth(auth) {
  // Loopback / explicitly-disabled mode: never challenge, always allow.
  // This branch is reached only when loadConfig accepted auth-disabled because
  // the bind address was loopback, so there is no network exposure.
  if (!auth || auth.enabled === false) {
    return function allowAll() {
      return { ok: true };
    };
  }

  const expectedUser = auth.username;
  const expectedPass = auth.password;

  return function checkAuth(req) {
    const header = req && req.headers ? req.headers.authorization : undefined;
    if (!header || typeof header !== 'string') return { ok: false };

    // Header form: "Basic <base64(user:pass)>". Matching is case-insensitive
    // on the scheme token (per RFC 7235 §2.1); the credentials payload is
    // compared exactly via safeEqual below.
    const match = /^Basic\s+([A-Za-z0-9+/=_-]+)\s*$/.exec(header);
    if (!match) return { ok: false };

    let decoded;
    try {
      decoded = Buffer.from(match[1], 'base64').toString('utf8');
    } catch {
      return { ok: false };
    }

    // The credential string is "user:pass"; the password may itself contain
    // colons, so split only on the first colon.
    const sep = decoded.indexOf(':');
    if (sep < 0) return { ok: false };
    const user = decoded.slice(0, sep);
    const pass = decoded.slice(sep + 1);

    if (!safeEqual(user, expectedUser)) return { ok: false };
    if (!safeEqual(pass, expectedPass)) return { ok: false };

    return { ok: true };
  };
}

module.exports = { createBasicAuth, REALM, safeEqual };