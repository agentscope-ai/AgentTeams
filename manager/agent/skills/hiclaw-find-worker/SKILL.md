---
name: hiclaw-find-worker
description: Use as a helper when the admin is assigning work but has not specified an existing Worker, so you need to search Nacos for a suitable Worker and ask whether to import it; also use when the admin explicitly says to import a Worker from the market or gives a direct nacos package URI.
---

# AgentTeams Find Worker

Use this skill as a helper around Worker discovery and import from Nacos AgentSpecs.

Use it in two cases:

- The admin is assigning work, but has not specified which existing Worker should do it. Search Nacos for a suitable Worker, recommend candidates, and ask whether to import one.
- The admin explicitly says to import a Worker from the market, or gives a direct `nacos://...` package URI.

Do not use this skill for generic hand-created Worker creation. This skill only handles Nacos-backed Worker search and install, not zip packages or other import formats.

## Quick Flow

```bash
# Search by requirement
bash /opt/agentteams/agent/skills/hiclaw-find-worker/scripts/hiclaw-find-worker.sh \
  --query "<admin requirement>" --limit 3 --json

# Install after the admin confirms
bash /opt/agentteams/agent/skills/hiclaw-find-worker/scripts/install-worker-template.sh \
  --template <TEMPLATE_NAME> --worker-name <NAME>

# Or import a direct package URI after the admin confirms
bash /opt/agentteams/agent/skills/hiclaw-find-worker/scripts/install-worker-template.sh \
  --package-uri <PACKAGE_URI> --worker-name <NAME>
```

> Full workflow: read `references/import-worker-template.md`

## Gotchas

- **Always confirm before importing search results** — when you searched by task/requirement, recommend candidates first and wait for the admin to choose one.
- **This skill only imports from Nacos** — do not stretch it to zip files or other package formats.
- **Do not run `create-worker.sh` for Nacos imports** — imports use `agt apply worker --package ...`.
- **If the admin gives you `nacos://...` directly, stay in this skill** — treat it as an explicit package import, confirm the Worker name, then install without searching.
- **Only fall back before installation** — you may switch to `worker-management` only when search proves there is no usable Nacos candidate. Once the admin confirms a specific Worker/package URI, stay in this skill until install succeeds or fails.
- **Report install failures directly** — if `install-worker-template.sh` or `agt apply worker` fails, stop there and tell the admin the import failed. Include the key error from the command output. Do not hand-create a Worker after a confirmed import fails unless the admin explicitly asks for that fallback.
- **`package_uri` is the handoff contract** — use the `package_uri` from the search output as the package to install.

## Operation Reference

| Admin wants to... | Read | Key script |
|---|---|---|
| Search Nacos Workers by requirement or exact name | `references/import-worker-template.md` | `scripts/hiclaw-find-worker.sh` |
| Import a direct `nacos://...` package URI | `references/import-worker-template.md` | `scripts/install-worker-template.sh` |
| Install a confirmed Worker from Nacos | `references/import-worker-template.md` | `scripts/install-worker-template.sh` |
