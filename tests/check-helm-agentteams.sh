#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${ROOT_DIR}/helm/agentteams"
COMMON_ARGS=(
    --set credentials.registrationToken=test
    --set credentials.adminPassword=test
    --set credentials.llmApiKey=test
    --set gateway.publicURL=http://localhost:18080
)

render="$(mktemp)"
deepagents_render="$(mktemp)"
deepagents_postgresql_render="$(mktemp)"
deepagents_ephemeral_postgresql_render="$(mktemp)"
deepagents_external_postgresql_render="$(mktemp)"
deepagents_special_render="$(mktemp)"
oss_render="$(mktemp)"
oss_deepagents_error="$(mktemp)"
trap 'rm -f "${render}" "${deepagents_render}" "${deepagents_postgresql_render}" "${deepagents_ephemeral_postgresql_render}" "${deepagents_external_postgresql_render}" "${deepagents_special_render}" "${oss_render}" "${oss_deepagents_error}"' EXIT

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" > "${render}"

grep -q 'name: agentteams-controller' "${render}"
grep -q 'app.kubernetes.io/name: agentteams' "${render}"
if grep -q 'name: agentteams-deepagents-postgresql' "${render}" || grep -q 'name: PGDATA' "${render}"; then
    echo "expected bundled PostgreSQL and PGDATA to be absent when DeepAgents is disabled" >&2
    exit 1
fi

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set deepagents.enabled=true > "${deepagents_render}"

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set deepagents.enabled=true \
    --set-string deepagents.postgresql.image.repository=registry.example.invalid/postgres \
    --set-string deepagents.postgresql.image.tag=upgrade-test \
    --set deepagents.postgresql.image.pullPolicy=Always \
    --show-only templates/deepagents/postgresql-statefulset.yaml > "${deepagents_postgresql_render}"

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set deepagents.enabled=true \
    --set deepagents.postgresql.persistence.enabled=false \
    --show-only templates/deepagents/postgresql-statefulset.yaml > "${deepagents_ephemeral_postgresql_render}"

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set deepagents.enabled=true \
    --set deepagents.postgresql.enabled=false \
    --set-string deepagents.checkpoint.dsn=postgresql://external.invalid/deepagents > "${deepagents_external_postgresql_render}"

grep -q 'name: agentteams-deepagents-postgresql' "${deepagents_render}"
grep -q 'name: deepagents-checkpoint-migrate' "${deepagents_render}"
grep -q 'name: AGENTTEAMS_DEEPAGENTS_RUNNER_IMAGE' "${deepagents_render}"
grep -q 'name: AGENTTEAMS_DEEPAGENTS_STATE_PVC_SIZE' "${deepagents_render}"
grep -q 'name: AGENTTEAMS_DEEPAGENTS_CHECKPOINT_SECRET' "${deepagents_render}"
if grep -q 'PGDATA' "${deepagents_postgresql_render}"; then
    echo "expected PostgreSQL to preserve the official root-level data path" >&2
    exit 1
fi
grep -q '^      automountServiceAccountToken: false$' "${deepagents_postgresql_render}"
grep -q '^      initContainers:$' "${deepagents_postgresql_render}"
postgresql_init="$(awk '/^      initContainers:$/ { capture=1 } capture { print } /^      containers:$/ { exit }' \
    "${deepagents_postgresql_render}")"
grep -q '^        - name: volume-permissions$' <<< "${postgresql_init}"
grep -q '^          image: "registry.example.invalid/postgres:upgrade-test"$' <<< "${postgresql_init}"
grep -q '^          imagePullPolicy: Always$' <<< "${postgresql_init}"
grep -q '^          command: \["chown", "70:70", "/var/lib/postgresql/data"\]$' <<< "${postgresql_init}"
grep -q '^            allowPrivilegeEscalation: false$' <<< "${postgresql_init}"
grep -q '^            readOnlyRootFilesystem: true$' <<< "${postgresql_init}"
grep -q '^              drop: \["ALL"\]$' <<< "${postgresql_init}"
grep -q '^              add: \["CHOWN"\]$' <<< "${postgresql_init}"
grep -q '^            runAsUser: 0$' <<< "${postgresql_init}"
grep -q '^              type: RuntimeDefault$' <<< "${postgresql_init}"
grep -q '^              mountPath: /var/lib/postgresql/data$' <<< "${postgresql_init}"
# A fresh PVC becomes writable at its mount root. An existing PVC retains the
# official root-level PG_VERSION/data layout because this is a non-recursive
# ownership change and neither PGDATA nor any move/delete operation is rendered.
if grep -Eq -- 'PGDATA|PG_VERSION|(^|[^[:alnum:]_])(rm|mv)([^[:alnum:]_]|$)|chown.*-R' <<< "${postgresql_init}"; then
    echo "expected volume permissions init to preserve root-level PostgreSQL contents" >&2
    exit 1
