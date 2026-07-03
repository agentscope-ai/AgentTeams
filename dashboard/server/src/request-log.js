'use strict';

// Server-side audit log for the three allow-listed write actions
// (wake/sleep/ensure-ready) -- plan Milestone 2 Step 3, "the #17 audit
// trail". Kept as a tiny injectable sink so tests can capture lines without
// touching real stdout or a filesystem.

/**
 * createRequestLogger returns a function `log(entry)` that writes one JSON
 * line per call to `sink` (defaults to process.stdout.write).
 *
 * @param {(line: string) => void} [sink]
 */
function createRequestLogger(sink) {
  const write = sink || ((line) => process.stdout.write(line));
  return function logWrite(entry) {
    const line =
      JSON.stringify({
        ts: new Date().toISOString(),
        ...entry,
      }) + '\n';
    write(line);
  };
}

module.exports = { createRequestLogger };
