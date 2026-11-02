# agentteams-matrix — Hermes Adapter

This adapter projects the bundle contract (`../../contract.yaml`) onto the
Hermes runtime by merging its `matrix:` section into the `platforms.matrix`
block of the Hermes config file, nested under the block's explicit `extra:`
map (see [Config shape and runtime capabilities](#config-shape-and-runtime-capabilities)).
It is a pure file projector: it reads the contract, rewrites one config
file, and performs no network access. The only runtime interactions are a
best-effort read-only probe of the Hermes install (its Matrix adapter
source) and an optional `hermes --version` probe — both used to resolve
the target runtime's capability, never to modify anything.

## Config shape and runtime capabilities

The supported Hermes generations parse `platforms.matrix` differently,
and the installer must target both. The gate follows **verified
capabilities, not tag strings**:

| Capability | Verified runtime | `enabled` / `token` | `extra.homeserver` / `extra.user_id` | `extra.allowed_users` / `extra.allowed_rooms` / `extra.require_mention` |
|:--|:--|:--|:--|:--|
| **basic** | tag v2026.4.16 (the Worker pin, package 0.10.0) | typed fields, consumed | consumed: `gateway/platforms/matrix.py:207,212` (also `:216,224` for `password` / `device_id`) | **not available** — the generation has no allowlist feature and `require_mention` is env-only (`MATRIX_REQUIRE_MENTION`) |
| **full** | tag v2026.9.24 (package 0.21.5) | typed fields, consumed | consumed: `plugins/platforms/matrix/adapter.py:842,844` | consumed: `adapter.py:873` (`allowed_rooms`), `adapter.py:908` (`allowed_users`), `adapter.py:950` (`require_mention`) |
| **full** | tag v0.22.5 (package 0.0.0, bf0203a7) | typed fields, consumed | consumed: `plugins/platforms/matrix/adapter.py:829,831` | consumed: `adapter.py:863` (`allowed_rooms`), `adapter.py:898` (`allowed_users`), `adapter.py:940` (`require_mention`) |

Tag strings and package versions are **not comparable across
generations**: the calendar tag v2026.9.24 is a newer generation than
the semver-shaped tag v0.22.5, which declares package version 0.0.0.

Consequences:

- **The full contract requires a full-capability runtime** (the
  allowlists and `require_mention` are part of it). Verified full tags:
  v2026.9.24 (package 0.21.5) and v0.22.5.
- **The current Worker pin is a basic-capability runtime**
  (v2026.4.16, package 0.10.0). Bumping the pinned runtime is tracked
  separately; until then, deploying the full contract on it requires
  `--allow-partial` (see below).
- **Settings are written only as `extra.<key>`, never as flat block keys.**
  The basic generation's `PlatformConfig.from_dict` reads
  `extra = data.get("extra", {})` and ignores every other non-typed block
  key — flat settings are silently dropped (the #1458 defect). The full
  generation promotes flat non-typed keys into `extra`, but an explicit
  `extra:` map wins on a clash (`gateway/config.py:448-452`), so flat
  keys would add nothing and create a contradiction risk. One nested
  shape therefore serves both generations.
- **Capability resolution order** (first decisive source wins):
  1. **tree probe** — a located Hermes root (`--hermes-root`, then
     `HERMES_HOME`, then an importable `gateway` package, then the
     `hermes` binary's prefix) whose Matrix adapter source detects as
     full or basic. Read-only ground truth; it outranks everything
     below. A root without a detectable Matrix shape is not a successful
     probe — the installer never guesses.
  2. **`--hermes-capabilities full|basic`** — an explicit declaration;
     outranks the version table.
  3. **verified table** — the version from `--hermes-version` /
     `HERMES_VERSION` / `hermes --version`, keyed by tag or by package
     version: v2026.4.16 / 0.10.0 → basic; v2026.9.24 / 0.21.5 → full;
     v0.22.5 / 0.0.0 → full.
  4. **unknown** — fails closed with guidance (names the settings,
     offers `--hermes-capabilities` / `--hermes-root` /
     `--allow-partial`); never a silent downgrade.
- **Strict/partial gate.** When the resolved runtime is
  basic-capability (or unknown) and the contract requires
  `allowed_users`, `allowed_rooms` or `require_mention`, the installer
  exits non-zero naming exactly those settings — it never exits 0 while
  dropping contract settings. With `--allow-partial` it projects the
  settings the runtime can (or is verified to be able to) honor, prints
  a warning naming the dropped ones, and records them in
  `<config dir>/.agentteams-matrix/hermes-install.json`
  (`{"adapter", "bundle", "capabilities", "capability_source", "config",
  "dropped", "runtime_version"}`). A later full projection removes the
  stale marker.

Key names and the target block follow the upstream Hermes gateway config
(lines cite tag v0.22.5, commit bf0203a7; the tag v2026.9.24 adapter
reads the same keys at `adapter.py:842,844` / `:873` / `:908` / `:950`):

- The `platforms:` top-level key of `~/.hermes/config.yaml` is parsed by
  `GatewayConfig.from_dict`
  (`gateway/config.py:781` → `by_platform("platforms",
  PlatformConfig.from_dict, dicts_only=True)`).
- `PlatformConfig.from_dict` takes the typed fields (`enabled`, `token`,
  ...) and everything else from the block's `extra:` map
  (`gateway/config.py:439-452`); on the full generation the promotion of
  flat non-typed keys means a block written with flat keys would *still*
  be consumed there — but not on the basic runtime.
- The matrix adapter reads its settings extra-first:
  `config.extra.get(key)` → `MATRIX_*` env → default
  (`gateway/platforms/_shared.py:106`, `plugins/platforms/matrix/adapter.py`).
- The AgentTeams worker bridge uses the same split: credentials flow through
  `MATRIX_*` env, `platforms.matrix.enabled` is set in `config.yaml`
  (`hermes/src/hermes_worker/bridge.py`, `bridge_openclaw_to_hermes`).

## Key mapping

| Contract field (`matrix:`) | Hermes key (`platforms.matrix.`) | How it is consumed (file:line) |
|:--|:--|:--|
| — (constant `true`) | `enabled` | typed `PlatformConfig` field: `gateway/config.py:410,467` |
| `homeserver` | `extra.homeserver` | read at `plugins/platforms/matrix/adapter.py:829` (fallback env `MATRIX_HOMESERVER`); on the pinned runtime at `gateway/platforms/matrix.py:207` |
| `user_id` | `extra.user_id` | read at `adapter.py:831` (fallback env `MATRIX_USER_ID`); on the pinned runtime at `gateway/platforms/matrix.py:212` |
| `allow_from[]` + `group_allow_from[]` | `extra.allowed_users` (union, deduped, contract order) | the adapter has a **single** user allowlist for DMs and groups (no DM/group distinction): `adapter.py:898`, applied at `adapter.py:2611-2612` (fallback env `MATRIX_ALLOWED_USERS`); YAML lists are accepted by `_csv_set` (`adapter.py:546-548`). **full-capability runtimes only** (verified: v2026.9.24, v0.22.5) |
| `rooms[]` | `extra.allowed_rooms` | room-ID gate, DMs exempt: `adapter.py:863`, `adapter.py:1973-1981` (fallback env `MATRIX_ALLOWED_ROOMS`). **full-capability runtimes only** (verified: v2026.9.24, v0.22.5) |
| `require_mention` | `extra.require_mention` | read at `adapter.py:940` (fallback env `MATRIX_REQUIRE_MENTION`, default `true`). **full-capability runtimes only** (verified: v2026.9.24, v0.22.5) |
| — (`--with-credentials`) | `token` | typed `PlatformConfig` field (`gateway/config.py:411,468`, read at `adapter.py:830`) or the `MATRIX_ACCESS_TOKEN` env var |
| `access_token_ref` | — (not projected) | no secret-reference concept: credentials are the typed `token` field or the `MATRIX_ACCESS_TOKEN` env var |
| `history_limit` | — (not projected) | no equivalent key in the matrix adapter (zero matches) |
| `history_mode` | — (not projected) | no equivalent key in the matrix adapter (zero matches) |

## Install

```bash
plugins/agentteams-matrix/scripts/install.sh --adapter hermes
# or directly:
adapters/hermes/install.sh [--config <path>] [--dry-run] [--with-credentials]
                  [--hermes-version <version>] [--hermes-root <path>]
                  [--hermes-capabilities full|basic] [--allow-partial]
```

The default config path is `$HOME/.hermes/config.yaml` (override with
`--config`; `~/.hermes` is the platform default home,
`hermes_constants.py:51-56`). `--dry-run` prints the resulting YAML to
stdout and writes nothing.

Capability resolution follows the order in [Config shape and runtime
capabilities](#config-shape-and-runtime-capabilities):

- `--hermes-root <path>` points the read-only tree probe at an explicit
  install root (overrides `HERMES_HOME`); the directory must exist.
- `--hermes-capabilities full|basic` states what the target runtime
  supports. It outranks the verified version table; a successful tree
  probe outranks it.
- `--hermes-version <v>` declares the target runtime version as a tag
  (e.g. `v2026.9.24`) or a package version (e.g. `0.21.5`). It
  overrides `HERMES_VERSION` and the `hermes --version` probe and is
  consulted only when the tree probe found no root and
  `--hermes-capabilities` was not passed.
- An unknown runtime (no decisive source) fails the strict gate closed
  with guidance instead of silently dropping contract settings.
  `--allow-partial` relaxes that gate to a warning plus the
  partial-projection marker.

The installer merges only the keys it owns into the `platforms.matrix`
block and preserves every other key in the file (`terminal`, `memory`,
other platforms, ...), including foreign keys already present inside the
block (e.g. `reply_to_mode`) or inside its existing `extra` map.
Re-running with an unchanged contract produces a byte-identical file.

## Credentials

By default the installer writes **no credential material at all**: the
contract's `access_token_ref` has no Hermes key (see the table above),
and nothing is printed to stdout. With `--with-credentials` it writes the
typed `token` field from the `AGENTTEAMS_MATRIX_ACCESS_TOKEN` environment
variable (hard error if the variable is unset). The config file is
written at mode 0600 whenever the rendered document carries a credential
value — written now or already present in the file — and the atomic
rewrite never loosens an existing mode, so a config that still holds a
credential after an uninstall stays 0600.

`--dry-run` previews redact credential values in the rendered document:
any non-empty string under a key ending in `token`, `secret`, `password`,
`passwd`, `apikey` or `credentials` (case-insensitive) is printed as
`<redacted>` — including credentials in sibling blocks this adapter does
not own. The config file itself is always written unredacted.

Where the env path is the established credential channel (as in the
worker bridge), the env var `MATRIX_ACCESS_TOKEN` wins over the YAML
token (`adapter.py:830`).

## Uninstall

```bash
adapters/hermes/uninstall.sh [--config <path>] [--dry-run]
```

Removes exactly what the installer writes, in **both** shapes it has
produced:

- typed top-level keys `enabled`, `token`, and the nested `extra.*` keys
  `homeserver`, `user_id`, `allowed_users`, `allowed_rooms`,
  `require_mention` (the current shape);
- the legacy flat top-level keys `homeserver`, `user_id`,
  `allowed_users`, `allowed_rooms`, `require_mention` written by
  pre-#1458 installers.

All other keys in the block (e.g. `reply_to_mode`, foreign `extra.*`
keys) and the rest of the file are preserved. When the `extra` map ends
up empty it is dropped; when the block ends up empty it is dropped, and
the `platforms` key only if it is empty as well. A partial-projection
marker left by the installer
(`<config dir>/.agentteams-matrix/hermes-install.json`) is removed as
well, including its directory once empty. The config file itself is
never deleted. Without anything to remove the uninstall is a no-op, so
re-running is safe.

## Requirements

bash, python3 with pyyaml (to read the contract and the config). No
required runtime binary (the tree probe and the `hermes --version` probe
are optional and read-only; neither is required for a declared
capability), no network.
