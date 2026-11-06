"""Run the built tracing view with isolated local demo credentials only."""

import argparse
import json
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("source", help="Dashboard worktree with npm run build completed")
    parser.add_argument("--image", default="agentteams/agentteams-dashboard:local-20261104120000")
    args = parser.parse_args()
    source = Path(args.source).resolve() / ".next" / "standalone"
    if not (source / "server.js").is_file():
        raise SystemExit("Build the Dashboard first: npm run build")
    env = {
        "LANGFUSE_BASE_URL": "http://langfuse:3000",
        "LANGFUSE_PUBLIC_URL": "http://localhost:13100",
        "LANGFUSE_PUBLIC_KEY": "pk-lf-at-local-demo",
        "LANGFUSE_SECRET_KEY": "sk-lf-at-local-demo",
        "LANGFUSE_PROJECT_ID": "at-workers",
        "AGENTTEAMS_TRACING_DEMO": "true",
        "AGENTTEAMS_AUTH_DISABLED": "true",
        "AGENTTEAMS_TRACING_DEMO_SCOPE": json.dumps({
            "teams": [{"name": "tracing-team", "label": "tracing-team"}],
            "workers": [{"name": name, "team": "tracing-team", "runtime": runtime}
                        for name, runtime in (("trace-qwenpaw", "qwenpaw"), ("trace-hermes", "hermes"))],
        }),
        "DASHBOARD_CONFIG_FILE": "/data/config.json",
        "DASHBOARD_BUILD_ID": (source / ".next" / "BUILD_ID").read_text().strip(),
        "HOSTNAME": "0.0.0.0", "PORT": "3000",
    }
    command = ["docker", "run", "-d", "--name", "at-tracing-demo-dashboard",
               "--restart", "unless-stopped", "--network", "at-tracing-demo_default",
               "-p", "127.0.0.1:13003:3000", "-v", f"{source}:/app:ro",
               "--tmpfs", "/data:uid=1001,gid=1001,mode=0700"]
    for key, value in env.items():
        command.extend(["-e", f"{key}={value}"])
    command.extend([args.image, "node", "server.js"])
    subprocess.run(command, check=True, stdout=subprocess.DEVNULL)
    print("Dashboard: http://localhost:13003/tracing-demo")


if __name__ == "__main__":
    main()
