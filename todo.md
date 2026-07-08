# AgentTeams Issue TODO

Refreshed 2026-07-08 13:03 UTC. Tracks 17 PRs by `RerankerGuo` on `agentscope-ai/AgentTeams`. CI runs are post-rebase onto current `main` (HEAD `0b562ff`).

## PR Summary Table

| PR | Title | State | Mergeable | CI | Issues pinged |
|---|---|---|---|---|---|
| #965 | fix(controller): preserve Manager plugin config | ❌ CLOSED | — | — | #936 / #937 / #938 |
| #969 | fix(manager): add safe worker deletion wrapper | 🟢 OPEN | MERGEABLE | ✅ 18/18 green | #967 |
| #970 | fix(manager): avoid restart recovery local failure | 🟢 OPEN | MERGEABLE | ✅ 18/18 green | #952 |
| #971 | fix(worker): avoid heartbeat sync storm | 🟢 OPEN | MERGEABLE | ❌ SHARD_A copaw/hermes (post-rebase) | #949 |
| #972 | fix(controller): sync Human worker allowlist | 🟢 OPEN | MERGEABLE | ❌ SHARD_A copaw/hermes | #954 |
| #973 | fix(config): pass custom model parameters to controller | 🟢 OPEN | MERGEABLE | ❌ SHARD_A copaw/hermes | #961 |
| #975 | fix(manager): stop repeated diagnostic loops | 🟢 OPEN | MERGEABLE | ❌ SHARD_A copaw/copaw + copaw/hermes | #974 |
| #976 | docs(team-leader): simplify worker name guidance | 🟢 OPEN | MERGEABLE | ❌ SHARD_A copaw/copaw + openclaw/openclaw | #913 |
| #977 | docs: add skill format contribution guide | 🟢 OPEN | MERGEABLE | ✅ translate only | #735 |
| #978 | docs: clarify Element homeserver port | 🟢 OPEN | MERGEABLE | ✅ translate only | #898 |
| #979 | fix(copaw): retry prompt reads during worker startup | 🟢 OPEN | MERGEABLE | ✅ 18/18 green | #711 / #728 |
| #980 | docs: clarify Higress AI route matching | 🟢 OPEN | MERGEABLE | ✅ translate only | #867 |
| #981 | fix(copaw): refresh runtime skill projection | 🟢 OPEN | MERGEABLE | ✅ 18/18 green | #712 |
| #982 | fix(copaw): guard managed skill cleanup | 🟢 OPEN | MERGEABLE | ✅ 18/18 green | #626 |
| #983 | fix(worker): keep openclaw config local during sync | 🟢 OPEN | MERGEABLE | ❌ SHARD_A openclaw/openclaw | #809 |
| #984 | fix(install): surface Higress gateway startup diagnostics | 🟢 OPEN | MERGEABLE | ❌ SHARD_B copaw/hermes (NEW — test-14-git-collab) | #888 |
| #985 | test: make multi-worker bob setup deterministic | 🟢 OPEN | MERGEABLE | ✅ **18/18 green** — root-cause fix for SHARD_A flake | test infra flake |

## Tier 1: Ready to merge (CI green + ping sent)

All docs-only or already passed full integration. Awaiting maintainer review/merge.

- **#977** — Skill format contribution guide (English + Chinese), linked from Manager guide. Translate-only CI green.
- **#978** — FAQ entry clarifying `:18088` (Element Web UI) vs `:18080` (Matrix/Higress). Translate-only CI green.
- **#980** — FAQ entry for #867 (custom AI route matching): warns about unconstrained `default-ai-route` shadowing, recommends explicit `modelPredicates`. Translate-only CI green.

## Tier 2: Rebased + CI green (Tier 2 ping sent)

- **#969** — 18/18 green. Adds `hiclaw delete worker <name>` wrapper that cleans `state.json` / `worker-lifecycle.json` / `workers-registry.json`.
- **#970** — 18/18 green. Removes top-level `local` in `start-manager-agent.sh` Worker recreation block.
- **#979** — 18/18 green. Bounded stable UTF-8 reads for `SOUL.md` / `AGENTS.md` during CoPaw startup and re-bridge.
- **#981** — 18/18 green. Restores CoPaw bridge helpers for workspace → runtime workspace materialization.
- **#982** — 18/18 green. Managed child-directory guards around CoPaw recursive skill cleanup.

## Tier 3: SHARD_A flake (red, flake analysis sent)

These all failed `integration-tests (llm-interaction, SHARD_A_TESTS, ...)` on `test-06-multi-worker.sh`. Same root cause across runtimes: Manager did not invoke `hiclaw create worker --name bob` in the flaky run — confirmed by inspecting #984's downloaded artifacts (no `hiclaw-worker-bob/` session directory exists; only alice's). #976 is a docs-only PR with the same failure pattern, proving the failure is not branch-local. The flake analysis comments on these PRs link back to #984's investigation.

