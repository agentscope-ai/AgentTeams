'use strict';

const fs = require('node:fs');

const DEFAULTS = Object.freeze({
  port: 8090,
  bindHost: '127.0.0.1',
  controllerUrl: 'http://127.0.0.1:8080',
  tokenFile: '/var/run/agentteams/cli-token',
  minioEndpoint: 'http://127.0.0.1:9000',
  bucket: 'agentteams-storage',
  upstreamTimeoutMs: 15_000,
  maxJsonBytes: 2 * 1024 * 1024,
  maxObjectBytes: 16 * 1024 * 1024,
  listDefaultLimit: 500,
  listMaxLimit: 1_000,
});

/**
 * Load and validate all dashboard configuration in one place.
 * Authentication is fail-closed: it is enabled unless explicitly disabled,
 * and disabling it is accepted only for a loopback bind address.
 */
function loadConfig(env = process.env, options = {}) {
  const warn = options.warn || ((message) => console.warn(message));
  const value = (current, legacy, fallback) => envValue(env, current, legacy, fallback, warn);

  const port = intValue(
    value('AGENTTEAMS_DASHBOARD_PORT', ['HICLAW_DASHBOARD_PORT', 'PORT'], String(DEFAULTS.port)),
    'AGENTTEAMS_DASHBOARD_PORT',
    { min: 1, max: 65535 },
  );
  const bindHost = value(
    'AGENTTEAMS_DASHBOARD_BIND_HOST',
    ['HICLAW_DASHBOARD_BIND_HOST'],
    DEFAULTS.bindHost,
  );
  const authDisabled = boolValue(
    value('AGENTTEAMS_DASHBOARD_AUTH_DISABLED', ['HICLAW_DASHBOARD_AUTH_DISABLED'], 'false'),
    'AGENTTEAMS_DASHBOARD_AUTH_DISABLED',
  );

  if (authDisabled && !isLoopbackBind(bindHost)) {
    throw new ConfigError('Dashboard authentication may only be disabled when binding to loopback');
  }

  let auth;
  if (authDisabled) {
    auth = { enabled: false, username: '', password: '' };
  } else {
    const username = requiredValue(
      value('AGENTTEAMS_DASHBOARD_USERNAME', ['HICLAW_DASHBOARD_USERNAME'], undefined),
      'AGENTTEAMS_DASHBOARD_USERNAME',
    );
    const passwordFile = requiredValue(
      value('AGENTTEAMS_DASHBOARD_PASSWORD_FILE', ['HICLAW_DASHBOARD_PASSWORD_FILE'], undefined),
      'AGENTTEAMS_DASHBOARD_PASSWORD_FILE',
    );
    const password = readSecretFile(passwordFile);
    if (!password) throw new ConfigError('Dashboard password file must not be empty');
    auth = { enabled: true, username, passwordFile, password };
  }

  const controllerUrl = validHttpUrl(
    value('AGENTTEAMS_CONTROLLER_URL', ['HICLAW_CONTROLLER_URL'], DEFAULTS.controllerUrl),
    'AGENTTEAMS_CONTROLLER_URL',
  );
  const tokenFile = value(
    'AGENTTEAMS_AUTH_TOKEN_FILE',
    ['HICLAW_AUTH_TOKEN_FILE'],
    DEFAULTS.tokenFile,
  );
  const upstreamTimeoutMs = intValue(
    value('AGENTTEAMS_DASHBOARD_UPSTREAM_TIMEOUT_MS', ['HICLAW_DASHBOARD_UPSTREAM_TIMEOUT_MS'], String(DEFAULTS.upstreamTimeoutMs)),
    'AGENTTEAMS_DASHBOARD_UPSTREAM_TIMEOUT_MS',
    { min: 1 },
  );
  const maxJsonBytes = intValue(
    value('AGENTTEAMS_DASHBOARD_MAX_JSON_BYTES', ['HICLAW_DASHBOARD_MAX_JSON_BYTES'], String(DEFAULTS.maxJsonBytes)),
    'AGENTTEAMS_DASHBOARD_MAX_JSON_BYTES',
    { min: 1 },
  );
  const maxObjectBytes = intValue(
    value('AGENTTEAMS_DASHBOARD_MAX_OBJECT_BYTES', ['HICLAW_DASHBOARD_MAX_OBJECT_BYTES'], String(DEFAULTS.maxObjectBytes)),
    'AGENTTEAMS_DASHBOARD_MAX_OBJECT_BYTES',
    { min: 1 },
  );
  const listMaxLimit = intValue(
    value('AGENTTEAMS_DASHBOARD_LIST_MAX_LIMIT', ['HICLAW_DASHBOARD_LIST_MAX_LIMIT'], String(DEFAULTS.listMaxLimit)),
    'AGENTTEAMS_DASHBOARD_LIST_MAX_LIMIT',
    { min: 1, max: 1000 },
  );
  const listDefaultLimit = intValue(
    value('AGENTTEAMS_DASHBOARD_LIST_DEFAULT_LIMIT', ['HICLAW_DASHBOARD_LIST_DEFAULT_LIMIT'], String(DEFAULTS.listDefaultLimit)),
    'AGENTTEAMS_DASHBOARD_LIST_DEFAULT_LIMIT',
    { min: 1, max: listMaxLimit },
  );
  const publicOriginValue = value(
    'AGENTTEAMS_DASHBOARD_PUBLIC_ORIGIN',
    ['HICLAW_DASHBOARD_PUBLIC_ORIGIN'],
    '',
  );

  return {
    port,
    bindHost,
    controllerUrl,
    tokenFile,
    publicOrigin: publicOriginValue ? validOrigin(publicOriginValue, 'AGENTTEAMS_DASHBOARD_PUBLIC_ORIGIN') : '',
    auth,
    limits: {
      upstreamTimeoutMs,
      maxJsonBytes,
      maxObjectBytes,
      listDefaultLimit,
      listMaxLimit,
    },
    minio: {
      endpoint: validHttpUrl(
        value('AGENTTEAMS_MINIO_ENDPOINT', [], env.MINIO_ENDPOINT || DEFAULTS.minioEndpoint),
        'AGENTTEAMS_MINIO_ENDPOINT',
      ),
      accessKey: value('AGENTTEAMS_MINIO_ACCESS_KEY', [], env.MINIO_ACCESS_KEY || env.MINIO_ACCESS || ''),
      secretKey: value('AGENTTEAMS_MINIO_SECRET_KEY', [], env.MINIO_SECRET_KEY || env.MINIO_SECRET || ''),
      bucket: value('AGENTTEAMS_FS_BUCKET', ['HICLAW_FS_BUCKET'], DEFAULTS.bucket),
    },
  };
}

