#!/usr/bin/env python3
"""
aliyun-api.py — Alibaba Cloud Worker management for HiClaw Manager.

Provides SAE application CRUD and AI Gateway consumer management,
callable from shell scripts (create-worker.sh, lifecycle-worker.sh).

Authentication priority:
  1. OIDC (ALIBABA_CLOUD_OIDC_TOKEN_FILE present) — SAE RRSA
  2. AK/SK (ALIBABA_CLOUD_ACCESS_KEY_ID present)  — local/debug
  3. Fail

Usage:
  aliyun-api.py sae-create  --name <worker> [--image <url>] [--envs '{"K":"V"}']
  aliyun-api.py sae-delete  --name <worker>
  aliyun-api.py sae-stop    --name <worker>
  aliyun-api.py sae-start   --name <worker>
  aliyun-api.py sae-status  --name <worker>
  aliyun-api.py sae-list
  aliyun-api.py gw-create-consumer --name <consumer>
  aliyun-api.py gw-bind-consumer   --consumer-id <id> --api-id <id> --env-id <id>

Output: JSON to stdout.  Logs to stderr.
"""

import argparse
import json
import os
import sys

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

def log(msg):
    print(f"[aliyun-api] {msg}", file=sys.stderr)

# ---------------------------------------------------------------------------
# Credential helpers
# ---------------------------------------------------------------------------

def _build_credential():
    """Build alibabacloud Credential based on environment."""
    from alibabacloud_credentials.client import Client as CredClient
    from alibabacloud_credentials.models import Config as CredConfig

    oidc_token_file = os.environ.get("ALIBABA_CLOUD_OIDC_TOKEN_FILE", "")
    ak = os.environ.get("ALIBABA_CLOUD_ACCESS_KEY_ID", "")

    if oidc_token_file and os.path.isfile(oidc_token_file):
        log("Using OIDC RRSA credentials")
        region = os.environ.get("HICLAW_REGION", "cn-hangzhou")
        conf = CredConfig(
            type="oidc_role_arn",
            role_arn=os.environ["ALIBABA_CLOUD_ROLE_ARN"],
            oidc_provider_arn=os.environ["ALIBABA_CLOUD_OIDC_PROVIDER_ARN"],
            oidc_token_file_path=oidc_token_file,
            role_session_name="hiclaw-manager-role",
            sts_endpoint=f"sts-vpc.{region}.aliyuncs.com",
        )
        return CredClient(conf)

    if ak:
        log("Using AK/SK credentials")
        conf = CredConfig(
            type="access_key",
            access_key_id=ak,
            access_key_secret=os.environ["ALIBABA_CLOUD_ACCESS_KEY_SECRET"],
        )
        return CredClient(conf)

    raise RuntimeError("No credentials found. Set ALIBABA_CLOUD_OIDC_TOKEN_FILE or ALIBABA_CLOUD_ACCESS_KEY_ID.")


def _get_sae_client():
    """Build SAE client with auto-detected credentials."""
    from alibabacloud_sae20190506.client import Client as SaeClient
    from alibabacloud_tea_openapi.models import Config as ApiConfig

    cred = _build_credential()
    region = os.environ.get("HICLAW_REGION", "cn-hangzhou")

    config = ApiConfig(
        credential=cred,
        region_id=region,
        endpoint=f"sae.{region}.aliyuncs.com",
    )
    return SaeClient(config)


def _get_apig_client():
    """Build AI Gateway (APIG) client with auto-detected credentials."""
    from alibabacloud_apig20240327.client import Client as ApigClient
    from alibabacloud_tea_openapi.models import Config as ApiConfig

    cred = _build_credential()
    region = os.environ.get("HICLAW_REGION", "cn-hangzhou")

    config = ApiConfig(
        credential=cred,
        region_id=region,
        endpoint=f"apig.{region}.aliyuncs.com",
    )
    return ApigClient(config)


# ---------------------------------------------------------------------------
# Helper: find SAE app by name
# ---------------------------------------------------------------------------

def _find_worker_app(sae, worker_name):
    """Find a SAE application by worker name. Returns (app_id, app_name) or (None, None)."""
    from alibabacloud_sae20190506 import models as sae_models

    namespace_id = os.environ.get("HICLAW_SAE_NAMESPACE_ID", "")
    app_name = f"hiclaw-worker-{worker_name}"

    req = sae_models.ListApplicationsRequest(
        namespace_id=namespace_id,
        app_name=app_name,
    )
    resp = sae.list_applications(req)
    if resp.body and resp.body.data and resp.body.data.applications:
        for app in resp.body.data.applications:
            if app.app_name == app_name:
                return app.app_id, app.app_name
    return None, None


