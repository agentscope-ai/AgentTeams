"""Provision brand-new Matrix accounts/storage and boot standard Worker entrypoints."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen

from run_live_acceptance import isolated_model, docker

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = ROOT / "output/observability-demo"
STATE = OUTPUT / "matrix-private.json"
MATRIX = "http://127.0.0.1:16167"
PASSWORD = "AT-isolated-tracing-demo-2026"


def ensure_matrix():
    exists = subprocess.run(["docker", "inspect", "at-tracing-demo-matrix"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
    if not exists:
        docker("run", "-d", "--name", "at-tracing-demo-matrix", "--restart", "unless-stopped", "--network", "at-tracing-demo_default",
            "-p", "127.0.0.1:16167:6167", "-v", "at-tracing-demo_matrix:/data",
            "-e", "CONDUWUIT_SERVER_NAME=tracing.local", "-e", "CONDUWUIT_DATABASE_PATH=/data/tuwunel",
            "-e", 'CONDUWUIT_ADDRESS=["0.0.0.0"]', "-e", "CONDUWUIT_PORT=6167",
            "-e", "CONDUWUIT_ALLOW_REGISTRATION=true", "-e", "CONDUWUIT_REGISTRATION_TOKEN=at-tracing-registration-only",
            "-e", "CONDUWUIT_ALLOW_LEGACY_MEDIA=true", "-e", "CONDUWUIT_ALLOW_UNSTABLE_ROOM_VERSIONS=true",
            "-e", "CONDUWUIT_NEW_USER_DISPLAYNAME_SUFFIX=", "--entrypoint", "/usr/local/bin/tuwunel",
            "agentteams/agentteams-embedded:local-20261104120000")
    else:
        docker("start", "at-tracing-demo-matrix")
    deadline = time.monotonic() + 30
    while True:
        try:
            with urlopen(MATRIX + "/_matrix/client/versions", timeout=2):
                return
        except OSError:
            if time.monotonic() >= deadline:
                raise RuntimeError("Isolated Matrix did not become ready") from None
            time.sleep(1)


def matrix(method, path, payload=None, token=""):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = Request(MATRIX + "/_matrix/client/v3" + path,
                      data=json.dumps(payload).encode() if payload is not None else None, method=method, headers=headers)
    try:
        with urlopen(request, timeout=30) as response:
            return json.load(response)
    except HTTPError as exc:
        body = json.load(exc)
        raise RuntimeError(f"Matrix {exc.code}: {body.get('errcode')} {body.get('error')}") from None


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--new-room", action="store_true", help="Start a clean acceptance session, retaining earlier traces")
    args = parser.parse_args()
    ensure_matrix()
    OUTPUT.mkdir(parents=True, exist_ok=True)
    if STATE.exists():
        saved = json.loads(STATE.read_text()); accounts = saved["accounts"]; room = saved["room_id"]
    else:
        accounts = {}
        for name in ("trace-human", "trace-qwenpaw", "trace-hermes"):
            accounts[name] = matrix("POST", "/register", {"username": name, "password": PASSWORD,
                "auth": {"type": "m.login.registration_token", "token": "at-tracing-registration-only"}})
        room = matrix("POST", "/createRoom", {"name": "AT full tracing acceptance", "preset": "private_chat",
            "invite": [accounts[n]["user_id"] for n in ("trace-qwenpaw", "trace-hermes")]}, accounts["trace-human"]["access_token"])["room_id"]
        for name in ("trace-qwenpaw", "trace-hermes"):
            matrix("POST", f"/rooms/{quote(room, safe='')}/join", {}, accounts[name]["access_token"])
        with open(STATE, "w", opener=lambda path, flags: os.open(path, flags, 0o600)) as file:
            json.dump({"room_id": room, "accounts": accounts}, file)
    human = accounts["trace-human"]
    if args.new_room:
        room = matrix("POST", "/createRoom", {"name": "AT full session acceptance", "preset": "private_chat",
            "invite": [accounts[n]["user_id"] for n in ("trace-qwenpaw", "trace-hermes")]}, human["access_token"])["room_id"]
        for name in ("trace-qwenpaw", "trace-hermes"):
            matrix("POST", f"/rooms/{quote(room, safe='')}/join", {}, accounts[name]["access_token"])
        STATE.write_text(json.dumps({"room_id": room, "accounts": accounts}))
    base_url, key, model = isolated_model()
    seed = OUTPUT / "matrix-seed"
    seed.mkdir(exist_ok=True)
    for runtime in ("qwenpaw", "hermes"):
        name = f"trace-{runtime}"
        account = accounts[name]
        directory = seed / "agents" / name
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "SOUL.md").write_text("You are an isolated tracing acceptance Worker. Follow user instructions and use real tools.\n")
        (directory / "AGENTS.md").write_text("You work only in your own /tmp or home workspace. Never inspect runtime session or credential files.\n")
        (directory / "skills").mkdir(exist_ok=True)
        channel = {"enabled": True, "homeserver": "http://at-tracing-demo-matrix:6167", "userId": account["user_id"],
            "accessToken": account["access_token"], "deviceId": account["device_id"], "encryption": False,
            "dm": {"policy": "allowlist", "allowFrom": [a["user_id"] for a in accounts.values()]},
            "groupPolicy": "allowlist", "groupAllowFrom": [a["user_id"] for a in accounts.values()],
            "requireMention": True, "autoThread": False}
        if runtime == "hermes":
            config = {"channels": {"matrix": channel}, "models": {"providers": {"agentteams-gateway": {
                "baseUrl": base_url, "apiKey": key, "models": [{"id": model, "name": model, "input": ["text"]}]}}},
                "agents": {"defaults": {"model": {"primary": "agentteams-gateway/" + model}}}}
            path = directory / "openclaw.json"
        else:
            config = {"apiVersion": "agentteams.io/v1beta1", "kind": "MemberRuntimeConfig", "metadata": {"generation": 1},
                "team": {"name": "tracing-team", "teamRoomId": room, "admin": {"name": "trace-human", "matrixUserId": human["user_id"]},
                         "members": [{"name": n, "runtimeName": n, "role": "worker", "matrixUserId": accounts[n]["user_id"]}
                                     for n in ("trace-qwenpaw", "trace-hermes")]},
                "member": {"name": name, "runtimeName": name, "runtime": runtime, "role": "worker", "matrixUserId": account["user_id"]},
                "desired": {"model": {"provider": "agentteams-gateway", "model": model, "baseUrl": base_url, "input": ["text"]},
                            "channels": {"matrix": {"enabled": True, "filter_tool_messages": True, "filter_thinking": True}},
                            "channelPolicy": {"allowedChannels": ["matrix"]}},
                "storage": {"memberPrefix": "agents/" + name, "sharedPrefix": "shared"},
                "credentials": {"matrixTokenEnv": "AGENTTEAMS_WORKER_MATRIX_TOKEN", "gatewayKeyEnv": "AGENTTEAMS_WORKER_GATEWAY_KEY"}}
            path = directory / "runtime/runtime.yaml"
            path.parent.mkdir(exist_ok=True)
        with open(path, "w", opener=lambda p, f: os.open(p, f, 0o600)) as file:
            json.dump(config, file)  # JSON is also valid YAML.
        task_dir = seed / "shared/tasks" / f"matrix-{runtime}-acceptance"
        task_dir.mkdir(parents=True, exist_ok=True)
        (task_dir / "meta.json").write_text(json.dumps({"task_id": f"matrix-{runtime}-acceptance",
            "project_id": "tracing-demo", "room_id": room, "assigned_to": name, "status": "in_progress"}))
    # Seed only the new demo bucket using the cached mc binary.
    subprocess.run(["docker", "run", "--rm", "--network", "at-tracing-demo_default", "-v", f"{seed}:/seed:ro",
        "-e", "MC_HOST_demo=http://minio:tracing-local@minio:9000", "--entrypoint", "/bin/sh",
        "agentteams/agentteams-embedded:local-20261104120000", "-c",
        "mc mb --ignore-existing demo/at-worker-tracing-demo && mc cp --recursive /seed/ demo/at-worker-tracing-demo/"],
        check=True, stdout=subprocess.DEVNULL)
    for runtime in ("qwenpaw", "hermes"):
        name = f"trace-{runtime}"
        container = f"at-tracing-worker-{runtime}"
        existing = subprocess.run(["docker", "inspect", container], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        if existing.returncode == 0:
            if json.loads(existing.stdout)[0]["State"]["Running"]:
                docker("exec", "-e", "MC_HOST_trace=http://minio:tracing-local@minio:9000", container,
                    "/usr/local/bin/mc.bin", "mirror", "--overwrite", "trace/at-worker-tracing-demo/shared",
                    "/root/agentteams-fs/shared" if runtime == "qwenpaw" else f"/root/agentteams-fs/agents/{name}/shared")
                continue
            docker("rm", container)
        env = {"AGENTTEAMS_WORKER_NAME": name, "AGENTTEAMS_TEAM_NAME": "tracing-team", "AGENTTEAMS_RUNTIME": "local",
            "HOME": f"/root/agentteams-fs/agents/{name}", "AGENTTEAMS_WORKER_HOME": f"/root/agentteams-fs/agents/{name}",
            "AGENTTEAMS_FS_ENDPOINT": "http://minio:9000", "AGENTTEAMS_FS_ACCESS_KEY": "minio",
            "AGENTTEAMS_FS_SECRET_KEY": "tracing-local", "AGENTTEAMS_FS_BUCKET": "at-worker-tracing-demo",
            "AGENTTEAMS_WORKER_GATEWAY_KEY": key, "AGENTTEAMS_AI_GATEWAY_URL": base_url.removesuffix("/v1"),
            "AGENTTEAMS_MATRIX_URL": "http://at-tracing-demo-matrix:6167", "AGENTTEAMS_MATRIX_DOMAIN": "tracing.local",
            "AGENTTEAMS_WORKER_MATRIX_TOKEN": accounts[name]["access_token"], "AGENTTEAMS_OTEL_ENABLED": "true",
            "AGENTTEAMS_OTEL_CAPTURE_CONTENT": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
            "AGENTTEAMS_PORT_GATEWAY": "8080", "AGENTTEAMS_MATRIX_E2EE": "false"}
        with tempfile.NamedTemporaryFile(mode="w", prefix="at-worker-", suffix=".env") as file:
            file.write("\n".join(f"{k}={v}" for k, v in env.items())); file.flush()
            command = ["create", "--name", f"at-tracing-worker-{runtime}", "--restart", "unless-stopped",
                "--network", "at-tracing-demo_default", "--env-file", file.name]
            if runtime == "qwenpaw":
                command.extend(["-p", "127.0.0.1:13088:8088"])
            command.append(f"agentteams/{runtime}-worker:local-20261106023000-full-otel")
            docker(*command)
        docker("start", f"at-tracing-worker-{runtime}")
    print(json.dumps({"room_id": room, "workers": ["trace-qwenpaw", "trace-hermes"], "matrix_url": MATRIX}))


if __name__ == "__main__":
    main()
