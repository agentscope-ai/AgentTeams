# Create a Worker

Use this workflow when the admin requests a new Worker. Keep model decisions to
the requested role, runtime, model and skills. Let the CLI and Controller own
provisioning, polling, credentials and container lifecycle.

## Resolve the inputs once

- Use the name and runtime already given. A name uses lowercase letters, digits
  and hyphens and starts with a letter or digit.
- If the request omits a runtime or model, use the configured defaults. Do not
  scan environment variables or image files to rediscover them.
- Use the requested role as concise SOUL text. Ask for missing information only
  when it prevents creation; do not repeat questions already answered.
- Add named or role-relevant skills. Builtin skills are already included.
- Use `agt apply -f` for CPU/memory resources or MCP server configuration.
  Do not invent a `--mcp-servers` flag for `agt create worker`.

## Create once, with a bounded CLI wait

For one Worker:

```bash
agt create worker --name frontend-worker --runtime qwenpaw \
  --soul 'You are a frontend Worker. Implement requested UI changes and report results.' \
  --wait-timeout 45s -o json
```

Add `--model MODEL` and `--skills skill1,skill2` only when needed.
Pass SOUL as one literal argument. Never build an `eval` command with user text;
dollar signs and backticks execute inside shell double quotes. Use safe literal
quoting or the CLI's supported `--soul-file` for already available content.

The command creates the resource once and polls its status in code, without
another model turn per check. Read its final result:

- Ready: report readiness. This proves the runtime is ready, not that a model
  conversation has been tested.
- Startup failure: report the returned phase and message. Diagnose the specific
  error only when needed.
- Wait timeout: creation was accepted and may still be progressing. Report the
  last status and end your turn. Do not create again, run ensure-ready, recreate
  the container or repeat the waiter in the same turn.

For a later operation that needs this existing Worker ready:

```bash
agt worker wait-ready --name frontend-worker --timeout 45s -o json
```

This waiter reads status; it does not wake, delete or recreate the Worker.

## Batch creation and communication

For several Workers, use one foreground `agt create worker ... --no-wait -o json`
call per Worker and acknowledge acceptance promptly. Do not background these
commands or write a new shell polling loop. Wait only for Workers needed by the
next operation, using the bounded waiter above.

Do not send greeting or repeated ping messages as a readiness test: they invoke
the Worker's model. Send a test conversation only when the admin requests it.
For an actual task, use the returned room ID and full Matrix ID from
`agt get workers NAME -o json` and send the requested task once.

## Autonomous checks

Ordinary OpenClaw Workers default to model-driven heartbeats off. Enable them
only when the admin requests proactive periodic checks:

```bash
agt update worker --name frontend-worker --llm-heartbeat-enabled=true
agt update worker --name frontend-worker --llm-heartbeat-enabled=false
```

The Dashboard Worker editor exposes the same setting. Do not create a competing
environment-variable override. Health reporting and independent scheduled jobs
such as Dreaming are separate.

## Imported Workers

An import may already have created the Worker. Read the import result before
creating anything; follow its supported start instructions instead of sending
a duplicate create request.