# ---------------------------------------------------------------------------
# SAE operations
# ---------------------------------------------------------------------------

def sae_create(args):
    """Create a SAE application for a Worker."""
    from alibabacloud_sae20190506 import models as sae_models

    sae = _get_sae_client()
    app_name = f"hiclaw-worker-{args.name}"

    # Check if already exists
    existing_id, _ = _find_worker_app(sae, args.name)
    if existing_id:
        log(f"Application already exists: {app_name} ({existing_id})")
        print(json.dumps({"app_id": existing_id, "app_name": app_name, "status": "exists"}))
        return

    # Parse extra envs (supports @/path/to/file or inline JSON)
    envs = {}
    if args.envs:
        raw = args.envs
        if raw.startswith("@"):
            with open(raw[1:], "r") as f:
                raw = f.read()
        envs = json.loads(raw)

    # Read config from environment
    region = os.environ.get("HICLAW_REGION", "cn-hangzhou")
    namespace_id = os.environ.get("HICLAW_SAE_NAMESPACE_ID", "")
    image = args.image or os.environ.get("HICLAW_SAE_WORKER_IMAGE", "")
    vpc_id = os.environ.get("HICLAW_SAE_VPC_ID", "")
    vswitch_id = os.environ.get("HICLAW_SAE_VSWITCH_ID", "")
    sg_id = os.environ.get("HICLAW_SAE_SECURITY_GROUP_ID", "")
    oidc_role_name = os.environ.get("HICLAW_SAE_WORKER_OIDC_ROLE_NAME", "hiclaw-worker-role")
    cpu = int(os.environ.get("HICLAW_SAE_WORKER_CPU", "1000"))
    memory = int(os.environ.get("HICLAW_SAE_WORKER_MEMORY", "2048"))

    if not image:
        print(json.dumps({"error": "No worker image. Set HICLAW_SAE_WORKER_IMAGE or --image."}))
        sys.exit(1)

    # Base envs for worker (runtime-specific envs are passed via --envs by caller)
    base_envs = {
        "HICLAW_WORKER_NAME": args.name,
        "HICLAW_REGION": region,
        "TZ": "Asia/Shanghai",
    }
    base_envs.update(envs)

    # Build SAE envs JSON array format
    env_list = [{"name": k, "value": v} for k, v in base_envs.items()]

    req = sae_models.CreateApplicationRequest(
        app_name=app_name,
        namespace_id=namespace_id,
        package_type="Image",
        image_url=image,
        cpu=cpu,
        memory=memory,
        replicas=1,
        vpc_id=vpc_id,
        v_switch_id=vswitch_id,
        security_group_id=sg_id,
        app_description=f"HiClaw Worker Agent: {args.name}",
        envs=json.dumps(env_list),
        oidc_role_name=oidc_role_name,
        custom_image_network_type="internet",
    )

    resp = sae.create_application(req)
    app_id = resp.body.data.app_id
    log(f"Application created: {app_name} ({app_id})")
    print(json.dumps({"app_id": app_id, "app_name": app_name, "status": "created"}))


def sae_delete(args):
    """Delete a SAE application for a Worker."""
    from alibabacloud_sae20190506 import models as sae_models

    sae = _get_sae_client()
    app_id, app_name = _find_worker_app(sae, args.name)

    if not app_id:
        print(json.dumps({"app_name": f"hiclaw-worker-{args.name}", "status": "not_found"}))
        return

    req = sae_models.DeleteApplicationRequest(app_id=app_id)
    sae.delete_application(req)
    log(f"Application deleted: {app_name} ({app_id})")
    print(json.dumps({"app_id": app_id, "app_name": app_name, "status": "deleted"}))


def sae_stop(args):
    """Stop a SAE application for a Worker."""
    from alibabacloud_sae20190506 import models as sae_models

    sae = _get_sae_client()
    app_id, app_name = _find_worker_app(sae, args.name)

    if not app_id:
        print(json.dumps({"app_name": f"hiclaw-worker-{args.name}", "status": "not_found"}))
        return

    req = sae_models.StopApplicationRequest(app_id=app_id)
    sae.stop_application(req)
    log(f"Application stopped: {app_name} ({app_id})")
    print(json.dumps({"app_id": app_id, "app_name": app_name, "status": "stopped"}))


def sae_start(args):
    """Start a SAE application for a Worker."""
    from alibabacloud_sae20190506 import models as sae_models

    sae = _get_sae_client()
    app_id, app_name = _find_worker_app(sae, args.name)

    if not app_id:
        print(json.dumps({"app_name": f"hiclaw-worker-{args.name}", "status": "not_found"}))
        return

    req = sae_models.StartApplicationRequest(app_id=app_id)
    sae.start_application(req)
    log(f"Application started: {app_name} ({app_id})")
    print(json.dumps({"app_id": app_id, "app_name": app_name, "status": "running"}))


