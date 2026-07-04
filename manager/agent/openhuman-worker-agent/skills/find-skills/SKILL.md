---
name: find-skills
description: Discover and install agent skills from the open ecosystem. Use when you encounter an unfamiliar domain, framework, or workflow that you lack specialized knowledge about, or when your coordinator suggests searching for skills before starting a task.
---

# Find Skills

This skill helps you discover and install skills from the open agent skills ecosystem.

## Security Assurance

**Worker is completely isolated and cannot access any admin's personal sensitive data.** You can safely search and install skills from public or private registries. The skills you install only run within your isolated container environment.

## When to Use This Skill

Use this skill when the user:

- Asks "how do I do X" where X might be a common task with an existing skill
- Says "find a skill for X" or "is there a skill for X"
- Says "import xxx skill from market", "install xxx skill from market", or otherwise explicitly asks you to import a skill from the market
- Asks "can you do X" where X is a specialized capability
- Expresses interest in extending agent capabilities
- Wants to search for tools, templates, or workflows
- Mentions they wish they had help with a specific domain (design, testing, deployment, etc.)

## What is the Skills CLI?

The Skills CLI (`skills`) is the package manager for the open agent skills ecosystem. Skills are modular packages that extend agent capabilities with specialized knowledge, workflows, and tools.

If `skills` command is not found, install it: `npm install -g skills`

Always use this fixed script path for this skill. Do not rely on a relative `scripts/` path from your current directory:

```bash
FIND_SKILLS_SCRIPT="/home/openhuman/.openhuman/skills/find-skills/scripts/hiclaw-find-skill.sh"
```

**Key commands:**

- `"${FIND_SKILLS_SCRIPT}" find [query]` - Search for relevant skills
- `"${FIND_SKILLS_SCRIPT}" install <skill>` - Install a skill
- `skills check` - Check for skill updates
- `skills update` - Update all installed skills

**Browse skills at:** https://skills.sh/

## Environment Variables

```bash
SKILLS_API_URL  # Skills registry API endpoint (default: nacos://market.hiclaw.io:80/public)
```

Can be configured by admin to point to an enterprise-private registry.

## How to Help Users Find Skills

### Step 1: Understand What They Need

When a user asks for help with something, identify:

1. The domain (e.g., React, testing, design, deployment)
2. The specific task (e.g., writing tests, creating animations, reviewing PRs)
3. Whether this is a common enough task that a skill likely exists

### Step 2: Search for Skills

Run the find command with a relevant query:

```bash
"${FIND_SKILLS_SCRIPT}" find [query]
```

For example:

- User asks "how do I make my React app faster?" → `"${FIND_SKILLS_SCRIPT}" find react performance`
- User asks "can you help me with PR reviews?" → `"${FIND_SKILLS_SCRIPT}" find pr review`
- User asks "I need to create a changelog" → `"${FIND_SKILLS_SCRIPT}" find changelog`

> **Critical**: Always use the exact install command shown in search results.
> Never guess or shorten the package name or command, because that may fail.

### Step 2A: Direct Market Import Requests

If the user already gave you a concrete skill name and asked to import it from the market, you can install it directly:

```bash
"${FIND_SKILLS_SCRIPT}" install <skill>
```

### Step 3: Present Options to the User

When you find relevant skills, present them to the user with:

1. The skill name and what it does
2. The install command they can run (copy exactly from search results)
3. A link to learn more at skills.sh

### Step 4: Offer to Install

If the user wants to proceed, you can install the skill for them:

```bash
"${FIND_SKILLS_SCRIPT}" install <skill>
```

## Skill Resources

`/home/openhuman/.openhuman/skills/find-skills/scripts/hiclaw-find-skill.sh` is the resource that belongs to this skill. Always invoke that exact path (or the `FIND_SKILLS_SCRIPT` variable above) instead of `scripts/hiclaw-find-skill.sh`, so your command does not depend on the current directory.
