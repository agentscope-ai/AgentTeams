# hermes-worker

`hermes-worker` is the AgentTeams Worker runtime built on top of
[hermes-agent](https://github.com/NousResearch/hermes-agent) (git tag
`v0.22.5`).

It plays the same role as `copaw-worker`:

* Bootstraps a per-worker home directory (`HERMES_HOME`) populated from
  MinIO (`agentteams://` workspace).
* Translates the AgentTeams common config schema (`openclaw.json`) into
  hermes-agent's native config (`config.yaml` + `.env`) via a *bridge*.
* Runs hermes-agent's gateway in Matrix-only mode.
* Ships an AgentTeams Matrix **user plugin** (`plugins/matrix-platform`) that
  overrides hermes-agent's bundled `matrix` platform adapter with
  `hermes_matrix.overlay_adapter.MatrixAdapter`. The subclass keeps the native
  `mautrix` transport (E2EE, media, streaming, reactions) and adds CoPaw-style
  Worker policies: allowlists, history buffering, mention enrichment, and
  vision gating.

## Layout

```
hermes/
├── Dockerfile                 multi-stage build, single hermes venv
├── pyproject.toml             hermes-worker package metadata
├── scripts/
│   └── hermes-worker-entrypoint.sh   container entrypoint
├── plugins/
│   └── matrix-platform/      user plugin overriding the bundled Matrix adapter
│       ├── plugin.yaml
│       └── adapter.py        registration bridge; swaps in the policy subclass
└── src/
    ├── hermes_worker/         worker bootstrap (cli, sync, bridge, run-loop)
    │   ├── __init__.py
    │   ├── cli.py             Typer CLI, exposed as `hermes-worker`
    │   ├── config.py          WorkerConfig dataclass + env parsing
    │   ├── sync.py            FileSync (MinIO mirror)
    │   ├── bridge.py          openclaw.json → hermes config.yaml/.env/SOUL.md/skills
    │   └── worker.py          orchestrates bootstrap → bridge → run gateway
    └── hermes_matrix/         AgentTeams policy overlay on the bundled adapter
        ├── __init__.py
        ├── overlay_adapter.py  mautrix subclass with AgentTeams policies
        └── policies.py         allowlist/history/mention helpers
```

## Local development

```bash
# Bake the image locally
make build-hermes-worker

# Run a worker against the local MinIO/Matrix stack (requires a manager up)
docker run -it --rm \
    -e AGENTTEAMS_WORKER_NAME=worker-test \
    -e AGENTTEAMS_OSS_ENDPOINT=http://manager:9000 \
    -e AGENTTEAMS_MATRIX_HOMESERVER=http://manager:8008 \
    agentteams/hermes-worker:latest
```

## Configuration bridge

The bridge maps `openclaw.json` to hermes config in the following way:

| openclaw key                            | hermes destination                      |
|-----------------------------------------|------------------------------------------|
| `agentName`                             | `config.yaml: name`, `.env: AGENT_NAME` |
| `model.{provider,model,apiKey,baseURL}` | `.env: OPENAI_*`, `config.yaml: model`  |
| `embedding.{...}`                       | `.env: EMBEDDING_*`                     |
| `matrix.homeserver`                     | `.env: MATRIX_HOMESERVER`               |
| `matrix.accessToken` / `matrix.userId`  | `.env: MATRIX_ACCESS_TOKEN/USER_ID`     |
| `matrix.encryption`                     | `.env: MATRIX_ENCRYPTION`               |
| `matrix.allowedUsers`                   | `.env: MATRIX_ALLOWED_USERS`            |
| `matrix.requireMention`                 | `.env: MATRIX_REQUIRE_MENTION`          |
| `matrix.freeResponseRooms`              | `.env: MATRIX_FREE_RESPONSE_ROOMS`      |
| `matrix.homeRoomId`                     | `.env: MATRIX_HOME_ROOM`                |
| `system_prompt` / `SOUL.md`             | `<HERMES_HOME>/SOUL.md`                 |
| `skills/<name>/SKILL.md`                | `<HERMES_HOME>/skills/<name>/SKILL.md`  |

## Why a custom Matrix adapter?

hermes-agent's stock adapter uses [mautrix](https://github.com/mautrix/python),
and v0.22.5 moved it into a bundled platform plugin. AgentTeams does not fork
the transport: the user plugin keeps the stock mautrix adapter and only swaps in
a subclass with the Worker policy layer (room allowlists, mention-required,
free-response rooms, history buffering, vision gating). Reusing the upstream
transport keeps E2EE, media, streaming, and reactions current with each Hermes
release while preserving CoPaw-equivalent operator behavior.
