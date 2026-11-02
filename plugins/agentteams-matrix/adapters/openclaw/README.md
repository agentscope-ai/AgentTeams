# agentteams-matrix — OpenClaw Adapter

This adapter projects the bundle contract (`../../contract.yaml`) onto the
OpenClaw runtime by merging its `matrix:` section into `channels.matrix` of
the OpenClaw config file. It is a pure file projector: it reads the
contract, rewrites one config file, and nothing else — no `openclaw`
binary is invoked and no network access is performed.

Key names and shapes follow the OpenClaw matrix extension's channel
config schema (upstream OpenClaw v2026.10.6, commit fe06181d). The
authoritative schema is `extensions/matrix/src/config-schema.ts`
(`MatrixConfigSchema`); per-key evidence is in the table below and the
same shape is what the AgentTeams controller generates for OpenClaw
workers (`buildMatrixChannelConfig` in
`agentteams-controller/internal/agentconfig/generator.go`).

## Key mapping

| Contract field (`matrix:`) | OpenClaw key (`channels.matrix.`) | Upstream evidence (file:line) |
|:--|:--|:--|
| — (constant `true`) | `enabled` | `config-schema.ts:103`; docs `setup.md:47`; channel presence = provider configured: `monitor/index.ts:238` |
| `homeserver` | `homeserver` | `config-schema.ts:127`; docs `setup.md:48` |
| `user_id` | `userId` | `config-schema.ts:130`; docs `setup.md:64` |
| `allow_from[]` | `dm.allowFrom` (+ `dm.policy: "allowlist"`) | dm block shape `{enabled, policy, allowFrom}`: `src/channels/plugins/config-schema.ts:89-96`; policy values `pairing\|allowlist\|open\|disabled`: `src/config/zod-schema.core.ts:436`; runtime default policy is `pairing` (`monitor/index.ts:264`), so the explicit `allowlist` is required for `allowFrom` to be a hard gate; docs `access-control.md:62-63` |
| `group_allow_from[]` | `groupAllowFrom` (+ `groupPolicy: "allowlist"`) | `config-schema.ts:142,163`; values: `src/config/zod-schema.core.ts:434`; fail-closed default once `channels.matrix` exists: `src/config/zod-schema.channel-messaging-common.ts:65`, `monitor/index.ts:236`; docs `access-control.md:66-67` |
| `rooms[]` | `autoJoin: "allowlist"` + `autoJoinAllowlist` + one `groups.<room>` entry per room | a room list has no top-level key: `groups`/`rooms` are room→entry maps (`config-schema.ts:169-170`; `rooms` is the legacy alias, docs `setup.md:122`); invite gating defaults to `off` (`onboarding.ts:187`) and `autoJoinAllowlist` accepts literal room IDs (`config-schema.ts:161-162`, `target-ids.ts:52-61`); under `groupPolicy: "allowlist"` a room is only admitted when it has a `groups` entry (`monitor/rooms.ts:39-52`, `monitor/handler-ingress-access.ts:160-170`) |
| `require_mention` | `groups.<room>.requireMention` (one per contract room) | `requireMention` is a per-room group-entry field: `src/channels/plugins/config-schema.ts:62`; there is no top-level `requireMention` in `MatrixConfigSchema`; docs `access-control.md:69`, `setup.md:91` |
| `history_limit` | `historyLimit` | `config-schema.ts:160`; runtime `monitor/index.ts:267` |
| `access_token_ref` | — (not projected) | no such key; `accessToken` accepts a plain string or a SecretRef whose env/store ids must match `^[A-Z][A-Z0-9_]{0,127}$` (`src/config/zod-schema.secret-input.ts:22-55`), which the ref `agentteams/matrix/access-token` cannot express |
| `history_mode` | — (not projected) | no such key in the matrix extension (zero matches) |

## Install

```bash
plugins/agentteams-matrix/scripts/install.sh --adapter openclaw
# or directly:
adapters/openclaw/install.sh [--config <path>] [--dry-run] [--with-credentials]
```

The default config path is `$HOME/.openclaw/openclaw.json` (override with
`--config`). `--dry-run` prints the resulting JSON to stdout and writes
nothing.

The installer merges only the keys it owns into `channels.matrix` and
preserves every other key in the file (other channels, the controller's
`streaming`/`network` settings, ...). The `dm` and `groups` blocks are
deep-merged: foreign sub-keys of a pre-existing `dm` block or of a
pre-existing per-room `groups` entry survive, and `groups` entries for
rooms outside the contract are left untouched. Re-running with an
unchanged contract produces a byte-identical file.

## Credentials

By default the installer writes **no credential material at all**: the
contract's `access_token_ref` has no OpenClaw key (see the table above),
and nothing is printed to stdout. With `--with-credentials` it writes
`accessToken` (plain string, `config-schema.ts:131`) from the
`AGENTTEAMS_MATRIX_ACCESS_TOKEN` environment variable (hard error if the
variable is unset). The config file is written at mode 0600 whenever the
rendered document carries a credential value — written now or already
present in the file — and the atomic rewrite never loosens an existing
mode, so a config that still holds a credential after an uninstall stays
0600.

`--dry-run` previews redact credential values in the rendered document:
any non-empty string under a key ending in `token`, `secret`, `password`,
`passwd`, `apikey` or `credentials` (case-insensitive) is printed as
`<redacted>` — including credentials in sibling blocks this adapter does
not own. The config file itself is always written unredacted.

## Uninstall

```bash
adapters/openclaw/uninstall.sh [--config <path>] [--dry-run]
```

Contract-driven, exactly inverse of the installer: it re-derives the
removal set from the bundle contract (same conditions as install) and
removes only those keys from `channels.matrix` — flat owned keys,
`dm.policy`/`dm.allowFrom`, and `groups.<room>.requireMention` per
contract room — keeping every foreign key and foreign `dm`/`groups`
sub-key, and never deleting the config file. Entries (and the `groups`
or `dm` blocks) that empty out are dropped; when the `matrix` block ends
up empty it is dropped, and the `channels` key only if it is empty as
well. Without anything to remove the uninstall is a no-op, so re-running
is safe. If the contract changed between install and uninstall, only the
new contract's keys are removed.

## Requirements

bash, python3 with pyyaml (to read the contract). No runtime binary, no
network.
