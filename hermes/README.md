# hermes-worker

`hermes-worker` is the AgentTeams Worker runtime built on top of
[hermes-agent](https://github.com/NousResearch/hermes-agent) (v0.10.0,
git tag `v2026.4.16`).

It plays the same role as `copaw-worker`:

* Bootstraps a per-worker home directory (`HERMES_HOME`) populated from
  MinIO (`agentteams://` workspace).
* Translates the AgentTeams common config schema (`openclaw.json`) into
  hermes-agent's native config (`config.yaml` + `.env`) via a *bridge*.
* Runs hermes-agent's gateway in Matrix-only mode.
* Replaces hermes-agent's native `mautrix`-based Matrix adapter with a
  `matrix-nio` adapter (`hermes_matrix.adapter.MatrixAdapter`) that
  mirrors CoPaw worker's Matrix policies (allowlists, mention-required,
  encryption, vision support, group/dm split).

## Layout

```
hermes/
├── Dockerfile                 multi-stage build, single hermes venv
├── pyproject.toml             hermes-worker package metadata
├── scripts/
│   └── hermes-worker-entrypoint.sh   container entrypoint
└── src/
    ├── hermes_worker/         worker bootstrap (cli, sync, bridge, run-loop)
    │   ├── __init__.py
    │   ├── cli.py             Typer CLI, exposed as `hermes-worker`
    │   ├── config.py          WorkerConfig dataclass + env parsing
    │   ├── sync.py            FileSync (MinIO mirror)
    │   ├── bridge.py          openclaw.json → hermes config.yaml/.env/SOUL.md/skills
    │   └── worker.py          orchestrates bootstrap → bridge → run gateway
    └── hermes_matrix/         Matrix adapter (overlays gateway/platforms/matrix.py)
        ├── __init__.py
        └── adapter.py         matrix-nio adapter, copaw-equivalent policies
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
which has a different state model and encryption story than `matrix-nio`. The
AgentTeams stack standardised on `matrix-nio` for the CoPaw worker (room
allowlists, mention-required policy, free-response rooms, vision-on-image
support) and the Manager's matrix tooling. Reusing those semantics here keeps
operator behaviour identical regardless of which runtime a Worker is using,
and avoids re-implementing the room-policy / encryption-key-store layers.