def sae_status(args):
    """Check SAE application status for a Worker."""
    from alibabacloud_sae20190506 import models as sae_models

    sae = _get_sae_client()
    app_id, app_name = _find_worker_app(sae, args.name)

    if not app_id:
        print(json.dumps({"app_name": f"hiclaw-worker-{args.name}", "status": "not_found"}))
        return

    req = sae_models.DescribeApplicationStatusRequest(app_id=app_id)
    resp = sae.describe_application_status(req)
    current_status = resp.body.data.current_status if resp.body.data else "unknown"

    # Normalize SAE status to simpler values
    status_map = {
        "RUNNING": "running",
        "STOPPED": "stopped",
        "UNKNOWN": "unknown",
        "DEPLOYING": "starting",
    }
    normalized = status_map.get(current_status, current_status.lower() if current_status else "unknown")

    print(json.dumps({
        "app_id": app_id,
        "app_name": app_name,
        "status": normalized,
        "sae_status": current_status,
    }))


def sae_list(args):
    """List all hiclaw-worker SAE applications."""
    from alibabacloud_sae20190506 import models as sae_models

    sae = _get_sae_client()
    namespace_id = os.environ.get("HICLAW_SAE_NAMESPACE_ID", "")

    req = sae_models.ListApplicationsRequest(namespace_id=namespace_id)
    resp = sae.list_applications(req)

    workers = []
    prefix = "hiclaw-worker-"
    if resp.body and resp.body.data and resp.body.data.applications:
        for app in resp.body.data.applications:
            if app.app_name and app.app_name.startswith(prefix):
                name = app.app_name[len(prefix):]
                workers.append({
                    "name": name,
                    "app_name": app.app_name,
                    "app_id": app.app_id,
                })

    print(json.dumps({"workers": workers}))


# ---------------------------------------------------------------------------
# AI Gateway consumer operations
# ---------------------------------------------------------------------------

def _find_existing_consumer(apig, consumer_name, retries=1, retry_delay=0):
    """Search for an existing consumer by name with optional retry (for API eventual consistency).

    Returns (consumer_id, api_key) or (None, None).
    """
    import time
    from alibabacloud_apig20240327 import models as apig_models

    for attempt in range(retries):
        if attempt > 0:
            log(f"Retry {attempt}/{retries - 1} after {retry_delay}s ...")
            time.sleep(retry_delay)

        page = 1
        while True:
            req = apig_models.ListConsumersRequest(
                    gateway_type="AI",
                    name_like=consumer_name,
                    page_number=page,
                    page_size=100,
                )
            resp = apig.list_consumers(req)
            if not resp.body.data or not resp.body.data.items:
                break
            for c in resp.body.data.items:
                if c.name == consumer_name:
                    detail = apig.get_consumer(c.consumer_id)
                    d = detail.body.data
                    key = None
                    if d.api_key_identity_config and d.api_key_identity_config.credentials:
                        key = d.api_key_identity_config.credentials[0].apikey
                    return c.consumer_id, key
            if len(resp.body.data.items) < 100:
                break
            page += 1

    return None, None


