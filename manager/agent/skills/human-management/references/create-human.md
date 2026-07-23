# Create Human

Create a Human CR through the Controller API. The Human reconciler owns Matrix identity provisioning, room access, and status.

## Prerequisites

- Referenced Team CRs already exist for `--teams`.
- Referenced Worker CRs already exist for `--workers`.
- The localpart in `--matrix-id` is used as the Human resource name.

## Usage

```bash
bash /opt/agentteams/agent/skills/human-management/scripts/create-human.sh \
  --matrix-id "@john:domain" --name "John Doe" --level 1 \
  --email john@example.com

bash /opt/agentteams/agent/skills/human-management/scripts/create-human.sh \
  --matrix-id "@jane:domain" --name "Jane Smith" --level 2 \
  --teams alpha-team,beta-team --workers standalone-dev
```

The script calls `agt create human` and then returns `agt get humans <name> -o json`. Query that resource again to follow asynchronous provisioning:

```bash
agt get humans john -o json
```

Do not register Matrix accounts or edit agent allowlists directly. The Human reconciler derives those effects from the Human CR.
