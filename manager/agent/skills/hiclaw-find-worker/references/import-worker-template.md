# Find Or Import a Worker Template

Use this flow when the admin asks you to:

- install or import a Worker template from a registry
- import a direct package URI such as `nacos://host:port/namespace/spec`
- find a Worker template that matches a natural-language requirement
- recommend a few template options before creating a Worker

Do not use this flow for hand-authored Workers. For that, switch to `worker-management` and read its `references/create-worker.md`.

## Step 0: Decide whether to search or import directly

If the admin already gave you a full package URI such as `nacos://host:port/namespace/spec`:

1. Do not search.
2. Confirm the Worker name you should create.
3. Briefly restate the package URI.
4. Wait for confirmation, then install with `--package-uri`.

If the admin did not give you a package URI, search templates first.

## Step 1: Search templates when needed

Use the template search script:

```bash
# Exact template name
bash /opt/agentteams/agent/skills/hiclaw-find-worker/scripts/hiclaw-find-worker.sh \
  --name <TEMPLATE_NAME> --json

# Requirement-based search
bash /opt/agentteams/agent/skills/hiclaw-find-worker/scripts/hiclaw-find-worker.sh \
  --query "<admin requirement>" --limit 3 --json
```

The JSON includes:

- `registry.host`, `registry.port`, `registry.namespace`
- `templates[].name`
- `templates[].summary`
- `templates[].package_uri`
- `templates[].match_reason`

Today the search implementation is backed by Nacos AgentSpecs, but the admin does not need to know or care about that detail.

## Step 2: Recommend, then wait for confirmation

If the admin named a template and there is exactly one match:

1. Confirm the Worker name you should create.
2. Briefly restate the template and package URI.
3. Wait for confirmation before installing.

If the admin described a need and multiple templates match:

1. Recommend the top 1-3 templates.
2. Explain in one short sentence why each one fits.
3. Ask the admin which template to install.
4. Do not install anything until they confirm.

If there are no useful template matches, switch back to `worker-management` and hand-create the Worker.
This fallback is only allowed before any template has been confirmed for installation.

## Step 3: Install the confirmed template

After the admin confirms the template and Worker name, run:

```bash
bash /opt/agentteams/agent/skills/hiclaw-find-worker/scripts/install-worker-template.sh \
  --template <TEMPLATE_NAME> \
  --worker-name <WORKER_NAME>
```

Optional overrides:

```bash
bash /opt/agentteams/agent/skills/hiclaw-find-worker/scripts/install-worker-template.sh \
  --template <TEMPLATE_NAME> \
  --worker-name <WORKER_NAME> \
  --model <MODEL_ID> \
  --runtime openclaw|copaw \
  --skills s1,s2 \
  --mcp-servers m1,m2
```

This script delegates to `agt apply worker --package nacos://...`.

If the install command fails:

1. Do not run `create-worker.sh`.
2. Do not switch to `worker-management` automatically.
3. Reply that the template installation failed.
4. Quote or summarize the key error from the command output so the admin sees the real failure reason.
5. Ask whether the admin wants a different template or an explicit hand-authored Worker instead.

If the admin already gave you a full package URI, run:

```bash
bash /opt/agentteams/agent/skills/hiclaw-find-worker/scripts/install-worker-template.sh \
  --package-uri <PACKAGE_URI> \
  --worker-name <WORKER_NAME>
```

## Gotchas

- Do not write `SOUL.md` yourself for imported Workers unless the admin explicitly wants a hand-authored Worker instead of a template.
- Do not run `create-worker.sh` for template imports.
- Always confirm before installing when the template came from a fuzzy search.
- If the admin already gave you a precise template name, you can skip recommendation and move straight to confirmation.
- If the admin already gave you a package URI, you can skip both search and recommendation and move straight to confirmation.
- After a confirmed template install fails, stay in the template-import flow and report the failure. Do not silently recover by hand-creating a Worker.
