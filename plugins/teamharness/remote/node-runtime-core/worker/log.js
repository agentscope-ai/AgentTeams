#!/usr/bin/env node
"use strict";

function normalizeError(error) {
  if (!error) return undefined;
  if (error instanceof Error) {
    const payload = {
      name: error.name || "Error",
      message: error.message || String(error)
    };
    if (error.code) payload.code = error.code;
    return payload;
  }
  return String(error);
}

function normalizeField(value) {
  if (value === undefined || typeof value === "function") return undefined;
  if (value instanceof Error) return normalizeError(value);
  if (Array.isArray(value)) {
    return value.map(normalizeField).filter(item => item !== undefined);
  }
  if (value && typeof value === "object") {
    const result = {};
    for (const [key, nested] of Object.entries(value)) {
      const normalized = normalizeField(nested);
      if (normalized !== undefined) result[key] = normalized;
    }
    return result;
  }
  return value;
}

function logEvent(level, event, fields) {
  const payload = {
    ts: new Date().toISOString(),
    level: String(level || "info"),
    event: String(event || "worker_event")
  };
  for (const [key, value] of Object.entries(fields || {})) {
    const normalized = normalizeField(value);
    if (normalized !== undefined) payload[key] = normalized;
  }
  const line = JSON.stringify(payload);
  if (payload.level === "error" || payload.level === "warn") {
    console.error(line);
  } else {
    console.log(line);
  }
}

module.exports = {
  logEvent,
  normalizeError,
  normalizeField
};
