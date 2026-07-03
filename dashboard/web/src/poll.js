// Small polling helper: runs `fn` immediately, then every `intervalMs`,
// until `stop()` is called. Errors from `fn` are forwarded to `onError`
// rather than stopping the poll -- a transient upstream hiccup shouldn't
// kill the whole panel.

export function startPolling(fn, intervalMs, onError) {
  let stopped = false;
  let timer = null;

  async function tick() {
    if (stopped) return;
    try {
      await fn();
    } catch (err) {
      if (onError) onError(err);
    }
    if (!stopped) {
      timer = setTimeout(tick, intervalMs);
    }
  }

  tick();

  return function stop() {
    stopped = true;
    if (timer) clearTimeout(timer);
  };
}
