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

The base Worker image must remain unchanged. On the first browser task, the
Worker follows the skill and calls its headless entry point; that entry point
installs dependencies if needed and continues the requested command. No normal
Human installation step or Controller installation hook is required.
Dependency loss when a container is deleted is acceptable: install again on
the replacement Worker's first browser call.

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

The existing ZIP/assignment flow distributes files and does not run arbitrary
install scripts. Keep that behavior. After skill activation, the Worker invokes
the skill's entry point using its existing shell tool. The updated candidate
performs lazy setup there; assignment alone does not download a browser.

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
4. Refresh/load the selected skill through the existing runtime adapter. Its
   instructions direct the Worker to use scripts/headless.sh for browser work.
5. On the first call, the entry point takes an installation lock, checks the
   pinned-version readiness marker and runtime files, installs missing
   dependencies, then runs the original command. Setup logs go to stderr so
   --json output remains parseable. Concurrent calls share one installation.
6. Subsequent calls check readiness and skip downloads. If container-local
   dependencies disappear, repeat setup on the next call. Do not persist the
   browser environment just to survive container deletion.

Only selected Workers pay the installation cost. Upgrading skill text alone
does not replace an already running CLI/browser pair: install/probe the selected
version before switching, close old sessions while idle, then activate. If setup
fails, retain the previous working version and report the error. Removing a
skill prevents subsequent use and closes its sessions; do not automatically
purge shared system libraries used by another skill.

For unsupported architectures, unavailable network or insufficient container
permissions, report the concrete blocker; the normal flow must not ask a Human
to preinstall packages. Preserve existing tool authorization. Do not grant
privilege, install on the host, or enlarge the base image as an automatic
fallback. The current headless candidate is validated only on root ARM64 Docker.

## Skill contract and browser execution

The optional package contains SKILL.md plus explicit installation and execution
scripts. The discovery instructions address the Worker as "you", name supported
runtimes/OS/architectures, and explain actual setup cost and restrictions.

The candidate uses pinned agent-browser plus local Chromium. Native ARM64 needs
a compatible browser source: the current Chrome for Testing installer does not
have a Linux ARM64 target. The candidate uses the pinned headless-shell pack;
the full Debian Chromium installer remains a comparison fixture. Production
packages and mirrors still need a version policy. Do not assume latest web docs
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
| Product integration, later | Actual creation selector -> runtime discovery -> skill first-use setup -> real Matrix task; failure/retry coverage |
| Compatibility, later | Docker/Kubernetes, amd64/arm64, QwenPaw/OpenClaw/Hermes with tested sandbox/resource settings |

CLI success in a QwenPaw-derived container is not evidence that the QwenPaw agent
has loaded the skill or completed a Matrix task. The POC report must distinguish
those layers. It must also distinguish two sessions in one container from two
independent Workers.

## Implementation sequence

1. Complete and document the disposable-container POC; retain failures and measured
   costs. Select exact supported CLI/browser versions.
2. Publish the optional package and preserve existing selection/catalog behavior.
3. Validate the skill-owned first-use setup, readiness checks and retry behavior;
   container replacement simply repeats first-use setup.
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
