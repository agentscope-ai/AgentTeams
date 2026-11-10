"""Exercise installer functions without a container engine or host mutations."""
import os
from pathlib import Path
import re
import subprocess
import unittest


SOURCE = Path(__file__).with_name("agentteams-install.sh").read_text()


def functions(*names):
    return "\n".join(
        re.search(r"^" + name + r"\(\) \{\n.*?^\}", SOURCE, re.M | re.S)[0]
        for name in names
    )


def run(script):
    return subprocess.run(
        ["bash"], input=script, text=True, capture_output=True,
        env={"PATH": os.environ["PATH"], "HOME": "/nonexistent/one-click-test"},
    )


class OneClickInstallTest(unittest.TestCase):
    def test_fresh_install_never_reads_input(self):
        steps = re.search(r"local _STEPS=\((.*?)\)", SOURCE, re.S)[1].split()
        script = functions("should_skip_step", "prompt", "prompt_optional", *steps)
        script += """
log() { :; }
msg() { printf '%s' "$1"; }
read() { echo "Unexpected prompt: $CURRENT_STEP" >&2; exit 99; }
read_secret() { read; }
openssl() { echo generated-password; }
AGENTTEAMS_QUICKSTART=1
AGENTTEAMS_NON_INTERACTIVE=0
AGENTTEAMS_LANGUAGE=zh
AGENTTEAMS_VERSION=v1.2.6
AGENTTEAMS_USE_EMBEDDED=1
AGENTTEAMS_REGISTRY=example.invalid
AGENTTEAMS_ENV_FILE=/nonexistent/one-click-test.env
"""
        script += "\nfor CURRENT_STEP in " + " ".join(steps) + "; do\n"
        script += 'if ! should_skip_step "$CURRENT_STEP"; then "$CURRENT_STEP"; fi\ndone\n'
        script += 'test -z "$AGENTTEAMS_LLM_API_KEY" && test -z "$AGENTTEAMS_EMBEDDING_MODEL" && test "$AGENTTEAMS_DASHBOARD" = 1\n'
        result = run(script)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_environment_model_is_preserved(self):
        result = run(functions("step_llm") + """
log() { :; }
msg() { :; }
AGENTTEAMS_QUICKSTART=1
AGENTTEAMS_LANGUAGE=zh
AGENTTEAMS_LLM_PROVIDER=openai-compat
AGENTTEAMS_OPENAI_BASE_URL=https://example.invalid/v1
AGENTTEAMS_DEFAULT_MODEL=custom-model
AGENTTEAMS_LLM_API_KEY=synthetic-key
AGENTTEAMS_EMBEDDING_MODEL=custom-embedding
step_llm
test "$AGENTTEAMS_LLM_API_KEY" = synthetic-key && test "$AGENTTEAMS_DEFAULT_MODEL" = custom-model && test "$AGENTTEAMS_OPENAI_BASE_URL" = https://example.invalid/v1 && test "$AGENTTEAMS_EMBEDDING_MODEL" = custom-embedding
""")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_upgrade_keep_all_skips_configuration(self):
        result = run(functions("should_skip_step") + """
AGENTTEAMS_UPGRADE=1
AGENTTEAMS_UPGRADE_KEEP_ALL=1
AGENTTEAMS_QUICKSTART=1
for step in step_llm step_admin step_dashboard step_e2ee step_hostshare; do
    should_skip_step "$step" || exit 1
done
""")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_falls_back_to_running_podman(self):
        result = run(functions("container_runtime_ready") + """
docker() { return 1; }
podman() { return 0; }
container_runtime_ready && test "$DOCKER_CMD" = podman
""")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_engine_setup_continues_when_ready(self):
        result = run(functions("check_container_runtime") + """
log() { :; }
msg() { :; }
die() { exit 1; }
uname() { echo Linux; }
open_url() { echo guide-opened; }
sleep() { :; }
attempts=0
container_runtime_ready() { attempts=$((attempts+1)); [ "$attempts" -ge 3 ]; }
check_container_runtime
test "$attempts" = 3
""")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("guide-opened", result.stdout)

    def test_noninteractive_never_opens_install_guide(self):
        result = run(functions("check_container_runtime") + """
AGENTTEAMS_NON_INTERACTIVE=1
container_runtime_ready() { return 1; }
msg() { :; }
die() { exit 7; }
open_url() { echo UNEXPECTED; }
check_container_runtime
""")
        self.assertEqual(result.returncode, 7)
        self.assertNotIn("UNEXPECTED", result.stdout)

    def test_password_goes_to_clipboard_only(self):
        result = run(functions("open_dashboard") + """
log() { :; }
msg() { :; }
AGENTTEAMS_ADMIN_PASSWORD=synthetic-password
AGENTTEAMS_ADMIN_USER=admin
pbcopy() { cat >&2; }
open_url() { echo "$1"; }
open_dashboard
""")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, "synthetic-password")
        self.assertEqual(result.stdout.strip(), "http://127.0.0.1:13000/?username=admin")


if __name__ == "__main__":
    unittest.main()
