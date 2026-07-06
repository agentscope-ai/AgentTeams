// Small polling helper: runs `fn` immediately, then every `intervalMs`,
// until `stop()` is called. Errors from `fn` are forwarded to `onError`
// rather than stopping the poll -- a transient upstream hiccup shouldn't
// kill the whole panel.
//
// While the document is hidden (background tab), the timer is paused rather
// than firing on a schedule the user can't see -- resuming (tab refocused)
// immediately ticks once so the view is fresh again, then resumes the normal
// interval.

export function startPolling(fn, intervalMs, onError) {
  let stopped = false;
  let timer = null;
  let running = false;

  async function tick() {
    if (stopped) return;
    if (document.hidden) {
      // Don't schedule a background timer; visibilitychange will resume us.
      timer = null;
      return;
    }
    // Guard against overlapping ticks: `timer` is null while a previous tick is
    // still awaiting fn(), so a visibilitychange-triggered tick() could otherwise
    // start a second concurrent fn() call. The in-flight tick reschedules itself.
    if (running) return;
    running = true;
    try {
      await fn();
    } catch (err) {
      if (onError) onError(err);
    } finally {
      running = false;
    }
    if (!stopped && !document.hidden) {
      timer = setTimeout(tick, intervalMs);
    }
  }

  function onVisibilityChange() {
    if (stopped) return;
    if (document.hidden) {
      // Cancel the pending scheduled tick so we don't fire on a background
      // schedule -- becoming visible again always re-ticks immediately
      // instead of waiting out whatever was left of the paused interval.
      if (timer) clearTimeout(timer);
      timer = null;
    } else if (!timer && !running) {
      tick();
    }
  }

  document.addEventListener('visibilitychange', onVisibilityChange);

  tick();

  return function stop() {
    stopped = true;
    if (timer) clearTimeout(timer);
    document.removeEventListener('visibilitychange', onVisibilityChange);
  };
}
