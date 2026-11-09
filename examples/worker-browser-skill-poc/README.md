# Optional browser skill POC

This example evaluates an optional skill on a disposable QwenPaw Worker
container. It does not change published images, running Workers, the creation
UI, or the trusted dependency-setup lifecycle.

See the [proposal](../../docs/design/worker-browser-automation.md) and
[中文方案](../../docs/zh-cn/design/worker-browser-automation.md).

## Package and assignment boundary

The skill is `browser-automation/` with SKILL.md and two shell scripts.
It has no assign_when rule, so role matching must not select it implicitly.
The package installs agent-browser 0.41.2 into a dedicated directory and
Chromium/fonts using Debian packages. The npm version is fixed; Debian package
versions depend on the configured repository in this POC.

Existing skill upload/assignment preserves these scripts but does not execute
install.sh automatically. Production integration still needs a reviewed
setup/probe/retry step for explicitly selected Workers. The browser files live
in the selected container; a new/replaced container needs setup again.

This installer requires a root Debian-based Docker Worker container and network
access. It must not run on a host. Non-root, read-only, offline and Kubernetes
setups are unverified.

## Reproduce

From the repository root:

```bash
docker build -t agentteams/browser-skill-poc:local examples/worker-browser-skill-poc
docker run --rm --init --memory 2g --cpus 2 --shm-size 256m \
  agentteams/browser-skill-poc:local -lc \
  'bash /poc/browser-automation/scripts/install.sh && python3 /poc/test_browser.py'
```

The root-container launch policy must be checked on the selected CLI/browser.
Do not assume a successful POC proves sandbox compatibility in production.
The tests use only a deterministic local HTTP fixture, not external accounts.

To retain screenshots and logs, omit --rm, give the container a temporary name,
then use docker cp on /tmp/browser-poc-results before removing it. Follow your
normal package mirror/proxy configuration if upstream downloads are unavailable;
the example does not configure mirrors on the host.

To package the optional skill (generated ZIP stays out of git):

```bash
python3 - <<'PY'
from pathlib import Path
from zipfile import ZipFile, ZIP_DEFLATED
root = Path("examples/worker-browser-skill-poc")
with ZipFile("/tmp/browser-automation-poc.zip", "w", ZIP_DEFLATED) as archive:
    for path in sorted((root / "browser-automation").rglob("*")):
        if path.is_file():
            archive.write(path, path.relative_to(root))
PY
python3 manager/agent/skills/worker-management/scripts/safe-extract-worker-skill.py \
  --archive /tmp/browser-automation-poc.zip --output-dir /tmp/browser-skill-check
```

Upload that ZIP through the existing marketplace flow, then select it for an
idle test Worker only. Actual upload/distribution and a live Agent/Matrix task
are not part of this CLI/container POC.

## Evidence boundaries

The test records commands and timings under /tmp/browser-poc-results:
navigation, interactive snapshot, reference-based fill, click with assertion,
scroll, Chinese screenshot, upload, download, and two task sessions with
separate cookies. It closes both sessions in a finally block.

Two sessions in one container are not two Worker containers. A working shell
command inside a QwenPaw image does not show that QwenPaw has discovered the skill
or reasoned through the task. Browser sandbox behavior, actual filesystem
locations, cleanup, package-install cost and remaining integration limitations
must be documented with the observed results.

## Observed result (2026-11-09)

Tested on native Linux ARM64 using QwenPaw Worker v1.2.6, digest
`sha256:580a3e747dfdb0b6950f7144f669f3ffb8fbfa83cdcb7bee2b97e84b80f1e886`,
with agent-browser 0.41.2 and Debian Chromium 156.0.8078.79.

- The original disposable Worker had neither agent-browser nor Chromium.
  Installing the skill's dependencies did not change the published image.
  The ZIP passed the existing safe extractor with hasAssignWhen=false.
- All 16 workflow commands plus two close commands passed on the controlled
  page. The confirmed run took 8.408 s; browser opens took 3.942/3.767 s;
  warm operations took 0.030–0.069 s (median 0.0485 s). These are a local
  sample, not a latency benchmark or real-model task time.
- Screenshot inspection confirmed readable Chinese text, the submitted value
  and uploaded filename. Download content and Cookie separation were asserted.
- Browser profile/cache/socket directories were under the configured temporary
  tree; the test workspace stayed empty and no ~/.agent-browser directory was
  observed. This is not an object-storage synchronization test.
- Debian reported 251 MB of archives and 845 MB additional installed disk
  space, with downloads taking 1 min 49 s after switching mirrors. The CLI
  installation occupied about 115 MiB separately. Browser operations are fast
  after setup, but first-install cost is material.

