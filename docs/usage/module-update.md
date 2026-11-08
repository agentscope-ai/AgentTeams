# Module Update & Distribution (Millforge channels)

> Added 2026-11-08 alongside the module-update endpoints (`#1526`) and the
> first Millforge-hosted release of a worker plugin (`qwenpaw-qwencode-bridge`).

AgentTeams ships some runtime pieces as **modules** (TeamHarness, the Matrix
bundle, the Qwen Code bridge, …) that can be installed and upgraded on a
running deployment **without rebuilding images**. The release artifacts and the
channel index live in a separate repository (the **Millforge** distribution
mirror); the AgentTeams repository stays the source of truth.

This document is the contract for the three install planes, the release index,
and the `channels.json` format.

## Scope & prerequisites

* The controller exposes the endpoints below when it runs with the module-update
  build (`GET /api/v1/millforge/index` + the three install routes). Container
  (docker) deployments support install; **Kubernetes deployments return 503**
  for the install routes.
* Outbound network access to `raw.githubusercontent.com` (GitHub) or
  `raw.gitcode.com` (GitCode) is required — those are the only two hosts the
  controller will fetch the index and artifacts from.
* Working directory of a target QwenPaw worker must be writable by the worker
  process; installed plugins land under `<working-dir>/plugins/<plugin-id>`.

## Why a distribution repository

The upstream AgentTeams repository cannot merge every feature PR inside a
release window, and rebuilding images for each plugin change is slow. Modules
therefore publish to a separate mirror with three channels:

| Channel | Meaning | Who should use it |
|:--|:--|:--|
| `stable` | Reviewed releases built from the upstream tag/mainline | Production deployments |
| `beta` | Release candidates, same artifact shape, may change | Early adopters |
| `alpha` | Pre-releases cut ahead of upstream merges (accepted work that is not merged yet) | Testing new capabilities before they land upstream |

The default Millforge repository is `LUOSENGWA/millforge`, layout:

```text
releases/
├── alpha/channels.json          # one index per channel
├── beta/channels.json
├── stable/channels.json
├── <module>/<version>/          # artifacts for every channel (one copy)
│   ├── <module>-<version>.zip   # or <pluginId>-<version>.zip
│   ├── sha256sums.txt
│   └── release-manifest.json
└── looprunner/                  # reserved runner slot (not shipped yet)
```

An artifact physically lives under `releases/<module>/<version>/`; **the channel
index only decides which versions are visible** to a given channel.

## Release index

### `GET /api/v1/millforge/index`

Fetches and validates a channel index, then returns it with fetch metadata.

| Query | Required | Default | Notes |
|:--|:--|:--|:--|
| `source` | yes | — | `github` or `gitcode` |
| `repo` | no | `LUOSENGWA/millforge` | Must be `org/name` |
| `ref` | no | `main` | Branch, tag, or commit |
| `channel` | no | `beta` | `alpha`, `beta` or `stable` |

Response:

```json
{
  "schema": 1,
  "channel": "stable",
  "modules": {
    "qwenpaw-qwencode-bridge": {
      "kind": "plugin",
      "pluginId": "qwen-code-bridge",
      "latest": "0.3.0",
      "versions": {
        "0.3.0": {
          "artifacts": [
            {"name": "qwenpaw-qwencode-bridge-0.3.0.zip",
             "sha256": "01efceee92716c9835e3b958337f427f1a6bea22f1a778d39206dc14ce7b4fa0"}
          ],
          "requires": {"qwenpaw": ">=2.2.0 <2.3.0", "qwen-code-cli": "0.26.x"},
          "commit": "c31dc45df",
          "published_at": "2026-11-08T01:30:00Z",
          "deprecated": false
        }
      }
    }
  },
  "source": "github",
  "repo": "LUOSENGWA/millforge",
  "fetched_at": "2026-11-08T01:30:12Z"
}
```

Fetch rules enforced by the controller: only the two allowlisted raw hosts,
**no cross-host redirects**, 10 s timeout, 1 MiB response cap. Upstream
`404` maps to `404`, any other upstream failure to `502`.

### `channels.json` contract

```json
{
  "schema": 1,
  "channel": "stable",
  "updated_at": "2026-11-08T01:30:00Z",
  "modules": {
    "<module>": {
      "kind": "plugin",
      "pluginId": "…",
      "latest": "x.y.z",
      "versions": {
        "x.y.z": {
          "artifacts": [{"name": "…", "sha256": "…"}],
          "requires": {"…": "…"},
          "commit": "…",
          "published_at": "…",
          "deprecated": false
        }
      }
    }
  }
}
```

| Field | Rules |
|:--|:--|
| `schema` | Must be `1`. |
| `channel` | Must match the directory (`releases/<channel>/channels.json`). |
| `kind` | `plugin`, `bundle` or `runner`. **`bundle` requires a `pluginId` different from the module name** (the installed QwenPaw plugin id differs from the bundle id). `runner` modules cannot be installed as QwenPaw plugins. |
| `pluginId` | QwenPaw plugin id actually installed. Omitted = the module name. |
| `latest` | Highest version in this channel, or `null` when the module has no release in this channel. |
| `versions.<v>.artifacts` | `{name, sha256}` under `releases/<module>/<v>/`. The sha256 in the index is authoritative — the installer verifies the download against it. |
| `versions.<v>.requires` | Runtime requirements checked before install (`qwenpaw`, `qwen-code-cli`, or `agentteams` / `contracts` / `tool_schema` for bundles). |
| `versions.<v>.commit` | Source commit the artifact was built from. |
| `versions.<v>.deprecated` | `true` marks a version as retired; installers should not pick it by default. |