fi
if grep -Eq '(^|[[:space:]])(env:|secretKeyRef:|serviceAccountName:)' <<< "${postgresql_init}"; then
    echo "expected volume permissions init to run without credentials or a service account" >&2
    exit 1
fi
test "$(grep -c '^          image: "registry.example.invalid/postgres:upgrade-test"$' "${deepagents_postgresql_render}")" -eq 2
test "$(grep -c '^          imagePullPolicy: Always$' "${deepagents_postgresql_render}")" -eq 2
grep -q '^              mountPath: /var/lib/postgresql/data$' "${deepagents_postgresql_render}"
grep -q '^        fsGroup: 70$' "${deepagents_postgresql_render}"
grep -q '^            runAsUser: 70$' "${deepagents_postgresql_render}"
if grep -q 'name: volume-permissions' "${deepagents_ephemeral_postgresql_render}" || \
    grep -q 'runAsUser: 0' "${deepagents_ephemeral_postgresql_render}"; then
    echo "expected ephemeral PostgreSQL storage to avoid the root permissions init" >&2
    exit 1
fi
if grep -q 'PGDATA' "${deepagents_ephemeral_postgresql_render}"; then
    echo "expected ephemeral PostgreSQL to preserve the official root-level data path" >&2
    exit 1
fi
grep -q '^      automountServiceAccountToken: false$' "${deepagents_ephemeral_postgresql_render}"
grep -q '^      volumes:$' "${deepagents_ephemeral_postgresql_render}"
grep -q '^          emptyDir: {}$' "${deepagents_ephemeral_postgresql_render}"
grep -q '^              mountPath: /var/lib/postgresql/data$' "${deepagents_ephemeral_postgresql_render}"
if grep -q 'name: agentteams-deepagents-postgresql' "${deepagents_external_postgresql_render}" || \
    grep -q 'name: volume-permissions' "${deepagents_external_postgresql_render}" || \
    grep -q 'PGDATA' "${deepagents_external_postgresql_render}"; then
    echo "expected bundled PostgreSQL resources to be absent when external PostgreSQL is selected" >&2
    exit 1
fi

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set deepagents.enabled=true \
    --set-string 'deepagents.postgresql.auth.username=user@name' \
    --set-string 'deepagents.postgresql.auth.password=p@ss:/?#% x' \
    --set-string 'deepagents.postgresql.auth.database=db/name' > "${deepagents_special_render}"

grep -q 'postgresql://user%40name:p%40ss%3A%2F%3F%23%25%20x@agentteams-deepagents-postgresql:5432/db%2Fname?sslmode=disable' "${deepagents_special_render}"

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set storage.provider=oss \
    --set storage.mode=existing \
    --set storage.oss.region=cn-test \
    --set credentialProvider.enabled=true \
    --set credentialProvider.image.repository=example.invalid/credential-provider > "${oss_render}"

grep -q 'name: agentteams-controller' "${oss_render}"

if helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set deepagents.enabled=true \
    --set storage.provider=oss \
    --set storage.mode=existing \
    --set storage.oss.region=cn-test \
    --set credentialProvider.enabled=true \
    --set credentialProvider.image.repository=example.invalid/credential-provider > /dev/null 2> "${oss_deepagents_error}"; then
    echo "expected DeepAgents with OSS storage to be rejected" >&2
    exit 1
fi

grep -q 'deepagents.enabled=true requires storage.provider=minio' "${oss_deepagents_error}"

echo "PASS: AgentTeams Helm release renders canonical resources and DeepAgents dependencies"