Failures and remaining limits:

- Debian/npm upstream downloads stalled. The disposable container used Debian
  and npm mirrors; no host configuration was changed. Canceling a slow npm
  download initially left its process active, causing ETXTBSY during a second
  install. The old process was stopped and the workflow passed again.
  The example installer now rejects concurrent installs using flock.
- The first test parsed an element ref from raw JSON incorrectly; parsing the
  structured refs map fixed the test. Every action is checked against actual
  page/file state rather than trusting a success exit code alone.
- The CLI launched Chromium with --no-sandbox under this root container.
  This proves functionality, not a production sandbox policy. Non-root/sandbox
  compatibility remains a release gate.
- No live Chromium processes remained after close; the initial tail-as-PID-1
  harness retained zombie children. Reproduction uses --init; verify reaping
  with the real Worker entrypoint before release.
- Agent discovery, automatic dependency setup on selection, container-rebuild
  recovery, two independent Worker containers, amd64, other runtimes and
  Kubernetes were not exercised.

## Native ARM64 headless follow-up

The lighter candidate uses agent-browser 0.41.2 with
@sparticuz/chromium-min 153.0.0 and its ARM64 headless-shell pack. Browser pack
and Simplified Chinese font inputs are pinned and checked with SHA-256.
The installer keeps only the current platform's CLI binary and one Chinese
font, and does not install Debian Chromium or fonts-noto-cjk.
The npm helper is used for extraction; its Chromium.args defaults are not
applied. The existing agent-browser launch path is retained.

Reproduce in the same disposable harness:

```bash
docker build -t agentteams/browser-skill-poc:local examples/worker-browser-skill-poc
docker run --rm --init --memory 2g --cpus 2 --shm-size 256m \
  agentteams/browser-skill-poc:local -lc \
  'bash /poc/browser-automation/scripts/install-headless.sh && \
   AGENT_BROWSER_EXECUTABLE_PATH=/tmp/chromium \
   LD_LIBRARY_PATH=/tmp/al2023/lib:/tmp FONTCONFIG_PATH=/tmp/fonts \
   BROWSER_SKILL_EXTENDED=1 python3 /poc/test_browser.py'
```

For individual commands after installation, use
`bash /poc/browser-automation/scripts/headless.sh`.
The installer accepts BROWSER_SLIM_ARCHIVE / BROWSER_SLIM_FONT for cached
local inputs, verifies the same digests, and deletes its staging directory.
Retain third-party licenses when redistributing the resulting environment.

### Measured result

| Check | Full Debian Chromium baseline | Headless follow-up |
| --- | --- | --- |
| Browser version | 156.0.8078.79 | 153.0.8010.0 |
| Dependency footprint | About 845 MB from apt + 115 MiB CLI directory | 270,159,751 bytes clean image increment (about 258 MiB) |
| Same 16 workflow commands | 8.408 s | 7.077 s |
| Browser open calls | 3.942 / 3.767 s | 3.407 / 3.226 s |
| Additional rendering checks | Not run | 12 commands passed; total 28 commands took 7.548 s |

The headless runtime files comprise approximately 190 MiB browser executable,
24 MiB graphics libraries, 4 MiB bundled runtime libraries, 16 MiB fonts and
23 MiB CLI/helper installation. These are expanded sizes, not download sizes.
The browser pack alone is 68,249,600 compressed bytes; font and npm downloads
are additional.

To measure the actual image change, the test removed input archives, npm cache,
browser profiles and generated outputs from the disposable container, then
committed a temporary local image. Base size was 1,937,651,076 bytes; candidate
size was 2,207,810,827 bytes. The clean image also launched and closed a fresh
browser from a new container. No image was published or substituted for a live
Worker.

The original form, Chinese screenshot, upload/download and task-session Cookie
checks passed. The additional fixture exercised CSS Grid, gradient/shadow
rendering, SVG, Canvas 2D with an asserted pixel value, asynchronous state
updates, select input and coordinate-based mouse clicking. Both screenshots
were visually inspected. With --init, only init and the harness process
remained after close; no browser or zombie children remained in that check.

The first extended run exposed that pinned CLI 0.41.2 expects integer mouse
coordinates. Rounding the element center fixed that test and the complete
28-command run passed. This was a CLI argument compatibility fix.

These are individual local samples across different browser versions, not a
performance benchmark or proof of equivalence on arbitrary websites.
The earlier root/no-sandbox limitation remains. Browser dependencies extracted
under /tmp need a proper lifecycle for tmpfs/container replacement; production
activation, amd64 and Kubernetes remain unverified.