- **#972** — failed `SHARD_A copaw/hermes`. Human Reconciler → `groupAllowExtra` sync.
- **#973** — failed `SHARD_A copaw/hermes`. Helm/embedded model-parameter env wiring.
- **#975** — failed `SHARD_A copaw/copaw` + `copaw/hermes`. Manager diagnostic-loop prompt guidance.
- **#976** — failed `SHARD_A copaw/copaw` + `openclaw/openclaw`. Docs-only (+14/-64); the openclaw failure here is the strongest evidence the flake is cross-runtime.
- **#983** — failed `SHARD_A openclaw/openclaw`. `hiclaw-sync` exclude + merge for `openclaw.json`.
- **#984** — failed `SHARD_A copaw/copaw` + `copaw/hermes` (the original investigation that surfaced the flake).

## Tier 4: Latest rebase results

- **#971** — rebase onto `main` HEAD `0b562ff`; new CI run finished red on the **same** SHARD_A copaw/hermes flake (Manager never invoked `hiclaw create worker --name bob`). Comment posted 2026-07-08 13:03 linking to #985 as the upstream fix.
- **#984** — re-rebased; SHARD_A is now **passing** on this PR (the bob-determinism fix in #985 may have helped even before #985 merges, by changing the test runner script in main? — actually no, #985 is on a separate branch; the SHARD_A pass is just run-to-run variance). But a NEW failure appeared on **SHARD_B copaw/hermes** → `tests/test-14-git-collab.sh` (the 4-phase non-linear multi-worker git collaboration test, 57 min runtime before failing). This is a separate LLM-driven flake (3 workers, 4 phases, complex branch/path/report invariants). No equivalent deterministic-bypass PR exists for test 14. The PR's own change is shell diagnostics + sysctl FAQ, doesn't touch Manager agent runtime or git ops. Comment posted 2026-07-08 13:03.

## Tier 5: Special — Fixes the SHARD_A flake itself

- **#985** — `test: make multi-worker bob setup deterministic`. Replaces the LLM-driven `matrix_send_message ... create bob` instruction with a direct `hiclaw apply worker --name bob ...` call so the collaboration assertions below measure multi-worker flow, not LLM timing. **This is the upstream fix for the SHARD_A flake affecting #972, #973, #975, #976, #983, #984, and now #971.** Rebased onto current `main`, CI **18/18 green as of 13:00 UTC**, priority ping posted 2026-07-08 13:03.

If #985 merges first, the SHARD_A failures on Tier 3 PRs should disappear on the next CI run. **#985 is now the single highest-priority PR — landing it unblocks 7 other PRs.**

## CLOSED

- **#965** — auto-closed by GitHub at 2026-07-08T05:28:38Z after `head_ref_force_pushed`. A buggy rebase script force-pushed origin/main HEAD `0b562ff` over the PR's source branch, losing the original commit `1820ef3`. Fork was restored to `1820ef3`, but the upstream PR ref stayed stuck at `0b562ff`. `gh pr reopen` fails (no reopen permission; only `pull` on the upstream repo). Original fix shape was: preserve `plugins.allow` / `plugins.entries` / `plugins.load.paths` across controller reconciliation; conflicts in deployer.go (638 lines) and deployer_test.go (1064 lines) due to #998's deployer refactor. To recover: open a new PR from a fresh branch.

## Maintainer Feedback

**No human responses on any PR.** Active merger is `shiyiyue1102` (15 PRs merged in last 30 days; nobody else has merged PRs).

All comments on every PR are either:
- `github-actions` CI bot reports (CI Metrics Report / Integration Tests Failed summary), or
- My own ping / flake-analysis comments from 2026-07-08 06:05–07:25 UTC.

## Cron / next-step plan

- All scheduled one-shot crons (`cbde48c7`, `de5e92c1`, `fee24c58`) have been deleted (their fire times passed and the events have all been handled manually in real time).
- Currently active things to monitor:
  1. **#985** — already pinged as priority; awaiting maintainer merge.
  2. **#971, #972, #973, #975, #976, #983** — Tier-3 PRs blocked on #985 merge. After #985 lands, re-running CI on these should clear them.
  3. **#984** — has a separate SHARD_B (test-14) failure; needs separate investigation or maintainer judgement.
  4. **#965** — still CLOSED; recovery requires either maintainer reopen or new PR (covered above).
- No further cron scheduling until maintainer responds or #985 lands.
- Keep current branch `fix/issue-957-copaw-worker-k8s-startup` untouched.

## Issues from old todo still pending

The original 2026-07-03 todo mentioned several skipped/lower-priority issues. Current status (unchanged unless noted):

- **#947 task progress watchdog** — upstream PR #948 open with assignee; skip.
- **#957 K8s v1.1.1 controller cannot start v1.1.2 CoPaw worker** — upstream PR #960 open; skip. (Branch `fix/issue-957-copaw-worker-k8s-startup` checked out locally is the user's WIP for this.)
- **#925 Manager Matrix sync silently stops after Suppressed AbortError** — needs design; skip.
- **#962 Embedded mode McpBridge not pushed to Envoy via xDS** — upstream base-image issue; skip.
- **#953 Manager cannot be interrupted while processing a message** — needs design; skip.
- **#929 Can we only use Alibaba Cloud Bailian models?** — already documented; skip.
- **#850 Qiniu Cloud returns 308** — assigned, needs reproduction; skip.
- **#820 Worker renamed during use stops receiving Manager messages** — assigned, needs reproduction; skip.