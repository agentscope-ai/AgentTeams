# Inspect runtime metrics and logs

Updated shared Harness images also provide [local runtime logs](runtime-logging.md)
with boot IDs, lifecycle state and supported message/model/tool error metadata.

You can ask your Manager, for example:

> Check the Controller and development Worker CPU/memory, then inspect the last
> 15 minutes of logs to explain why the Worker is not replying.

The Manager's `task-diagnosis` skill uses its own Controller identity and the
read-only CLI below. It correlates metrics, container logs and authorized traces,
reports observation times and evidence gaps, and distinguishes observations from
a likely explanation. A diagnosis does not restart a container or retry a task.

Replies lead with the answer and show short Markdown evidence blocks for the
sources actually queried: resource and sample time, relevant CPU/memory values,
a few verbatim redacted log lines, or a Trace's error/slowest step. Detail links
open the corresponding Dashboard resource, log window, or exact Trace/span.
Log links preserve the query start time and tail; refreshing reads newer data,
so details are not an immutable snapshot of the reply. The viewer's permissions
still apply. Unavailable sources and evidence gaps are stated explicitly.

The evidence styling and targeted navigation require the Dashboard version
containing observability conversation links. Other Markdown clients retain the
readable summary; relative Dashboard links work when opened in the Dashboard.

## CLI

Run these commands in a Manager/Controller environment with `agt` and an
authorized identity already configured:

```bash
agt monitoring
agt monitoring --search dev --page 1 --limit 12 -o json
agt logs component controller --since 15m --tail 300
agt logs manager default --since 1h --tail 300 -o json
agt logs worker dev --since 2026-11-14T08:00:00+08:00 --tail 100 -o json
```

Use `monitoring` to discover exact resource kinds/names. Pagination uses
`--page` and `--limit` (1–50); JSON preserves all Controller fields including
activity, lifecycle information and availability. CPU 100% means one CPU core.
Unknown usage stays unavailable; unlimited memory means no explicit container
quota. These are current samples, not historical resource measurements.

`logs` supports `worker`, `manager` and `component`, `--tail` (1–1000), and
`--since` (positive duration or RFC3339 timestamp with timezone). Text mode emits
log lines to stdout and sample/truncation information to stderr; terminal control
characters are removed. JSON preserves the Controller's text and coverage fields.
Common credential values are masked by the Controller; avoid sharing unnecessary
log content. Container output is bounded to 1 MiB and is not a full archive or a
query of every application log file.

Both commands use the existing `AGENTTEAMS_CONTROLLER_URL`,
`AGENTTEAMS_AUTH_TOKEN` / `AGENTTEAMS_AUTH_TOKEN_FILE` contract. They only issue
GET requests through the Controller; they do not use Docker, exec, host sockets,
or Dashboard credentials. Administrators and Managers can query containers;
scoped Humans, Leaders and Workers are denied. Permission/source failures exit
nonzero rather than producing a successful empty result.

## Availability and rollout

Upgrade the Controller's [monitoring API](../design/runtime-monitoring-api.md)
before using these commands, then update the shared Harness image containing
the new `agt` and Manager skill bundle. Controller image staging and both QwenPaw
plugin packages include the skill. A source-file change alone does not update an
already running Manager's CLI or loaded skills.

Docker embedded supplies the current metrics/log source. Kubernetes/external
Workers may report unsupported. Embedded Matrix, gateway and storage share the
Controller container. Tracing requires its collection/query backend to be
configured independently; an unavailable backend or empty log window does not
prove there was no execution or error.