def gw_create_consumer(args):
    """Create an AI Gateway consumer for a Worker.

    Consumer name is prefixed with a short gateway ID to avoid account-level
    name collisions across gateways (Consumer is an account-level resource).
    The gateway ID is read from HICLAW_GW_GATEWAY_ID env var.
    """
    from alibabacloud_apig20240327 import models as apig_models

    apig = _get_apig_client()
    raw_name = args.name

    # Prefix consumer name with gateway ID to avoid cross-gateway collisions
    gateway_id = os.environ.get("HICLAW_GW_GATEWAY_ID", "")
    if gateway_id:
        consumer_name = f"{gateway_id}-{raw_name}"
    else:
        log("WARNING: HICLAW_GW_GATEWAY_ID not set, using raw consumer name")
        consumer_name = raw_name

    existing_id, existing_key = _find_existing_consumer(apig, consumer_name)
    if existing_id:
        log(f"Consumer already exists: {existing_id}")
        print(json.dumps({"consumer_id": existing_id, "api_key": existing_key, "status": "exists"}))
        return

    try:
        req = apig_models.CreateConsumerRequest(
            name=consumer_name,
            gateway_type="AI",
            enable=True,
            description=f"HiClaw Worker: {raw_name}",
            apikey_identity_config=apig_models.ApiKeyIdentityConfig(
                type="Apikey",
                apikey_source=apig_models.ApiKeyIdentityConfigApikeySource(
                    source="Default",
                    value="Authorization",
                ),
                credentials=[
                    apig_models.ApiKeyIdentityConfigCredentials(generate_mode="System")
                ],
            ),
        )
        resp = apig.create_consumer(req)
        consumer_id = resp.body.data.consumer_id
    except Exception as e:
        if "ConsumerNameDuplicate" in str(e) or "409" in str(e):
            log(f"Consumer creation returned 409, re-querying with retries...")
            existing_id, existing_key = _find_existing_consumer(apig, consumer_name, retries=3, retry_delay=2)
            if existing_id:
                log(f"Consumer found after 409: {existing_id}")
                print(json.dumps({"consumer_id": existing_id, "api_key": existing_key, "status": "exists"}))
                return
            raise RuntimeError(f"Consumer 409 but not found on re-query: {e}") from e
        raise

    detail = apig.get_consumer(consumer_id)
    key = None
    if detail.body.data.api_key_identity_config and detail.body.data.api_key_identity_config.credentials:
        key = detail.body.data.api_key_identity_config.credentials[0].apikey

    log(f"Consumer created: {consumer_id}, key={key}")
    print(json.dumps({"consumer_id": consumer_id, "api_key": key, "status": "created"}))


def gw_bind_consumer(args):
    """Bind a consumer to an HTTP API (LLM type)."""
    from alibabacloud_apig20240327 import models as apig_models

    apig = _get_apig_client()

    try:
        req = apig_models.QueryConsumerAuthorizationRulesRequest(
            consumer_id=args.consumer_id,
            resource_id=args.api_id,
            environment_id=args.env_id,
            resource_type="LLM",
            page_number=1,
            page_size=100,
        )
        resp = apig.query_consumer_authorization_rules(req)
        if resp.body.data and resp.body.data.items and len(resp.body.data.items) > 0:
            rule_ids = [r.consumer_authorization_rule_id for r in resp.body.data.items]
            log(f"Consumer already bound: {len(rule_ids)} rules")
            print(json.dumps({"rule_ids": rule_ids, "status": "exists"}))
            return
    except Exception:
        pass

    req = apig_models.CreateConsumerAuthorizationRulesRequest(
        authorization_rules=[
            apig_models.CreateConsumerAuthorizationRulesRequestAuthorizationRules(
                consumer_id=args.consumer_id,
                resource_type="LLM",
                expire_mode="LongTerm",
                resource_identifier=apig_models.CreateConsumerAuthorizationRulesRequestAuthorizationRulesResourceIdentifier(
                    resource_id=args.api_id,
                    environment_id=args.env_id,
                ),
            )
        ],
    )
    resp = apig.create_consumer_authorization_rules(req)
    rule_ids = resp.body.data.consumer_authorization_rule_ids or []
    log(f"Consumer bound: {len(rule_ids)} rules")
    print(json.dumps({"rule_ids": rule_ids, "status": "created"}))


# ---------------------------------------------------------------------------
# CLI entry point
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="HiClaw Cloud Worker API")
    sub = parser.add_subparsers(dest="command")

    # SAE commands
    p = sub.add_parser("sae-create")
    p.add_argument("--name", required=True)
    p.add_argument("--image")
    p.add_argument("--envs", default="{}")

    p = sub.add_parser("sae-delete")
    p.add_argument("--name", required=True)

    p = sub.add_parser("sae-stop")
    p.add_argument("--name", required=True)

    p = sub.add_parser("sae-start")
    p.add_argument("--name", required=True)

    p = sub.add_parser("sae-status")
    p.add_argument("--name", required=True)

    sub.add_parser("sae-list")

    # Gateway commands
    p = sub.add_parser("gw-create-consumer")
    p.add_argument("--name", required=True)

    p = sub.add_parser("gw-bind-consumer")
    p.add_argument("--consumer-id", required=True)
    p.add_argument("--api-id", required=True)
    p.add_argument("--env-id", required=True)

    args = parser.parse_args()

    commands = {
        "sae-create": sae_create,
        "sae-delete": sae_delete,
        "sae-stop": sae_stop,
        "sae-start": sae_start,
        "sae-status": sae_status,
        "sae-list": sae_list,
        "gw-create-consumer": gw_create_consumer,
        "gw-bind-consumer": gw_bind_consumer,
    }

    if args.command not in commands:
        parser.print_help()
        sys.exit(1)

    try:
        commands[args.command](args)
    except Exception as e:
        print(json.dumps({"error": str(e)}))
        sys.exit(1)


if __name__ == "__main__":
    main()
