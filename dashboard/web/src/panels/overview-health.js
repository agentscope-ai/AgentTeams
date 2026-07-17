export function parseRFC3339(value) {
  if (!value) return null;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? null : ms;
}

export function formatAge(fromMs, nowMs = Date.now()) {
  const deltaMs = Math.max(0, nowMs - fromMs);
  const sec = Math.floor(deltaMs / 1000);
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.floor(hr / 24);
  return `${day}d ago`;
}

/** Map a heartbeat/active timestamp to freshness label + badge class. */
export function freshnessFromAge(ts, nowMs = Date.now()) {
  const parsed = parseRFC3339(ts);
  if (parsed === null) {
    return { label: 'unknown', cls: 'badge-unknown', age: null };
  }
  const ageMin = (nowMs - parsed) / 60000;
  if (ageMin < 10) return { label: 'healthy', cls: 'badge-ready', age: formatAge(parsed, nowMs) };
  if (ageMin < 30) return { label: 'degraded', cls: 'badge-degraded', age: formatAge(parsed, nowMs) };
  return { label: 'down', cls: 'badge-failed', age: formatAge(parsed, nowMs) };
}

export function healthDotClass(status) {
  switch (status) {
    case 'healthy':
      return 'health-dot-healthy';
    case 'degraded':
      return 'health-dot-degraded';
    case 'down':
      return 'health-dot-down';
    default:
      return 'health-dot-unknown';
  }
}