class ConfigError extends Error {
  constructor(message) {
    super(message);
    this.name = 'ConfigError';
    this.code = 'INVALID_CONFIG';
  }
}

function envValue(env, currentName, legacyNames, fallback, warn) {
  if (hasValue(env, currentName)) return env[currentName];
  for (const name of legacyNames) {
    if (!hasValue(env, name)) continue;
    if (name.startsWith('HICLAW_')) {
      warn(name + ' is deprecated; use ' + currentName + ' instead');
    }
    return env[name];
  }
  return fallback;
}

function hasValue(env, name) {
  return Object.prototype.hasOwnProperty.call(env, name) && env[name] !== '' && env[name] !== undefined;
}

function requiredValue(value, name) {
  if (value === undefined || value === null || String(value).length === 0) {
    throw new ConfigError('Missing required environment variable: ' + name);
  }
  return String(value);
}

function intValue(value, name, { min, max } = {}) {
  if (!/^\d+$/.test(String(value))) throw new ConfigError(name + ' must be an integer');
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || (min !== undefined && parsed < min) || (max !== undefined && parsed > max)) {
    throw new ConfigError(name + ' is outside the allowed range');
  }
  return parsed;
}

function boolValue(value, name) {
  const normalized = String(value).toLowerCase();
  if (['true', '1', 'yes'].includes(normalized)) return true;
  if (['false', '0', 'no'].includes(normalized)) return false;
  throw new ConfigError(name + ' must be true or false');
}

function validHttpUrl(value, name) {
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    throw new ConfigError(name + ' must be a valid URL');
  }
  if (!['http:', 'https:'].includes(parsed.protocol)) {
    throw new ConfigError(name + ' must use http or https');
  }
  return parsed.toString().replace(/\/$/, '');
}

function validOrigin(value, name) {
  const normalized = validHttpUrl(value, name);
  const parsed = new URL(normalized);
  if (parsed.pathname !== '/' || parsed.search || parsed.hash || parsed.username || parsed.password) {
    throw new ConfigError(name + ' must contain only scheme and host');
  }
  return parsed.origin;
}

function isLoopbackBind(value) {
  const normalized = String(value).trim().toLowerCase().replace(/^\[|\]$/g, '');
  return normalized === '127.0.0.1' || normalized === '::1' || normalized === 'localhost';
}

function readSecretFile(secretFile) {
  let value;
  try {
    value = fs.readFileSync(secretFile, 'utf8');
  } catch (err) {
    throw new ConfigError('Unable to read dashboard password file: ' + (err.code || 'unknown error'));
  }
  return value.replace(/[\r\n]+$/, '');
}

const tokenCache = new Map();

function readTokenFile(tokenFile) {
  const stat = fs.statSync(tokenFile);
  const cached = tokenCache.get(tokenFile);
  if (cached && cached.mtimeMs === stat.mtimeMs) return cached.token;
  const token = fs.readFileSync(tokenFile, 'utf8').trim();
  if (!token) throw new ConfigError('Controller token file must not be empty');
  tokenCache.set(tokenFile, { mtimeMs: stat.mtimeMs, token });
  return token;
}

function invalidateTokenFile(tokenFile) {
  tokenCache.delete(tokenFile);
}

module.exports = {
  ConfigError,
  DEFAULTS,
  invalidateTokenFile,
  isLoopbackBind,
  loadConfig,
  readSecretFile,
  readTokenFile,
};
