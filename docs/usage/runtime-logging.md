# Manager and Worker runtime logs

Updated Harness images use the same launcher for Manager, Leader and Worker.
The launcher captures bootstrap and native stdout/stderr, preserves the exit
code, forwards termination signals and keeps streaming redacted output to
Docker/Kubernetes. Updating the Controller alone does not update running Harnesses.

## Files and retention

| File | Contents |
| --- | --- |
| `runtime.log` | Bootstrap and native stdout/stderr |
| `events.jsonl` | Lifecycle, messaging and supported model/tool metadata |
| `status.json` | Latest lifecycle state, boot ID, readiness evidence and exit code |

The default directory is `/var/log/agentteams/manager/` for Managers and
`/var/log/agentteams/workers/<resource-name>/` for other members. Every launch
has its own boot ID and runtime/role/name labels. `AGENTTEAMS_LOG_DIR` overrides
the directory. Mount a volume there to retain logs after deleting a container;
the launcher does not upload logs or automatically provision persistent volumes.

Each rotating log defaults to 10 MiB and three backups. Use
`AGENTTEAMS_LOG_MAX_BYTES` and `AGENTTEAMS_LOG_BACKUPS` to change retention.
Log files use mode 0600. Credential environment values, common credential fields,
Bearer tokens and URL passwords are masked. Structured records omit message
bodies, prompts, answers and tool arguments; error summaries and native output
can still contain application text. Review logs before sharing them.

## Diagnostic coverage

| Harness | Ready evidence | Message events | Model/tool events |
| --- | --- | --- | --- |
| OpenClaw | Native gateway health | Native console | Native verbose console |
| QwenPaw | Native API HTTP success | Native console and tracing adapter | Local native span metadata, errors and HTTP status |
| Hermes | Native Matrix connection | Received, sent, send failure | Local native span metadata, errors and HTTP status |
| DeepSeek | Successful Matrix sync | Received, sent, channel/turn failure | Native request/tool metadata and agent-turn errors |
| Codex | Successful Matrix sync | Received, sent, poll/delivery failure | App Server tool IDs/status/errors/exit codes and turn errors |
| OpenHuman | Native core health | Native console | Native console |

OpenClaw and OpenHuman retain native console coverage; this collector does not
invent structured tool/model events for them. DeepSeek's native request/tool
metadata is collected when its subprocess completes. An `inference` event with
`layer=agent_turn` can contain several model requests and tool calls. A `ready`
state proves only the stated check, not a successful model conversation.

QwenPaw and Hermes reuse the bundled native instrumentation for local metadata
without requiring a Collector. With remote OTel disabled, no OTLP exporter is
created and content capture defaults to off. Existing explicit OTel settings
continue to control remote tracing. Native duplicate QwenPaw worker file logs
default to off; explicitly enabling `QWENPAW_WORKER_LOG_FILE_ENABLED` preserves
them under `<log-directory>/native` unless its native path is overridden.

## Diagnosis

Use the existing read-only Controller interface first:

```sh
agt logs manager default --since 15m --tail 300
agt logs worker alice --since 15m --tail 300
```

The structured events also appear in container stdout, so the existing Manager
diagnosis skill can inspect them without Docker access. The CLI reads container
logs, not every local file. For host operators, inspect a mounted archive or:

```sh
docker exec agentteams-manager cat /var/log/agentteams/manager/status.json
docker exec agentteams-manager tail -n 100 /var/log/agentteams/manager/events.jsonl
```

Correlate the boot ID and Matrix room/event IDs, then inspect the actual error,
HTTP status or tool exit code. Missing instrumentation is an evidence gap, not
proof that a call did not happen. See [runtime diagnosis](runtime-diagnosis.md)
for permissions, source coverage and Dashboard navigation.
