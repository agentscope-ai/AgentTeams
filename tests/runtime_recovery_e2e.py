"""Opt-in #1631 live acceptance; only the named disposable installation.

Run with a real AGENTTEAMS_LLM_API_KEY. Never discovers or changes a default
installation. Runtime/model execution evidence is separate from setup readiness.
"""
import json
import base64
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time
import urllib.request

PREFIX = "recovery-1631-live-20261116"
CONTROLLER = PREFIX + "-controller"
NETWORK = PREFIX + "-net"
STATE = Path("/tmp/recovery-1631-e2e-state.json")
CORE_IMAGE = "recovery-1631/controller:local-20261116205000"
HARNESS_IMAGES = {runtime: "recovery-1631/" + runtime + ":local-20261116205000" for runtime in ("codex", "hermes")}


def docker(*args, text=None):
    result = subprocess.run(["docker", *args], input=text, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError("docker " + args[0] + " failed: " + result.stderr[:2048])
    return result.stdout.strip()


def agt(*args, text=None):
    return docker("exec", "-i", CONTROLLER, "agt", *args, text=text)


def state():
    return json.loads(STATE.read_text())


def api(method, path, body=None):
    token = docker("exec", CONTROLLER, "cat", "/var/run/agentteams/cli-token")
    # OrbStack may rebind dynamically published ports on network attachment.
    port = docker("port", CONTROLLER, "8090/tcp").rsplit(":", 1)[1]
    request = urllib.request.Request("http://127.0.0.1:" + port + "/api/v1/" + path,
        data=json.dumps(body).encode() if body is not None else None, method=method,
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def tracing():
    # Reuse the running local backend without changing its configuration or data.
    # Only our disposable Collector and Controller join its network.
    original = json.loads(docker("inspect", "agentteams-otel-collector"))[0]
    env = dict(item.split("=", 1) for item in original["Config"]["Env"])
    public, secret = base64.b64decode(env["LANGFUSE_BASIC_AUTH"]).decode().split(":", 1)
    collector = PREFIX + "-collector"
    if subprocess.run(["docker", "inspect", collector], capture_output=True).returncode:
        config = next(m["Source"] for m in original["Mounts"] if m["Destination"] == "/etc/otelcol-contrib/config.yaml")
        args = ["run", "-d", "--name", collector, "--network", NETWORK,
                "--label", "io.agentteams.recovery.owner=1631", "-v", config + ":/etc/otelcol-contrib/config.yaml:ro",
                "-v", PREFIX + "-traces:/var/lib/otelcol"]
        for key in ("LANGFUSE_BASIC_AUTH", "LANGFUSE_OTLP_BASE_URL"):
            args.extend(["-e", key + "=" + env[key]])
        docker(*args, original["Config"]["Image"], "--config=/etc/otelcol-contrib/config.yaml")
    for container in (CONTROLLER, collector):
        networks = json.loads(docker("inspect", container))[0]["NetworkSettings"]["Networks"]
        if "agentteams-net" not in networks:
            docker("network", "connect", "agentteams-net", container)
    if json.loads(docker("inspect", collector))[0]["State"]["Status"] != "running":
        docker("run", "--rm", "--entrypoint", "/bin/sh", "-v", PREFIX + "-traces:/traces", CORE_IMAGE,
               "-c", "chown 10001:10001 /traces")
        docker("start", collector)
    previous = api("GET", "observability/config")
    saved = api("PUT", "observability/config", {"revision": previous.get("revision"), "backend": "langfuse",
        "enabled": True, "captureContent": False, "mode": "collector", "langfuseUrl": "http://agentteams-langfuse:3000",
        "publicUrl": "http://127.0.0.1:13100", "publicKey": public, "secretKey": secret,
        "collectorEndpoint": "http://" + collector + ":4318"})
    result = api("POST", "observability/apply", {"revision": saved["revision"], "workers": ["leader"]})
    # Apply currently supports Hermes/QwenPaw; Codex receives the same ingestion
    # settings through its Worker spec, without any backend query credentials.
    worker = api("GET", "workers/agent-a")
    worker_env = worker.get("env", {})
    worker_env.update(AGENTTEAMS_OTEL_ENABLED="true", AGENTTEAMS_OTEL_CAPTURE_CONTENT="false",
        AGENTTEAMS_OTEL_CONFIG_REVISION=saved["revision"], OTEL_EXPORTER_OTLP_ENDPOINT="http://" + collector + ":4318",
        OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="http://" + collector + ":4318/v1/traces", OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf")
    api("PUT", "workers/agent-a", {"env": worker_env})
    print(json.dumps({"configured": True, "revision": saved["revision"], "apply": result,
                      "acceptance": "real trace ingestion and readback not yet verified"}))


def seed():
    task_id, main_id, team = "live-codex-hermes-01", "live-main-01", "recovery-team"
    room = api("GET", "teams/" + team)["teamRoomID"]
    if subprocess.run(["docker", "exec", CONTROLLER, "mc", "stat",
            "agentteams/recovery-1631/teams/" + team + "/shared/tasks/" + task_id + "/meta.json"], capture_output=True).returncode == 0:
        raise RuntimeError("Fixture Task already exists; inspect instead of overwriting")
    main = {"task_id": main_id, "task_kind": "main", "team_id": team, "status": "in_progress", "room_id": room,
        "title": "Runtime recovery live acceptance", "dispatch_mode": "manual", "plan_type": "dag",
        "tasks": [{"task_id": task_id, "parent_task_id": main_id, "status": "assigned"}]}
    task = {"task_id": task_id, "main_task_id": main_id, "parent_task_id": main_id, "team_id": team,
        "task_kind": "subtask", "title": "Run three stable steps across Codex to Hermes", "status": "assigned",
        "assigned_to": "@agent-a:recovery-1631.test", "room_id": room, "notifyTxn": "live-assignment-01"}
    spec = f"""You are executing a bounded real runtime-recovery acceptance task {task_id} in team {team}.
ACK this task using taskflow.ack_task and confirm synced=true. Keep its stable task ID.
Pull acceptance.py from $AGENTTEAMS_STORAGE_PREFIX/teams/{team}/shared/tasks/{task_id}/acceptance.py into your task scratch directory using mc cp.
Use your native terminal tool to run python3 acceptance.py {task_id} {team}. Each invocation executes ONE real persisted side effect and commits its checkpoint through the real Controller.
Read the authoritative completed_steps. Never repeat a completed step or edit the fixture, ledger or checkpoint outside the helper.
In execution generation 1, run only step one. Then call the helper once more to verify PAUSED_AT_STEP_BOUNDARY and end your turn with a short progress report. Leave task status in_progress. Do not submit, wait, poll or request a new message.
When recovered in generation 2 or later, continue remaining steps two and three with the same helper, then submit this same task using taskflow.submit_task with status success, summary, 3 conclusions and the output/step-one.json, output/step-two.json, output/step-three.json deliverables.
Outputs are durable in the task's shared object-storage output/ directory; pull them to your task output/ directory before submission. Do not create a new task or send messages to other members.
The external test driver will replace your container at a step boundary without sending any new Matrix message.
"""
    objects = {main_id + "/meta.json": json.dumps(main), task_id + "/meta.json": json.dumps(task),
        task_id + "/spec.md": spec, task_id + "/attempts.json": "[]",
        task_id + "/acceptance.py": (Path(__file__).parent / "fixtures/runtime_recovery_step.py").read_text()}
    for suffix, value in objects.items():
        docker("exec", "-i", CONTROLLER, "mc", "pipe", "agentteams/recovery-1631/teams/" + team + "/shared/tasks/" + suffix, text=value)
    event = {"kind": "task_assigned", "main_task_id": main_id, "task_id": task_id,
             "delivery_id": task["notifyTxn"], "text": spec}
    code = """import json,os,pathlib,urllib.request,sys
token=pathlib.Path(os.environ['AGENTTEAMS_AUTH_TOKEN_FILE']).read_text().strip()
request=urllib.request.Request(os.environ['AGENTTEAMS_CONTROLLER_URL']+'/api/v1/coordination/events',data=sys.stdin.buffer.read(),headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
with urllib.request.urlopen(request,timeout=20) as response: print(response.read().decode())
"""
    receipt = docker("exec", "-i", PREFIX + "-worker-leader", "python3", "-c", code, text=json.dumps(event))
    print(json.dumps({"task_id": task_id, "main_task_id": main_id, "room_id": room, "notification": json.loads(receipt)}))


def refresh_controller():
    original = json.loads(docker("inspect", CONTROLLER))[0]
    if original["Config"]["Labels"].get("io.agentteams.recovery.owner") != "1631":
        raise RuntimeError("Only this fixture's labelled disposable Controller may be replaced")
    args = ["run", "-d", "--name", CONTROLLER, "--network", NETWORK,
            "--label", "io.agentteams.recovery.owner=1631"]
    for port, bindings in original["NetworkSettings"]["Ports"].items():
        for binding in bindings or []:
            args.extend(["-p", binding["HostIp"] + ":" + binding["HostPort"] + ":" + port])
    for bind in original["HostConfig"]["Binds"]:
        args.extend(["-v", bind])
    for value in original["Config"]["Env"]:
        args.extend(["-e", value])
    docker("stop", "--timeout", "15", CONTROLLER)
    if json.loads(docker("inspect", CONTROLLER))[0]["State"]["Running"]:
        raise RuntimeError("Old disposable Controller is still running")
    docker("rm", CONTROLLER)
    docker(*args, CORE_IMAGE)
    docker("network", "connect", "agentteams-net", CONTROLLER)
    print(json.dumps({"controller_image": CORE_IMAGE, "replaced_only_owned_fixture": True}))


def setup():
    if STATE.exists():
        raise RuntimeError("This disposable setup already has a state file; inspect it instead of replacing it")
    if not os.getenv("AGENTTEAMS_LLM_API_KEY"):
        raise RuntimeError("A real AGENTTEAMS_LLM_API_KEY is required")
    if subprocess.run(["docker", "inspect", CONTROLLER], capture_output=True).returncode == 0:
        raise RuntimeError("The disposable container name already exists; refusing to replace it")
    password = secrets.token_urlsafe(24)
    registration = secrets.token_urlsafe(24)
    minio_secret = secrets.token_urlsafe(24)
    env = {
        "AGENTTEAMS_KUBE_MODE": "embedded", "AGENTTEAMS_RUNTIME": "docker",
        "AGENTTEAMS_WORKER_BACKEND": "docker", "AGENTTEAMS_DOCKER_NETWORK": NETWORK,
        "AGENTTEAMS_RESOURCE_PREFIX": PREFIX + "-", "AGENTTEAMS_RESOURCE_AUTOPREFIX": "true",
        "AGENTTEAMS_CONTROLLER_NAME": PREFIX, "AGENTTEAMS_MANAGER_ENABLED": "false",
        "AGENTTEAMS_ADMIN_USER": "recovery-admin", "AGENTTEAMS_ADMIN_PASSWORD": password,
        "AGENTTEAMS_REGISTRATION_TOKEN": registration, "AGENTTEAMS_MINIO_USER": "recovery-minio",
        "AGENTTEAMS_MINIO_PASSWORD": minio_secret, "AGENTTEAMS_FS_ENDPOINT": "http://127.0.0.1:9000",
        "AGENTTEAMS_FS_ACCESS_KEY": "recovery-minio", "AGENTTEAMS_FS_SECRET_KEY": minio_secret,
        "AGENTTEAMS_FS_BUCKET": "recovery-1631", "AGENTTEAMS_STORAGE_PREFIX": "agentteams/recovery-1631",
        "AGENTTEAMS_LLM_PROVIDER": "qwen", "AGENTTEAMS_DEFAULT_MODEL": "qwen3.6-plus",
        "AGENTTEAMS_DEFAULT_WORKER_RUNTIME": "codex", "AGENTTEAMS_MATRIX_E2EE": "0",
        "AGENTTEAMS_CONTROLLER_URL": "http://" + CONTROLLER + ":8090",
        "AGENTTEAMS_AI_GATEWAY_URL": "http://" + CONTROLLER + ":8080",
        "AGENTTEAMS_MATRIX_URL": "http://127.0.0.1:6167",
        "AGENTTEAMS_MATRIX_DOMAIN": "recovery-1631.test",
        "AGENTTEAMS_MATRIX_APPSERVICE_AS_TOKEN": secrets.token_urlsafe(32),
        "AGENTTEAMS_MATRIX_APPSERVICE_HS_TOKEN": secrets.token_urlsafe(32),
    }
    # This Mac's default Docker pools are exhausted by existing installations.
    # Use the separately inspected, non-overlapping disposable test subnet.
    docker("network", "create", "--subnet", "10.231.163.0/24", NETWORK)
    docker("volume", "create", PREFIX + "-data")
    args = ["run", "-d", "--name", CONTROLLER, "--network", NETWORK,
            "--label", "io.agentteams.recovery.owner=1631", "-p", "127.0.0.1::8090",
            "-p", "127.0.0.1::6167", "-p", "127.0.0.1::8080",
            "-v", PREFIX + "-data:/data", "-v", "/var/run/docker.sock:/var/run/docker.sock",
            "-e", "AGENTTEAMS_LLM_API_KEY"]
    for key, value in env.items():
        args.extend(["-e", key + "=" + value])
    docker(*args, CORE_IMAGE)
    ports = {}
    for name, port in {"controller": 8090, "matrix": 6167, "gateway": 8080}.items():
        host_port = docker("port", CONTROLLER, str(port) + "/tcp").rsplit(":", 1)[1]
        ports[name] = "http://127.0.0.1:" + host_port
    data = {"prefix": PREFIX, "core_image": CORE_IMAGE, "ports": ports,
            "admin_user": "recovery-admin", "admin_password": password,
            "started_at": time.time(), "real_model_configured": True}
    with os.fdopen(os.open(STATE, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as stream:
        json.dump(data, stream, indent=2)
    print(json.dumps({"container": CONTROLLER, "ports": ports, "state": str(STATE)}))


def status():
    data = state()
    result = {}
    for name, path in {"controller": "/healthz", "matrix": "/_matrix/client/versions"}.items():
        try:
            with urllib.request.urlopen(data["ports"][name] + path, timeout=5) as response:
                result[name] = response.status
        except OSError as error:
            result[name] = type(error).__name__
    try:
        result["workers"] = json.loads(agt("get", "workers", "-o", "json"))
    except RuntimeError:
        result["workers"] = "not-ready"
    print(json.dumps(result, ensure_ascii=False))


def members():
    # This creates resources only through this fixture's Controller CLI.
    # The existing installation and its task graph are never selected.
    for name, runtime in (("agent-a", "codex"), ("leader", "hermes")):
        manifest = {"apiVersion": "agentteams.io/v1beta1", "kind": "Worker",
            "metadata": {"name": name}, "spec": {"workerName": name, "runtime": runtime,
                "model": "qwen3.6-plus", "image": HARNESS_IMAGES[runtime]}}
        if runtime == "codex":
            # The fixture runs only inside our disposable container; its
            # deterministic step helper needs the Controller/storage network.
            manifest["spec"]["env"] = {"CODEX_SANDBOX_MODE": "danger-full-access"}
        agt("apply", "-f", "/dev/stdin", text=json.dumps(manifest))
    team = {"apiVersion": "agentteams.io/v1beta1", "kind": "Team", "metadata": {"name": "recovery-team"},
        "spec": {"teamName": "recovery-team", "admin": {"name": "recovery-admin"},
            "workerMembers": [{"name": "leader", "role": "team_leader"}, {"name": "agent-a", "role": "worker"}]}}
    agt("apply", "-f", "/dev/stdin", text=json.dumps(team))
    print("Created isolated Agent A and Leader; model/task execution not yet verified")


if __name__ == "__main__":
    {"setup": setup, "status": status, "members": members, "tracing": tracing, "seed": seed,
     "refresh-controller": refresh_controller}[sys.argv[1]]()
