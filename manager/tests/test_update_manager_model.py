"""Exercise update-manager-model.sh without a running AI Gateway / OpenClaw.

The script's real entry point runs a connectivity pre-flight against
``${AGENTTEAMS_AI_GATEWAY_URL}/v1/chat/completions`` and then patches
``openclaw.json`` with jq. These tests keep the pre-flight out of the way and
assert on the resulting ``openclaw.json``, so they need only bash + jq.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "agent/skills/model-switch/scripts/update-manager-model.sh"
PREFLIGHT_START = "# ── Pre-flight"
PREFLIGHT_END = "TMP=$(mktemp)"


def _script_without_preflight() -> str:
    source = SCRIPT.read_text()
    start = source.index(PREFLIGHT_START)
    end = source.index(PREFLIGHT_END)
    # Drop the connectivity probe (needs a live gateway) and the shared env
    # library (only provides log()); keep everything else verbatim.
    return (source[:start] + source[end:]).replace(
        "source /opt/agentteams/scripts/lib/agentteams-env.sh\n",
        'log() { echo "$*" >&2; }\n',
    )


@unittest.skipUnless(shutil.which("jq"), "jq is required to patch openclaw.json")
class UpdateManagerModelTest(unittest.TestCase):
    def switch(self, model, *extra, existing_model=True):
        """Run the OpenClaw path of update-manager-model.sh in a temp HOME."""
        script = _script_without_preflight()
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            workspace = home / "manager-workspace"
            workspace.mkdir()
            config = workspace / "openclaw.json"
            models = (
                [{
                    "id": "my-model",
                    "name": "my-model",
                    "reasoning": False,
                    "contextWindow": 150000,
                    "maxTokens": 128000,
                    "input": ["text"],
                }]
                if existing_model
                else []
            )
            config.write_text(json.dumps({
                "models": {"providers": {"agentteams-gateway": {"models": models}}},
                "agents": {"defaults": {"model": {"primary": "agentteams-gateway/old"},
                                        "models": {}}},
            }, indent=2))

            env = dict(os.environ)
            env["HOME"] = str(home)
            env["AGENTTEAMS_MANAGER_RUNTIME"] = "openclaw"
            env.pop("AGENTTEAMS_AI_GATEWAY_URL", None)
            result = subprocess.run(
                ["bash", "-s", "--", model, *extra],
                input=script, capture_output=True, text=True, env=env,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            return json.loads(config.read_text()), result.stderr

    def entry(self, config, model="my-model"):
        models = config["models"]["providers"]["agentteams-gateway"]["models"]
        return next(m for m in models if m["id"] == model)

    def test_context_window_override_applies_to_existing_model(self):
        # Regression: the pre-existing branch only updated `reasoning`, so a
        # --context-window correction for a model already in the array (e.g. one
        # first added with the 150000 fallback) was silently discarded.
        config, _ = self.switch("my-model", "--context-window", "300000")
        self.assertEqual(self.entry(config)["contextWindow"], 300000)

    def test_context_window_override_applies_to_new_model(self):
        config, _ = self.switch("my-model", "--context-window", "300000",
                                existing_model=False)
        self.assertEqual(self.entry(config)["contextWindow"], 300000)

    def test_reported_context_window_matches_stored_value(self):
        # The startup log line must not claim a window the file does not get.
        config, stderr = self.switch("my-model", "--context-window", "300000")
        logged = next(l for l in stderr.splitlines() if l.startswith("Updating Manager model:"))
        self.assertIn("ctx=300000", logged)
        self.assertEqual(self.entry(config)["contextWindow"], 300000)

    def test_no_reasoning_flag_still_applies(self):
        config, _ = self.switch("my-model", "--no-reasoning")
        self.assertFalse(self.entry(config)["reasoning"])

    def test_reasoning_defaults_to_true(self):
        config, _ = self.switch("my-model")
        self.assertTrue(self.entry(config)["reasoning"])

    def test_existing_model_fields_refresh_from_catalog(self):
        # A model already listed with stale values is brought back in line with
        # the script's own table instead of keeping the stale entry.
        config, _ = self.switch("qwen3.6-plus", existing_model=False)
        stale = self.entry(config, "qwen3.6-plus")
        self.assertEqual((stale["contextWindow"], stale["maxTokens"]), (200000, 64000))
        self.assertEqual(stale["input"], ["text", "image"])

        config, _ = self.switch("qwen3.6-plus", existing_model=True)
        refreshed = self.entry(config, "qwen3.6-plus")
        self.assertEqual((refreshed["contextWindow"], refreshed["maxTokens"]), (200000, 64000))
        self.assertEqual(refreshed["input"], ["text", "image"])

    def test_switch_always_updates_primary_and_alias(self):
        for existing in (True, False):
            with self.subTest(existing_model=existing):
                config, _ = self.switch("my-model", existing_model=existing)
                defaults = config["agents"]["defaults"]
                self.assertEqual(defaults["model"]["primary"], "agentteams-gateway/my-model")
                self.assertEqual(defaults["models"]["agentteams-gateway/my-model"],
                                 {"alias": "my-model"})

    def test_switch_keeps_other_models_untouched(self):
        script = _script_without_preflight()
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            workspace = home / "manager-workspace"
            workspace.mkdir()
            config = workspace / "openclaw.json"
            config.write_text(json.dumps({
                "models": {"providers": {"agentteams-gateway": {"models": [
                    {"id": "my-model", "name": "my-model", "reasoning": False,
                     "contextWindow": 150000, "maxTokens": 128000, "input": ["text"]},
                    {"id": "other-model", "name": "other-model", "reasoning": True,
                     "contextWindow": 999999, "maxTokens": 1, "input": ["text", "image"]},
                ]}}},
                "agents": {"defaults": {"model": {"primary": "agentteams-gateway/old"},
                                        "models": {}}},
            }, indent=2))
            env = dict(os.environ)
            env["HOME"] = str(home)
            env["AGENTTEAMS_MANAGER_RUNTIME"] = "openclaw"
            result = subprocess.run(
                ["bash", "-s", "--", "my-model", "--context-window", "300000"],
                input=script, capture_output=True, text=True, env=env,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            data = json.loads(config.read_text())
            by_id = {m["id"]: m for m in data["models"]["providers"]["agentteams-gateway"]["models"]}
            self.assertEqual(by_id["other-model"]["contextWindow"], 999999)
            self.assertEqual(by_id["other-model"]["maxTokens"], 1)
            self.assertEqual(by_id["other-model"]["input"], ["text", "image"])


if __name__ == "__main__":
    unittest.main()
