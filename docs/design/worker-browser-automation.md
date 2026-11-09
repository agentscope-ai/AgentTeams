# Optional Worker Browser Skill Proposal

Status: proposed, with a disposable-container POC.
Feature tracking: [#1569](https://github.com/agentscope-ai/AgentTeams/issues/1569).
[中文方案](../zh-cn/design/worker-browser-automation.md)

## Decision and goal

Ship an official browser skill in the skill catalog, **unassigned by default**.
When creating a Worker, a user can select this skill, choose another browser
skill, or omit browser capability. Do not add it to every Worker's mandatory
builtin template or require a new core Worker image for each skill update.

The official candidate bundles short instructions and a pinned CLI installer.
Its dependency installation runs only inside the selected Worker's execution
environment. Browser binaries are environment dependencies, not files embedded
in the skill ZIP. The browser runs in that Worker's container and network.

This replaces the initial proposal of automatically enabling browser capability
for all supported Workers. Official catalog availability and per-Worker
activation are separate decisions.

The first target is QwenPaw, OpenClaw, and Hermes on Docker and Kubernetes,
subject to per-runtime/architecture acceptance. Legacy CoPaw is not a new-install
target. DeepSeek Harness and OpenHuman require separate validation. Host Chrome,
visible desktop takeover, persistent login across replacement, and a central
browser service remain outside the first implementation.

## Existing architecture and the missing part

Source baseline: upstream main `7e9a1c73`, inspected on 2026-11-09.

| Current source | Verified behavior | Design consequence |
| --- | --- | --- |
| [Worker skill guide](../usage/worker-guide.md) | Creation/editing can select marketplace or Nacos skills; distribution uploads the package and reconciles Worker.spec.skills | Reuse the existing selector and explicit assignments |
| [Safe skill importer](../../manager/agent/skills/worker-management/scripts/safe-extract-worker-skill.py) | Validates a ZIP and preserves files under a single skill root | Instructions and installer scripts can travel as one optional package |
| [Runtime projection](../../agentteams-controller/internal/service/runtime_config.go) / [QwenPaw application](../../qwenpaw/src/qwenpaw_worker/update.py) | Desired skills are synchronized, refreshed, and enabled | Preserve the existing activation path and user choices |
| [Builtin selection](../../agentteams-controller/internal/service/builtin_templates.go) | Mandatory templates and catalog availability share selection rules | Do not turn the optional browser package into a mandatory template builtin |
| [QwenPaw image](../../qwenpaw/Dockerfile) | Does not explicitly provision Chromium or agent-browser | Skill file delivery alone cannot make a browser usable |

The existing ZIP/assignment flow does **not** declare or execute a system-package
installation lifecycle. Do not describe upload, selected assignment, or API
enablement as successful browser setup. The POC calls its installer explicitly;
automatic setup during Worker creation is subsequent integration work.

## Alternatives

| Delivery model | Benefit | Cost |
| --- | --- | --- |
| Browser dependencies in every Worker image | Fast first use after image pull, simpler offline use | Bigger images; tool version updates require rebuilding affected images |
| Optional skill with environment setup | Per-Worker choice; skill/CLI version evolves separately from the core image | First-use network/download cost, package permissions, reinstall after replacement |
| Optional skill using a browser-equipped image or existing endpoint | Useful for offline, locked-down or remote-browser deployments | Requires compatible image/endpoint provisioning and separate credentials/lifecycle |

Recommend the second as the default design to validate, while allowing the third
where local installation is unavailable. Reuse ordinary skill packages rather
than adding a new browser-provider CRD. Another browser skill can supply its own
documented environment requirements; choosing it must not also enable the
official candidate.

## Selected-Worker setup and update flow

1. Publish the versioned official package to the existing skill catalog. Its
   source is proposed under `manager/agent/worker-skills/`, not mandatory
   `worker-agent/skills/`. Omit automatic role-matching rules that would
   silently select it.
2. The user explicitly selects skill names in the current Worker creation form.
   Preserve those names in Worker.spec.skills; do not union in browser defaults.
3. Materialize selected packages using current upload/assignment ordering.
4. Add a bounded environment-setup step for the official, reviewed package:
   verify its identity/version, install its dependencies in the target Worker,
   then run an executable/launch probe. Reuse the existing execution boundary;
   do not execute arbitrary uploaded install scripts simply because a ZIP has
   a scripts directory. This setup hook is proposed, not implemented today.
5. Refresh/load the selected skill through its runtime adapter. Report separately
   whether files, dependencies, and runtime activation succeeded. A setup failure
   remains retryable and must not be displayed as browser-ready.
6. On Worker replacement, check environment capability and restore the selected
   dependency version. A cached skill ZIP is not a cached Chromium installation.

Only selected Workers pay the installation cost. Upgrading skill text alone
does not replace an already running CLI/browser pair: install/probe the selected
version before switching, close old sessions while idle, then activate. If setup
fails, retain the previous working version and report the error. Removing a
skill prevents subsequent use and closes its sessions; do not automatically
purge shared system libraries used by another skill.

For read-only root filesystems, non-root images without package-install rights,
or offline hosts, use a pre-provisioned compatible image/endpoint or report the
requirement. Do not grant privilege or silently install on the host. The POC's
root Debian Docker installer is not a production solution for those cases.

## Skill contract and browser execution

The optional package contains SKILL.md plus explicit installation and execution
scripts. The discovery instructions address the Worker as "you", name supported
runtimes/OS/architectures, and explain actual setup cost and restrictions.

The candidate uses pinned agent-browser plus local Chromium. Native ARM64 needs
a compatible browser source: the current Chrome for Testing installer does not
have a Linux ARM64 target. The POC evaluates Debian Chromium; production packages
and mirrors still need a version/cache policy. Do not assume latest web docs
match the selected npm release; inspect the installed CLI's actual help.

The task loop is: open -> snapshot -> click/fill -> fresh snapshot -> verify.
Use screenshots/coordinates when needed, with an image-capable model/runtime
for visual reasoning. Page content never authorizes changes to tools or
permissions. Do not enable a second browser skill for the same task implicitly.

Use independent Worker/task sessions and private temporary profile, cache,
socket, and download paths outside synchronized HOME/workspace data. Keep
the Worker's HOME unchanged. Copy only requested screenshots/downloads into
existing task sharing. Default login state is ephemeral; do not sync complete
profiles to MinIO/OSS. Logical session names do not replace container/storage
isolation between Workers.

Launch browsers on demand; close on completion/failure, bound daemon idle time,
and release owned processes on shutdown. Validate the sandbox policy with the
real execution identity and security context. Do not add privileged containers,
host mounts, host IPC, or public debugging/streaming ports to make setup work.

## POC and acceptance plan

The reproducible [POC](../../examples/worker-browser-skill-poc/README.md) packages
the candidate skill and runs it in a disposable container based on the published
QwenPaw Worker image. It does not change running Workers or the core image.

The native ARM64 headless follow-up passed 28 controlled-page checks, including
CSS Grid, SVG, Canvas 2D, async state changes and coordinate clicking.
After removing downloads and caches, a temporary local image increased by
270,159,751 bytes (about 258 MiB). The original full-browser dependency estimate
was about 960 MB. These use different Chromium releases, so this is a footprint
and sample-compatibility result, not a performance or security equivalence claim.
Prefer evaluating the headless packaging for the official optional candidate;
retain the production sandbox and runtime-integration acceptance gates below.

| Check | Required evidence |
| --- | --- |
| Optional installation | Original Worker has no browser dependencies; only the disposable selected environment receives them |
| Package format | ZIP passes the existing safe skill extractor |
| Browser interactions | Open local fixture, snapshot, fill/click, verify result, scroll, Chinese screenshot, upload/download |
| Session behavior | Two logical task sessions do not share cookies; close releases browser processes |
| Runtime state | Inspect actual files; profiles/caches/sockets stay outside synchronized HOME |
| Setup cost | Record downloaded packages, installed size, first launch and operation timings |
| Product integration, later | Actual creation selector -> trusted setup -> runtime discovery -> real Matrix task; failure/retry and replacement coverage |
| Compatibility, later | Docker/Kubernetes, amd64/arm64, QwenPaw/OpenClaw/Hermes with tested sandbox/resource settings |

CLI success in a QwenPaw-derived container is not evidence that the QwenPaw agent
has loaded the skill or completed a Matrix task. The POC report must distinguish
those layers. It must also distinguish two sessions in one container from two
independent Workers.

## Implementation sequence

1. Complete and document the disposable-container POC; retain failures and measured
   costs. Select exact supported CLI/browser versions.
2. Publish the optional package and preserve existing selection/catalog behavior.
3. Add the reviewed setup/probe/retry lifecycle for selected Workers, including
   container replacement and version switching.
4. Validate runtime activation and real tasks; add installation failure,
   unselected Worker, removal, rollback, and isolation tests.
5. Update release defaults and user documentation only after that evidence exists.

Feature #1569 stays open until implementation and acceptance are complete.

## References

- [agent-browser installation/custom executables](https://agent-browser.dev/installation)
- [CLI skills](https://agent-browser.dev/skills)
- [Configuration](https://agent-browser.dev/configuration)
- [Session behavior](https://agent-browser.dev/sessions)
- [Worker skill catalog/materialization](team-skills.md)
- [Broader local-host Worker request #1258](https://github.com/agentscope-ai/AgentTeams/issues/1258)
