# OpenClaw Upgrade Plan

AgentTeams still ships OpenClaw `2026.4.14` (`higress-group/openclaw@2f35b6fa6b65d012e3b0c9f24af3f8a4b617a6e0`). Upstream OpenClaw is at `2026.10.6` (2026-10-22). This note is the upgrade path after the config hardening that replaced `tools.elevated.allowFrom.matrix=["*"]`. It does **not** change the image pin.

## Why the pin stays

- `higress-group/openclaw` `main` is still around `2026.2.6-3`. Pointing `OPENCLAW_REPO_URL` at fork `main` would move **backward**.
- AgentTeams depends on 2026.4.x contracts that later releases may change: gateway port `18799`, `gateway.bind=lan`, Matrix `autoJoin=always`, `dangerouslyAllowPrivateNetwork`, Control UI on the same listener, Matrix plugin path `/opt/openclaw/extensions/matrix`.
- A pin bump rebuilds `openclaw-base` and every downstream Manager/Worker image. Compatibility must be proven on a candidate commit first.

## Target versions

| Goal | OpenClaw version | What it covers |
|------|-----------------|----------------|
| Minimum security floor | `2026.8.1` | 2026-09-11 GHSA batch, including Matrix user-id case-fold (`GHSA-hgv5-f2r3-6v9r`) and exec/workspace escapes |
| Matrix display-name allowlist | `2026.5.7` (already included in 2026.8.1) | CVE-2026-53811 / `GHSA-7hxm-f538-3xp6` |
| Browser CDP SSRF | `2026.6.6` (already included in 2026.8.1) | CVE-2026-62197 / `GHSA-3x84-qq85-fj65` |
| Preferred current line | `2026.10.6` | October Node certificate-chain requirement (`Node >= 24.22.1` or `>= 26.12.1`) plus later engine fixes |

Recommended first candidate: **`openclaw/openclaw` tag `v2026.8.1`** (or the equivalent commit). Second candidate after that tag boots: **`v2026.10.6`**.

Do not pin `higress-group/openclaw` `main`. Either cherry-pick official tags onto the fork, or switch `OPENCLAW_REPO_URL` to `https://github.com/openclaw/openclaw.git` for the candidate build only.

## Node.js

`openclaw-base/Dockerfile` currently runs `setup_24.x` and installs whatever Node 24 the NodeSource repo serves. OpenClaw `2026.10.6` requires **Node 24.22.1+** (or 26.12.1+) for the 2026-10 stolen-certificate patch.

When the pin moves to 2026.10.6:

1. Install a **pinned** Node 24.22.1+ package (or checksum-pinned tarball), not floating `setup_24.x`.
2. Rebuild published `openclaw-base` / Manager / Worker tags. Existing images keep the old Node even if the Dockerfile later changes.

A 2026.8.1 candidate can stay on current Node 24 for the first compatibility pass.

## Compatibility checklist

Run against a locally built `openclaw-base` that uses the candidate commit (`OPENCLAW_BASE_IMAGE=agentteams/openclaw-base OPENCLAW_BASE_VERSION=latest`):

1. **Gateway listen contract.** Control UI and health probes still reach container port `18799` with `gateway.bind=lan`.
2. **Matrix boot.** Worker/Manager join the provisioned room (`autoJoin=always`) and `/sync` against the private homeserver (`dangerouslyAllowPrivateNetwork=true`).
3. **Allowlists.** `channels.matrix.dm.allowFrom` / `groupAllowFrom` still use immutable Matrix user IDs (`@user:domain`). Confirm the new engine no longer matches display names (CVE-2026-53811) and no longer case-folds the server-name (GHSA-hgv5-f2r3-6v9r).
4. **Elevated exec.** `tools.elevated.allowFrom.matrix` is an ID list (admin, plus manager or team leader). Confirm `["*"]` is no longer required for YOLO host exec.
5. **Control UI.** Higress still fronts the console over HTTP with `dangerouslyDisableDeviceAuth` + `allowInsecureAuth`. Do not drop those flags in this upgrade; they are how the local installer reaches the console.
6. **Schema drift.** Diff `openclaw.json` keys AgentTeams writes (`tools.exec`, `elevatedDefault`, `plugins.load.paths`, `session.resetByType`) against the candidate schema. Fix templates before shipping.
7. **Tests.** `tests/test-01-manager-boot.sh`, `tests/test-02-create-worker.sh`, and at least one Matrix round-trip (`test-06` or equivalent) on OpenClaw Manager **and** OpenClaw Worker. QwenPaw Manager is out of this blast radius.

## Suggested sequence

1. Resolve the `v2026.8.1` commit SHA on `openclaw/openclaw` (or the cherry-pick onto `higress-group/openclaw`).
2. Build a throwaway `openclaw-base` with that SHA; leave `Makefile` `OPENCLAW_BASE_VERSION` unchanged.
3. Run the checklist above.
4. If 2026.8.1 boots, repeat with `v2026.10.6` **and** pin Node 24.22.1+.
5. Only then change `openclaw-base/Dockerfile` fetch SHA and `Makefile` `OPENCLAW_BASE_VERSION`.

## Already done (config hardening, this branch)

- Manager template: `tools.elevated.allowFrom.matrix` is the admin Matrix ID.
- Worker template / controller generator: Manager+Admin (standalone) or Leader+Admin (team). No `"*"`.
- Existing Manager `openclaw.json`: boot merge rewrites historical `matrix=["*"]` to the admin ID and leaves a custom allowlist alone.
- Existing Worker `openclaw.json`: entrypoint rewrites a leftover `matrix=["*"]` to `groupAllowFrom` (or `dm.allowFrom`). `tools` is local-first, so this must happen on the worker disk.
- Control UI CORS: boot merge fills `allowedOrigins` only when missing, so an operator-tightened list survives restart. Defaults stay `["*"]` because the installer cannot enumerate every console origin.
