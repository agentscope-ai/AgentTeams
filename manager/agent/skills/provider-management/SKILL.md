---
name: provider-management
description: Register, update, or remove an extra OpenAI-compatible LLM provider on the Higress AI Gateway from chat. Use when the admin asks to add/onboard a new LLM provider or model vendor, or to remove one.
---

# Provider Management

Chat-driven onboarding of an additional OpenAI-compatible LLM provider (e.g. Ollama Cloud,
Xiaomi MiMo, or any other OpenAI-compatible endpoint) alongside the default provider, without
touching the boot-time `default-ai-route` configuration.

This is the interactive counterpart to `setup-higress.sh`'s env-gated `HICLAW_EXTRA_LLM_PROVIDERS`
loop (boot-persistent alternative — see "Boot-persistent alternative" below). Both paths register
the identical shapes: a DNS service-source, an `openai`-type provider, and the provider's own AI
route named `hiclaw-<name>-route`, matched by model-name prefix `<name>/`.

## When to use

The human admin asks (in chat) to add, register, or onboard a new LLM provider/model vendor, or
to remove a previously-registered one.

## Usage

```bash
bash /opt/hiclaw/agent/skills/provider-management/scripts/register-provider.sh <name> \
    --url <base-url> (--key <API-key> | --key-env <VAR>) [--models "id1,id2"] [--delete]
```

- `<name>` — provider name (becomes the Higress service-source/provider name and the route's
  model-prefix match `^<name>/`). Must not contain `/`.
- `--url <base-url>` — required (unless `--delete`). OpenAI-compatible base URL.
- `--key <API-key>` / `--key-env <VAR>` — exactly one required (unless `--delete`). See
  **Key hygiene** below.
- `--models "id1,id2"` — informational only; recorded in the script's log output as a pointer for
  the optional catalog follow-up (see below). Does not change the route's matching, which is
  always the `<name>/` prefix.
- `--delete` — reverses registration: removes the route, provider, and service-source for `<name>`.

Examples:
```bash
bash register-provider.sh ollama --url https://ollama.com/v1 --key-env OLLAMA_KEY
bash register-provider.sh mimo --url https://platform.xiaomimimo.com/v1 --key sk-xxxx
bash register-provider.sh ollama --delete
```

## Key hygiene (important)

A key passed via `--key` transits the Manager's chat history over Matrix before it ever reaches
this script — and **E2EE is off in the local deployment**. Prefer `--key-env <VAR>` with the key
pre-set as an environment variable outside of chat wherever possible. If the admin does paste a
key directly in chat, treat it as compromised once onboarding succeeds and ask them to rotate it
at the provider. The script itself never echoes the key to stdout or logs — it only appears
inline in the (unlogged) request body sent to the Higress Console.

## What the script does

1. Ensures the Higress console session is usable: if a call returns the HTML session-expired page
   (or a 401/403), it re-logs in once using `HICLAW_ADMIN_USER`/`HICLAW_ADMIN_PASSWORD` (always
   present in the Manager container env) and retries exactly once. If re-login also fails, it
   hard-fails with a clear message — it does not loop or guess.
2. Parses `<name>` and `--url` (validates no `/` in the name, per `docs/faq.md:550-552`).
3. GET-then-PUT/POST idempotent upsert of:
   - a DNS service-source for the provider's domain,
   - an `openai`-type LLM provider using `openaiCustomUrl`,
   - the provider's own AI route `hiclaw-<name>-route`, matched by model-prefix `<name>/`, with
     `allowedConsumers: ["manager"]` (matching the boot-loop's `setup-higress.sh:364-386` shape).
4. **Never reads or writes `default-ai-route`.** That route is rewritten on every Manager boot
   (`setup-higress.sh:253-281`); any edits made to it interactively would be silently clobbered at
   the next restart. Keeping the new provider on its own route name/model-prefix sidesteps that
   entirely — this is the same rule the `setup-higress.sh` 5c loop already follows.
5. `--delete` removes the route, provider, and service-source for the name (best-effort; each
   delete is independent so a partially-registered provider can still be cleaned up).

## Priority note (S5)

⚠️ Whether `hiclaw-<name>-route`'s prefix-matched requests could ever be shadowed by
`default-ai-route`'s path-only catch-all is a route-priority question that can only be confirmed
against a live Higress instance (carried from the Milestone-2 unverified-assumption ledger,
items 1–3; Milestone-3 ledger item 5). If a newly-onboarded provider's models aren't being routed
correctly, this is the first thing to check once live, not a sign the registration bodies are
wrong.

## After registration: pin agents to the new provider

Registering a provider does not automatically send any traffic to it. To actually use it:

- **Per-agent:** set `spec.modelProvider: "<name>"` on the Worker/Team-member spec. The
  controller's `ResolveModelProvider`/`AuthorizeAIRoutes` logic adds that agent's consumer to the
  matching route automatically — no skill action needed here.
- **Team-wide default:** set `Team.spec.modelProvider: "<name>"` (Milestone 3 Step 4) so every
  member of the team without its own per-agent pin falls back to the new provider.

## Model catalog note

Until the model catalog is updated, models from a newly-onboarded provider resolve to the unknown-
model default (150k context / 128k max output, `hiclaw-controller/internal/agentconfig/generator.go:412`).
This is safe but imprecise. If you know the new provider's real context/output limits, the optional
follow-up is the same 4-spot catalog procedure used for Ollama/MiMo in Milestone 2 Step 5
(`generator.go`, `manager/configs/known-models.json`, `manager/configs/manager-openclaw.json.tmpl`,
`manager/agent/skills/model-switch/scripts/update-manager-model.sh`) — update all four together.

## Boot-persistent alternative

For a provider that should survive every Manager restart without being re-registered from chat,
set `HICLAW_EXTRA_LLM_PROVIDERS="name1=url1;name2=url2"` (plus per-provider
`HICLAW_<NAME>_API_KEY`) in the Manager's env — `setup-higress.sh`'s 5c loop registers the exact
same shapes on every boot. Use this script for one-off, chat-driven onboarding instead; use the
env var for providers that should be part of the standing deployment config.

## Out of scope

- No controller or Gitea involvement (decision #12 stays untouched).
- No broadcast of the new provider's consumer to every worker (decision #14) — the route's
  `allowedConsumers` starts as `["manager"]` only; the controller adds pinned agents individually.
- Live console acceptance of these request bodies on the deployed Higress version, and an actual
  end-to-end chat-driven onboarding of a real provider, are deploy-time checks (Milestone-3 ledger
  items 5/6), not something this skill can verify offline.
