"""Reuse only the explicitly authorized local Worker's model connection."""

import argparse
import json
from pathlib import Path
import subprocess
import tempfile


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def authorized_model():
    source = json.loads(docker("inspect", "atlocal-20261104-worker-smoke"))[0]
    source_env = dict(item.split("=", 1) for item in source["Config"]["Env"])
    key = source_env["AGENTTEAMS_WORKER_GATEWAY_KEY"]
    model = json.loads(docker("exec", "atlocal-20261104-worker-smoke", "/opt/venv/qwenpaw/bin/python", "-c",
        "import json; print(json.dumps(json.load(open('/root/agentteams-fs/agents/smoke/.qwenpaw/workspaces/default/agent.json'))['active_model']))"))["model"]
    return key, model


def isolated_model():
    key, model = authorized_model()
    name = "at-tracing-model-proxy"
    exists = subprocess.run(["docker", "inspect", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
    if not exists:
        script = Path(__file__).resolve().parent / "model_proxy.py"
        with tempfile.NamedTemporaryFile(mode="w", prefix="at-proxy-", suffix=".env") as file:
            file.write(f"AUTHORIZED_MODEL={model}\nAUTHORIZED_MODEL_KEY={key}\n"); file.flush()
            docker("run", "-d", "--name", name, "--restart", "unless-stopped", "--network", "at-tracing-demo_default",
                "--env-file", file.name, "-v", f"{script}:/demo/model_proxy.py:ro", "--entrypoint", "/opt/venv/qwenpaw/bin/python",
                "agentteams/qwenpaw-worker:local-20261106003000-otel", "/demo/model_proxy.py")
    else:
        docker("start", name)
    return "http://at-tracing-model-proxy:8080/v1", "at-isolated-model-proxy", model


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("runtime", choices=["qwenpaw", "hermes"])
    args = parser.parse_args()
    runtime = args.runtime
    root = Path(__file__).resolve().parents[2]
    base_url, key, model = isolated_model()
    name = f"at-tracing-full-{runtime}"
    env = {"DEMO_LLM_BASE_URL": base_url,
           "DEMO_LLM_API_KEY": key, "DEMO_LLM_MODEL": model,
           "AGENTTEAMS_OTEL_ENABLED": "true", "AGENTTEAMS_OTEL_RUNTIME": runtime,
           "AGENTTEAMS_WORKER_NAME": f"{runtime}-full-session", "AGENTTEAMS_OTEL_CAPTURE_CONTENT": "true",
           "LOONGSUITE_PYTHON_SITE_BOOTSTRAP": "false", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
           "PYTHONPATH": "/opt/agentteams/scripts/lib/telemetry"}
    # NamedTemporaryFile is mode 0600; neither credentials nor the legacy env are logged.
    with tempfile.NamedTemporaryFile(mode="w", prefix="at-model-", suffix=".env") as file:
        file.write("\n".join(f"{k}={v}" for k, v in env.items()))
        file.flush()
        docker("create", "--name", name, "--network", "at-tracing-demo_default", "--env-file", file.name,
               "-v", f"{root / 'examples/observability'}:/demo:ro", "--entrypoint", f"/opt/venv/{runtime}/bin/python",
               f"agentteams/{runtime}-worker:local-20261106023000-full-otel", "/demo/full_session_demo.py")
    print(f"Running {runtime} with authorized model {model}; only isolated demo files are modified.", flush=True)
    subprocess.run(["docker", "start", "-a", name], check=True)
    code = int(docker("inspect", name, "--format", "{{.State.ExitCode}}"))
    if code:
        raise SystemExit(code)


if __name__ == "__main__":
    main()
