"""Validate a Worker record and derive legacy-shaped native bridge input.

The YAML is Controller-owned authority. The generated JSON is a private local
cache for adapters; it must never be uploaded as Worker desired state.
"""
from __future__ import annotations

import argparse
import copy
import json
import os
from pathlib import Path
import tempfile

import yaml


def project(record, harness, worker, compatibility=None):
    if record.get("apiVersion") != "agentteams.io/v1beta1" or record.get("kind") != "MemberRuntimeConfig":
        raise ValueError("Unsupported Worker runtime configuration format")
    member = record.get("member", {})
    if harness not in {"codex", "hermes", "qwenpaw", "deepseek-harness", "openclaw", "openhuman", "opencode", "qoder", "claudecode"}:
        raise ValueError("Unsupported Worker Harness")
    if member.get("runtime") != harness or member.get("runtimeName") != worker:
        raise ValueError("Worker runtime configuration identity does not match this process")
    if record.get("storage", {}).get("memberPrefix") != "agents/" + worker:
        raise ValueError("Worker runtime configuration storage scope does not match")

    bindings = record.get("credentials", {})

    def credential(binding, default, explicit):
        value = os.getenv(bindings.get(binding, default), "").strip() or str(explicit or "").strip()
        if not value:
            raise ValueError("Worker credential binding is unavailable: " + binding)
        return value

    transport = record.get("matrix", {}).get("transport")
    if not isinstance(transport, dict) or not transport.get("homeserver") or not member.get("matrixUserId"):
        raise ValueError("Worker Matrix transport and identity are required")
    matrix = copy.deepcopy(transport)
    matrix.pop("deviceId", None)
    matrix.update(userId=member["matrixUserId"], homeRoomId=member.get("personalRoomId", ""),
                  accessToken=credential("matrixTokenEnv", "AGENTTEAMS_WORKER_MATRIX_TOKEN", record.get("matrix", {}).get("accessToken")))
    output = copy.deepcopy(compatibility or {})
    output.setdefault("channels", {})["matrix"] = matrix

    model = record.get("desired", {}).get("model")
    if model is not None:
        if not model.get("model") or not model.get("gatewayUrl"):
            raise ValueError("Worker model and gateway URL are required")
        endpoint = model["gatewayUrl"].rstrip("/")
        if not endpoint.endswith("/v1"):
            endpoint += "/v1"
        output.setdefault("models", {})["providers"] = {"agentteams-gateway": {
            "baseUrl": endpoint,
            "apiKey": credential("gatewayKeyEnv", "AGENTTEAMS_WORKER_GATEWAY_KEY", model.get("gatewayKey")),
            "models": [{"id": model["model"], "input": model.get("input", ["text"])}],
        }}
        output.setdefault("agents", {}).setdefault("defaults", {}).setdefault("model", {})["primary"] = "agentteams-gateway/" + model["model"]
    elif not output.get("models"):
        raise ValueError("Undeclared Worker model requires external compatibility configuration")
    return output


def materialize(workspace: Path, harness: str, worker: str) -> Path:
    runtime = workspace / "runtime/runtime.yaml"
    record = yaml.safe_load(runtime.read_text())
    compatibility = workspace / "openclaw.json"
    legacy = None
    if record.get("desired", {}).get("model") is None and compatibility.is_file():
        legacy = json.loads(compatibility.read_text())
    output = project(record, harness, worker, legacy)
    target = runtime.parent / "compat-config.json"
    staging = workspace / "credentials"
    staging.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", dir=staging, prefix=".runtime-config-", delete=False) as pending:
        try:
            json.dump(output, pending)
            pending.close()
            os.replace(pending.name, target)
        finally:
            Path(pending.name).unlink(missing_ok=True)
    return target


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--harness", required=True)
    parser.add_argument("--worker-home", type=Path, required=True)
    args = parser.parse_args()
    # Credentials are written only to the private output file, never stdout.
    materialize(args.worker_home, args.harness, os.environ["AGENTTEAMS_WORKER_NAME"])