Client-side selection rules:

1. `latest` is used when the request omits `version`.
2. When the request omits `artifact`, the first `.zip` artifact is used (a
   release may also carry a runtime-neutral `.tar.gz`).
3. Versions are **semver**: `0.1.1-alpha.2 < 0.1.1`, but `0.1.1-alpha.2 > 0.1.0`.
4. Downgrades are allowed when pinned explicitly; clients should warn.

## Install endpoints

| Plane | Route | Authorization |
|:--|:--|:--|
| Worker | `POST /api/v1/workers/{name}/plugins/install` | `update` on `module` |
| Team | `POST /api/v1/teams/{name}/modules/{module}/install` | `update` on `team` |
| Manager | `POST /api/v1/managers/{name}/plugins/install` | `update` on `manager` |

Shared request body:

```json
{
  "module": "qwenpaw-qwencode-bridge",
  "source": "github",
  "repo": "LUOSENGWA/millforge",
  "ref": "main",
  "channel": "stable",
  "version": "0.3.0",
  "artifact": "qwenpaw-qwencode-bridge-0.3.0.zip",
  "sha256": "01efceee…"
}
```

`version` and `artifact` are optional (index `latest` / first `.zip` apply).
`sha256` is optional; when present it must match the index — a mismatch is
rejected with `409`.

Responses:

```json
// worker / manager
{"installed": true, "loaded": true, "previous_version": "0.2.0"}

// team
{"updated": ["worker-a", "worker-b"], "target_version": "0.3.0",
 "module": "qwenpaw-qwencode-bridge", "pluginId": "qwen-code-bridge",
 "members": {"worker-a": "0.3.0", "worker-b": "0.3.0"}, "consistent": true}
```

Behaviour:

* The controller **re-reads the channel index** for every install and uses the
  index checksum; the artifact is downloaded from the allowlisted raw hosts
  (60 s timeout, 64 MiB cap) and verified before upload.
* Team installs roll through `workerMembers` **in order and stop at the first
  failure**; the response lists the updated members and the failing one(s).
* Kube mode: install routes return `503` (image-based deployments update by
  rebuilding).
* Manager plane: only a QwenPaw manager is installable; other runtimes return
  `400`.
* `runner` modules return `400` on these routes.

## Reading installed module state

| Route | Returns |
|:--|:--|
| `GET /api/v1/workers/{name}/plugins` | Installed plugins on one worker (`id`, `version`, `loaded`, …) |
| `GET /api/v1/managers/{name}/plugins` | Installed plugins on the manager |
| `GET /api/v1/teams/{name}/modules` | Aggregated view over `workerMembers`: `module`, `pluginId`, per-member versions, `consistent` |

These are what the dashboard’s "check for updates" view compares against the
channel index.

## End-to-end example

```bash
# 1) what is available on stable?
curl -s "http://<controller>/api/v1/millforge/index?source=github&channel=stable" \
  | jq '.modules["qwenpaw-qwencode-bridge"].latest'

# 2) install it on one worker
curl -s -X POST "http://<controller>/api/v1/workers/worker-a/plugins/install" \
  -H 'Content-Type: application/json' \
  -d '{"module":"qwenpaw-qwencode-bridge","source":"github","channel":"stable"}'

# 3) verify
curl -s "http://<controller>/api/v1/workers/worker-a/plugins" \
  | jq '.[] | select(.id=="qwen-code-bridge")'
```

## Failure modes

| Symptom | Cause | Fix |
|:--|:--|:--|
| `404 module not found in index` | Module key typo, or the module has no entry in this channel | Check `latest` for the module; try another channel |
| `409` on install | Body `sha256` differs from the index, or the index changed between requests | Omit `sha256` (index is authoritative) or refetch |
| `502 millforge index: upstream unavailable` | Raw host unreachable / index path wrong | Verify `repo`/`ref`/`channel` and network access |
| `400 bundle modules require an adapter artifact with a distinct pluginId` | `kind: bundle` entry without `pluginId` | Fix the publisher’s `channels.json` |
| `400 runner modules are not installed as qwenpaw plugins` | Tried to install `looprunner` | Runners have their own deployment path |
| `503` | Kubernetes mode | Rebuild the image instead |

## Authentication & authorization

Two bearer-token paths are accepted, matching the rest of the controller API:

1. **Kubernetes service-account token** (TokenReview): admin / manager / worker.
2. **Matrix access token** (L2 humans): validated with
   `GET /_matrix/client/v3/account/whoami`; the owning localpart must map to a
   `Human` CR with `permissionLevel: 2`.

Reads require `get`; installs require `update` on the target resource
(`module`, `team`, or `manager`). Team leaders can only install into their own
team.

## Related

* [Plugin inventory](../../plugins/README.md) — what lives under `plugins/`
  and how each form is installed.
* [Qwen Code bridge](../../plugins/qwen-code-bridge/README.md) — the first
  worker plugin shipped through this channel.
* [Local deployment](deployment/local.md) — image-based upgrades.
