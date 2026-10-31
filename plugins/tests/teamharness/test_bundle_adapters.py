"""Static-projection tests for the agentteams-teamharness OpenClaw and Hermes
runtime adapters.

These tests exercise install.sh / uninstall.sh as black-box scripts against
throwaway config files and assert the shape of the runtime configuration the
adapters project — no live OpenClaw or Hermes runtime is required (the
conformance E2E is a separate, later gate).
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import subprocess
from pathlib import Path

import pytest
import yaml

BUNDLE = Path(__file__).resolve().parents[3] / "plugins" / "agentteams-teamharness"
OPENCLAW_ADAPTER = BUNDLE / "adapters" / "openclaw"
HERMES_ADAPTER = BUNDLE / "adapters" / "hermes"
MCP_SERVER_PY = BUNDLE / "mcp" / "server.py"
ENV_VAR = "TEAMHARNESS_RUNTIME_CONFIG"
ENV_REF = "${" + ENV_VAR + "}"


def run_script(script: Path, *args: str, env_extra: dict | None = None) -> subprocess.CompletedProcess:
    env = dict(os.environ)
    if env_extra:
        env.update(env_extra)
    return subprocess.run(
        ["bash", str(script), *args],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )


# ---------------------------------------------------------------------------
# 1. dry-run exits 0 and output is parseable
# ---------------------------------------------------------------------------


def test_dry_run_both_adapters_exit_zero_and_parse(tmp_path):
    result = run_script(
        OPENCLAW_ADAPTER / "install.sh",
        "--dry-run",
        "--config",
        str(tmp_path / "openclaw.json"),
    )
    assert result.returncode == 0, result.stderr
    config = json.loads(result.stdout)
    assert isinstance(config, dict)

    result = run_script(
        HERMES_ADAPTER / "install.sh",
        "--dry-run",
        "--config",
        str(tmp_path / "config.yaml"),
        "--plugin-home",
        str(tmp_path / "plugin-home"),
    )
    assert result.returncode == 0, result.stderr
    config = yaml.safe_load(result.stdout)
    assert isinstance(config, dict)

    # dry-run changes nothing
    assert not (tmp_path / "openclaw.json").exists()
    assert not (tmp_path / "config.yaml").exists()
    assert not (tmp_path / "plugin-home").exists()


# ---------------------------------------------------------------------------
# 2. MCP entry exists and points at the bundle's mcp/server.py
# ---------------------------------------------------------------------------


def test_mcp_entries_point_at_bundle_server():
    result = run_script(
        OPENCLAW_ADAPTER / "install.sh", "--dry-run", "--config", "/tmp/unused-oc.json"
    )
    assert result.returncode == 0, result.stderr
    entry = json.loads(result.stdout)["mcp"]["servers"]["teamharness"]
    assert entry["command"] == "python3"
    assert entry["args"] == ["-m", "mcp.server"]
    assert Path(entry["cwd"]) == BUNDLE
    assert (Path(entry["cwd"]) / "mcp" / "server.py").is_file()
    assert entry["env"][ENV_VAR] == ENV_REF

    result = run_script(
        HERMES_ADAPTER / "install.sh",
        "--dry-run",
        "--config",
        "/tmp/unused-hm.yaml",
        "--plugin-home",
        "/tmp/unused-hm-plugin",
        # the runtime-config reference is projected only when resolvable;
        # set it explicitly so the assertion is deterministic
        env_extra={ENV_VAR: "/tmp/unused-runtime.yaml"},
    )
    assert result.returncode == 0, result.stderr
    entry = yaml.safe_load(result.stdout)["mcp_servers"]["teamharness"]
    assert entry["command"] == "python3"
    assert entry["args"] == [str(MCP_SERVER_PY)]
    assert MCP_SERVER_PY.is_file()
    assert entry["env"][ENV_VAR] == ENV_REF


# ---------------------------------------------------------------------------
# 3. two consecutive runs are byte-identical
# ---------------------------------------------------------------------------


def test_install_twice_is_byte_identical(tmp_path):
    manifest = OPENCLAW_ADAPTER / "openclaw.plugin.json"
    manifest_before = manifest.read_bytes()
    oc_config = tmp_path / "openclaw.json"
    oc_config.write_text(json.dumps({"agent": {"displayName": "kept"}}) + "\n")
    for _ in range(2):
        result = run_script(OPENCLAW_ADAPTER / "install.sh", "--config", str(oc_config))
        assert result.returncode == 0, result.stderr
    assert json.loads(oc_config.read_text())["agent"] == {"displayName": "kept"}
    assert manifest.read_bytes() == manifest_before, (
        "install must not rewrite an in-sync manifest"
    )

    hm_config = tmp_path / "config.yaml"
    hm_plugin = tmp_path / "plugin-home"
    hm_config.write_text(yaml.safe_dump({"tools": {"enabled": ["kept"]}}))
    for _ in range(2):
        result = run_script(
            HERMES_ADAPTER / "install.sh",
            "--config",
            str(hm_config),
            "--plugin-home",
            str(hm_plugin),
        )
        assert result.returncode == 0, result.stderr
    after = hm_config.read_bytes()
    assert yaml.safe_load(after)["tools"] == {"enabled": ["kept"]}
    for name in ("plugin.yaml", "register.py"):
        assert (hm_plugin / name).read_bytes() == (HERMES_ADAPTER / name).read_bytes()
    skill_md = hm_plugin / "skills" / "agent" / "find-skills" / "SKILL.md"
    assert skill_md.read_bytes() == (BUNDLE / "skills" / "agent" / "find-skills" / "SKILL.md").read_bytes()


# ---------------------------------------------------------------------------
# 3b. manifest skills paths resolve to real SKILL.md dirs from the staged
#     plugin root (the adapter dir registered in plugins.load.paths)
# ---------------------------------------------------------------------------


def test_manifest_skills_resolve_from_staged_plugin_root(tmp_path):
    """Manifest entries are in-root paths, materialised by the installer.

    OpenClaw rejects a skill path that escapes the plugin root, so the entries
    are relative to the plugin root itself; install.sh copies the bundle's
    skills into the staged root, which is the only place they need to resolve.
    """
    config = tmp_path / ".openclaw" / "openclaw.json"
    result = subprocess.run(
        ["bash", str(OPENCLAW_ADAPTER / "install.sh"), "--config", str(config)],
        capture_output=True,
        text=True,
        env={**os.environ, "HOME": str(tmp_path)},
        timeout=120,
    )
    assert result.returncode == 0, result.stderr
    staged_root = config.parent / "plugins" / "agentteams-teamharness"
    manifest = json.loads((staged_root / "openclaw.plugin.json").read_text())
    skills = manifest.get("skills")
    assert isinstance(skills, list) and skills, (
        "manifest must carry the bundle's skill entries"
    )
    for entry in skills:
        assert not entry.startswith("../"), (
            f"manifest skill path {entry!r} escapes the plugin root"
        )
        skill_dir = (staged_root / entry).resolve()
        assert (skill_dir / "SKILL.md").is_file(), (
            f"manifest skill path {entry!r} does not resolve to a SKILL.md "
            "in the staged plugin root"
        )


# ---------------------------------------------------------------------------
# 4. uninstall removes exactly this adapter's keys, keeps the rest
# ---------------------------------------------------------------------------


def test_uninstall_removes_only_own_keys(tmp_path):
    oc_config = tmp_path / "openclaw.json"
    seeded = {
        "mcp": {"servers": {"other": {"command": "other-cmd"}}},
        "plugins": {
            "allow": ["other-plugin"],
            "load": {"paths": ["/elsewhere/plugin"]},
            "entries": {"other-plugin": {"enabled": True}},
        },
        "agent": {"displayName": "kept"},
    }
    oc_config.write_text(json.dumps(seeded, indent=2) + "\n")
    for script, in ((OPENCLAW_ADAPTER / "install.sh",), (OPENCLAW_ADAPTER / "uninstall.sh",)):
        result = run_script(script, "--config", str(oc_config))
        assert result.returncode == 0, result.stderr
    config = json.loads(oc_config.read_text())
    assert "teamharness" not in config["mcp"]["servers"]
    assert config["mcp"]["servers"]["other"] == {"command": "other-cmd"}
    assert "agentteams-teamharness" not in config["plugins"]["allow"]
    assert "other-plugin" in config["plugins"]["allow"]
    assert str(OPENCLAW_ADAPTER) not in config["plugins"]["load"]["paths"]
    assert config["plugins"]["load"]["paths"] == ["/elsewhere/plugin"]
    assert "agentteams-teamharness" not in config["plugins"]["entries"]
    assert config["plugins"]["entries"]["other-plugin"] == {"enabled": True}
    assert config["agent"] == {"displayName": "kept"}

    hm_config = tmp_path / "config.yaml"
    hm_plugin = tmp_path / "plugin-home"
    hm_config.write_text(yaml.safe_dump({"mcp_servers": {"other": {"url": "http://example.internal/mcp"}}}))
    result = run_script(
        HERMES_ADAPTER / "install.sh",
        "--config",
        str(hm_config),
        "--plugin-home",
        str(hm_plugin),
    )
    assert result.returncode == 0, result.stderr
    assert hm_plugin.is_dir()
    result = run_script(
        HERMES_ADAPTER / "uninstall.sh",
        "--config",
        str(hm_config),
        "--plugin-home",
        str(hm_plugin),
    )
    assert result.returncode == 0, result.stderr
    config = yaml.safe_load(hm_config.read_text())
    assert "teamharness" not in config.get("mcp_servers", {})
    assert config["mcp_servers"]["other"] == {"url": "http://example.internal/mcp"}
    assert not hm_plugin.exists()

    # a foreign plugin dir with the same path is not touched
    hm_plugin.mkdir(parents=True)
    (hm_plugin / "plugin.yaml").write_text("name: some-other-plugin\nversion: 1.0\n")
    result = run_script(
        HERMES_ADAPTER / "uninstall.sh",
        "--config",
        str(hm_config),
        "--plugin-home",
        str(hm_plugin),
    )
    assert result.returncode == 0, result.stderr
    assert hm_plugin.is_dir()


# ---------------------------------------------------------------------------
# 5. register.py imports cleanly and exposes a callable register
# ---------------------------------------------------------------------------


def test_register_py_importable_with_callable_register(monkeypatch):
    spec = importlib.util.spec_from_file_location(
        "th_hermes_register_test", HERMES_ADAPTER / "register.py"
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    assert callable(module.register)

    # a ctx without the hermes API is a safe no-op (no real registration)
    class ForeignCtx:
        pass

    assert module.register(ForeignCtx()) == 0

    # pointed at the bundle skills, it registers exactly the bundle's skills
    monkeypatch.setattr(module, "SKILLS_DIR", BUNDLE / "skills")

    class FakeCtx:
        def __init__(self):
            self.registered = []

        def register_skill(self, name, path):
            self.registered.append((name, path))

    ctx = FakeCtx()
    count = module.register(ctx)
    assert count == len(ctx.registered) > 0
    names = {name for name, _ in ctx.registered}
    assert "find-skills" in names and "roomflow" in names
    for _name, path in ctx.registered:
        assert Path(path).is_file()


# ---------------------------------------------------------------------------
# 6. no token literal ever reaches stdout, stderr, or the written files
# ---------------------------------------------------------------------------


def test_no_secret_value_in_output_or_configs(tmp_path):
    secret = "sk-test-secret-9f8e7d6c5b4a"
    env = {ENV_VAR: secret}
    oc_config = tmp_path / "openclaw.json"
    hm_config = tmp_path / "config.yaml"
    hm_plugin = tmp_path / "plugin-home"

    collected = []
    for script, args in (
        (OPENCLAW_ADAPTER / "install.sh", ["--dry-run", "--config", str(oc_config)]),
        (OPENCLAW_ADAPTER / "install.sh", ["--config", str(oc_config)]),
        (
            HERMES_ADAPTER / "install.sh",
            [
                "--dry-run",
                "--config",
                str(hm_config),
                "--plugin-home",
                str(hm_plugin),
            ],
        ),
        (
            HERMES_ADAPTER / "install.sh",
            ["--config", str(hm_config), "--plugin-home", str(hm_plugin)],
        ),
    ):
        result = run_script(script, *args, env_extra=env)
        assert result.returncode == 0, result.stderr
        collected.append(result.stdout + result.stderr)

    collected.append(oc_config.read_text())
    collected.append(hm_config.read_text())
    for path in sorted(hm_plugin.rglob("*")):
        if path.is_file():
            collected.append(path.read_text())

    for blob in collected:
        assert secret not in blob
    assert ENV_REF in oc_config.read_text()
    assert ENV_REF in hm_config.read_text()


# ---------------------------------------------------------------------------
# 7. dry-run previews redact pre-existing credential values; the config file
#    on disk stays byte-identical
# ---------------------------------------------------------------------------


def test_previews_redact_seeded_runtime_credentials(tmp_path):
    synthetic = "sk-synth-TOKEN-BBB"
    oc_config = tmp_path / "openclaw.json"
    oc_config.write_text(
        json.dumps(
            {
                "channels": {
                    "matrix": {
                        "homeserver": "https://hs.example.org",
                        "userId": "@bot:hs.example.org",
                        "accessToken": synthetic,
                    }
                }
            },
            indent=2,
        )
        + "\n"
    )
    oc_hash_before = hashlib.sha256(oc_config.read_bytes()).hexdigest()

    hm_config = tmp_path / "config.yaml"
    hm_plugin = tmp_path / "plugin-home"
    hm_config.write_text(
        yaml.safe_dump(
            {"platforms": {"matrix": {"homeserver": "https://hs.example.org", "token": synthetic}}}
        )
    )
    hm_hash_before = hashlib.sha256(hm_config.read_bytes()).hexdigest()

    for script, args in (
        (OPENCLAW_ADAPTER / "install.sh", ["--dry-run", "--config", str(oc_config)]),
        (OPENCLAW_ADAPTER / "uninstall.sh", ["--dry-run", "--config", str(oc_config)]),
        (
            HERMES_ADAPTER / "install.sh",
            ["--dry-run", "--config", str(hm_config), "--plugin-home", str(hm_plugin)],
        ),
        (
            HERMES_ADAPTER / "uninstall.sh",
            ["--dry-run", "--config", str(hm_config), "--plugin-home", str(hm_plugin)],
        ),
    ):
        result = run_script(script, *args)
        assert result.returncode == 0, result.stderr
        assert synthetic not in result.stdout + result.stderr
        assert "<redacted>" in result.stdout

    assert hashlib.sha256(oc_config.read_bytes()).hexdigest() == oc_hash_before
    assert hashlib.sha256(hm_config.read_bytes()).hexdigest() == hm_hash_before


# ---------------------------------------------------------------------------
# 8. redaction covers the whole rendered document, not only this adapter's
#    owned keys (deeper foreign credentials are masked too)
# ---------------------------------------------------------------------------


def test_previews_redact_nested_foreign_credentials(tmp_path):
    synthetic = "sk-synth-TOKEN-BBB"
    foreign = "xoxb-synth-FOREIGN-998877"
    oc_config = tmp_path / "openclaw.json"
    oc_config.write_text(
        json.dumps(
            {
                "channels": {
                    "matrix": {
                        "homeserver": "https://hs.example.org",
                        "accessToken": synthetic,
                    },
                    "telegram": {"botToken": foreign},
                }
            },
            indent=2,
        )
        + "\n"
    )
    oc_hash_before = hashlib.sha256(oc_config.read_bytes()).hexdigest()

    hm_config = tmp_path / "config.yaml"
    hm_plugin = tmp_path / "plugin-home"
    hm_config.write_text(
        yaml.safe_dump(
            {
                "platforms": {
                    "matrix": {"homeserver": "https://hs.example.org", "token": synthetic},
                    "slack": {"token": foreign},
                }
            }
        )
    )
    hm_hash_before = hashlib.sha256(hm_config.read_bytes()).hexdigest()

    for script, args in (
        (OPENCLAW_ADAPTER / "install.sh", ["--dry-run", "--config", str(oc_config)]),
        (OPENCLAW_ADAPTER / "uninstall.sh", ["--dry-run", "--config", str(oc_config)]),
        (
            HERMES_ADAPTER / "install.sh",
            ["--dry-run", "--config", str(hm_config), "--plugin-home", str(hm_plugin)],
        ),
        (
            HERMES_ADAPTER / "uninstall.sh",
            ["--dry-run", "--config", str(hm_config), "--plugin-home", str(hm_plugin)],
        ),
    ):
        result = run_script(script, *args)
        assert result.returncode == 0, result.stderr
        for value in (synthetic, foreign):
            assert value not in result.stdout + result.stderr

    assert hashlib.sha256(oc_config.read_bytes()).hexdigest() == oc_hash_before
    assert hashlib.sha256(hm_config.read_bytes()).hexdigest() == hm_hash_before


# ---------------------------------------------------------------------------
# 9. real install/uninstall keep an existing 0600 config's mode (write_atomic)
# ---------------------------------------------------------------------------


def test_real_install_uninstall_do_not_loosen_config_mode(tmp_path):
    secret = "sk-synth-TOKEN-BBB"
    oc_config = tmp_path / "openclaw.json"
    oc_config.write_text(json.dumps({"channels": {"matrix": {"accessToken": secret}}}) + "\n")
    oc_config.chmod(0o600)
    assert oc_config.stat().st_mode & 0o777 == 0o600
    result = run_script(OPENCLAW_ADAPTER / "install.sh", "--config", str(oc_config))
    assert result.returncode == 0, result.stderr
    assert oc_config.stat().st_mode & 0o777 == 0o600
    result = run_script(OPENCLAW_ADAPTER / "uninstall.sh", "--config", str(oc_config))
    assert result.returncode == 0, result.stderr
    assert oc_config.stat().st_mode & 0o777 == 0o600
    json.loads(oc_config.read_text())

    hm_config = tmp_path / "config.yaml"
    hm_plugin = tmp_path / "plugin-home"
    hm_config.write_text(yaml.safe_dump({"platforms": {"matrix": {"token": secret}}}))
    hm_config.chmod(0o600)
    assert hm_config.stat().st_mode & 0o777 == 0o600
    result = run_script(
        HERMES_ADAPTER / "install.sh",
        "--config",
        str(hm_config),
        "--plugin-home",
        str(hm_plugin),
    )
    assert result.returncode == 0, result.stderr
    assert hm_config.stat().st_mode & 0o777 == 0o600
    result = run_script(
        HERMES_ADAPTER / "uninstall.sh",
        "--config",
        str(hm_config),
        "--plugin-home",
        str(hm_plugin),
    )
    assert result.returncode == 0, result.stderr
    assert hm_config.stat().st_mode & 0o777 == 0o600
    yaml.safe_load(hm_config.read_text())


# ---------------------------------------------------------------------------
# 10. Worker layout: the effective config target is honoured and the config
#     symlink survives (maintainer P1 on 0614c92a)
# ---------------------------------------------------------------------------


def _worker_layout(tmp_path):
    """The Worker's layout: canonical $HOME/openclaw.json with
    $HOME/.openclaw/openclaw.json a symlink to it. Returns
    (home, canonical, link)."""
    home = tmp_path / "home"
    (home / ".openclaw").mkdir(parents=True)
    canonical = home / "openclaw.json"
    canonical.write_text(json.dumps({"agent": {"displayName": "worker"}}) + "\n")
    link = home / ".openclaw" / "openclaw.json"
    link.symlink_to(canonical)
    return home, canonical, link


def run_worker_script(
    script: Path, *args: str, home: Path, **worker_vars
) -> subprocess.CompletedProcess:
    """Adapter run against a Worker layout: HOME pinned, the OPENCLAW_*
    resolution variables scrubbed, then the worker variables under test
    applied."""
    env = dict(os.environ)
    env["HOME"] = str(home)
    for key in ("OPENCLAW_CONFIG", "OPENCLAW_CONFIG_PATH", "OPENCLAW_WORKSPACE"):
        env.pop(key, None)
    env.update(worker_vars)
    return subprocess.run(
        ["bash", str(script), *args],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )


def test_openclaw_config_path_default_install_honours_symlink(tmp_path):
    """OPENCLAW_CONFIG_PATH set (Worker export), OPENCLAW_CONFIG unset, no
    --config: the canonical target gains the entry; the default-path
    symlink survives; readback through the symlink and the canonical path
    both show the entry; the staged root resolves under the canonical
    config directory."""
    home, canonical, link = _worker_layout(tmp_path)
    result = run_worker_script(
        OPENCLAW_ADAPTER / "install.sh", home=home, OPENCLAW_CONFIG_PATH=str(canonical)
    )
    assert result.returncode == 0, result.stderr
    canonical_data = json.loads(canonical.read_text())
    assert "teamharness" in canonical_data["mcp"]["servers"]
    assert link.is_symlink(), "install must not replace the config symlink"
    via_link = json.loads(link.read_text())
    assert via_link["mcp"]["servers"]["teamharness"] == canonical_data["mcp"]["servers"]["teamharness"]
    staged_root = canonical.parent / "plugins" / "agentteams-teamharness"
    assert staged_root.is_dir(), "staged root must live under the canonical config directory"
    assert str(staged_root) in canonical_data["plugins"]["load"]["paths"]
    assert "agentteams-teamharness" in canonical_data["plugins"]["allow"]


def test_default_fallback_without_worker_vars_writes_through_symlink(tmp_path):
    """OPENCLAW_CONFIG_PATH and OPENCLAW_CONFIG both unset: the default
    $HOME/.openclaw/openclaw.json (the symlink) is still written through
    to the canonical target."""
    home, canonical, link = _worker_layout(tmp_path)
    result = run_worker_script(OPENCLAW_ADAPTER / "install.sh", home=home)
    assert result.returncode == 0, result.stderr
    assert "teamharness" in json.loads(canonical.read_text())["mcp"]["servers"]
    assert link.is_symlink()


def test_legacy_openclaw_config_alone_is_honoured(tmp_path):
    """Legacy OPENCLAW_CONFIG alone (no OPENCLAW_CONFIG_PATH): honoured — a
    symlink value is written through to its canonical target."""
    home = tmp_path / "home"
    home.mkdir()
    data_dir = tmp_path / "data"
    data_dir.mkdir()
    canonical = data_dir / "openclaw.json"
    canonical.write_text("{}\n")
    link_dir = tmp_path / "link"
    link_dir.mkdir()
    link = link_dir / "openclaw.json"
    link.symlink_to(canonical)
    result = run_worker_script(
        OPENCLAW_ADAPTER / "install.sh", home=home, OPENCLAW_CONFIG=str(link)
    )
    assert result.returncode == 0, result.stderr
    data = json.loads(canonical.read_text())
    assert "teamharness" in data["mcp"]["servers"]
    assert link.is_symlink()
    assert str(canonical.parent / "plugins" / "agentteams-teamharness") in data["plugins"]["load"]["paths"]


def test_openclaw_config_path_wins_over_legacy(tmp_path):
    """Resolution priority: OPENCLAW_CONFIG_PATH beats legacy
    OPENCLAW_CONFIG; the legacy path is left untouched."""
    home = tmp_path / "home"
    (home / ".openclaw").mkdir(parents=True)
    preferred = home / "openclaw.json"
    preferred.write_text("{}\n")
    legacy = home / ".openclaw" / "openclaw.json"
    legacy.write_text("{}\n")
    result = run_worker_script(
        OPENCLAW_ADAPTER / "install.sh",
        home=home,
        OPENCLAW_CONFIG_PATH=str(preferred),
        OPENCLAW_CONFIG=str(legacy),
    )
    assert result.returncode == 0, result.stderr
    assert "teamharness" in json.loads(preferred.read_text())["mcp"]["servers"]
    assert "mcp" not in json.loads(legacy.read_text())


def test_uninstall_through_worker_layout_preserves_symlink(tmp_path):
    """Install + uninstall in the Worker layout: the canonical file loses
    exactly this adapter's keys (the seeded foreign key survives), the
    staged root next to the canonical config is removed, and the symlink
    survives the whole round trip."""
    home, canonical, link = _worker_layout(tmp_path)
    for script in (OPENCLAW_ADAPTER / "install.sh", OPENCLAW_ADAPTER / "uninstall.sh"):
        result = run_worker_script(script, home=home, OPENCLAW_CONFIG_PATH=str(canonical))
        assert result.returncode == 0, result.stderr
    data = json.loads(canonical.read_text())
    assert "teamharness" not in data.get("mcp", {}).get("servers", {})
    assert "agentteams-teamharness" not in data.get("plugins", {}).get("entries", {})
    assert data["agent"] == {"displayName": "worker"}
    assert not (canonical.parent / "plugins" / "agentteams-teamharness").exists()
    assert link.is_symlink()


def test_adapter_scripts_pass_bash_syntax_check():
    for script in (OPENCLAW_ADAPTER / "install.sh", OPENCLAW_ADAPTER / "uninstall.sh"):
        result = subprocess.run(
            ["bash", "-n", str(script)],
            capture_output=True,
            text=True,
            timeout=60,
        )
        assert result.returncode == 0, f"bash -n {script.name}: {result.stderr}"


# ---------------------------------------------------------------------------
# 11. Hermes env projection: narrow allowlist, by reference only
#     (maintainer P1 on 1143b604 — the SDK hands the MCP child only a
#     small safe baseline plus the entry's env map, so the entry must
#     project the storage/Matrix context the child needs, as ${NAME}
#     references)
# ---------------------------------------------------------------------------

HERMES_ALLOWLIST = (
    # state/runtime
    "TEAMHARNESS_RUNTIME_CONFIG",
    "TEAMHARNESS_SHARED_DIR",
    "AGENTTEAMS_SHARED_DIR",
    "AGENTTEAMS_STORAGE_PREFIX",
    "AGENTTEAMS_SHARED_STORAGE_PREFIX",
    "TEAMHARNESS_MATRIX_CONTEXT_FILE",
    # Matrix
    "AGENTTEAMS_MATRIX_URL",
    "AGENTTEAMS_WORKER_MATRIX_TOKEN",
    "AGENTTEAMS_WORKER_NAME",
    "AGENTTEAMS_MATRIX_USER_ID",
    "AGENTTEAMS_MATRIX_DOMAIN",
    # storage credentials
    "AGENTTEAMS_FS_ENDPOINT",
    "AGENTTEAMS_FS_ACCESS_KEY",
    "AGENTTEAMS_FS_SECRET_KEY",
)
HERMES_ROLE_VARS = ("AGENTTEAMS_AGENT_ROLE", "AGENTTEAMS_WORKER_ROLE")
HERMES_JUNK_VAR = "TH_TEST_UNRELATED_JUNK"
HERMES_EXCLUDED = HERMES_ROLE_VARS + (HERMES_JUNK_VAR,)


def hermes_script_env(tmp_path: Path, extra: dict) -> dict:
    """Deterministic installer env: every allowlisted/role/junk variable
    scrubbed, HOME + HERMES_HOME pinned (hermetic ~/.hermes/.env
    resolution), then ``extra`` applied."""
    home = tmp_path / "home"
    home.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ)
    env["HOME"] = str(home)
    env["HERMES_HOME"] = str(home / ".hermes")
    for key in list(env):
        if key in HERMES_ALLOWLIST or key in HERMES_EXCLUDED:
            del env[key]
    env.update(extra)
    return env


def install_hermes(config: Path, plugin_home: Path, env: dict) -> subprocess.CompletedProcess:
    return subprocess.run(
        [
            "bash",
            str(HERMES_ADAPTER / "install.sh"),
            "--config",
            str(config),
            "--plugin-home",
            str(plugin_home),
        ],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )


def test_hermes_env_is_exactly_the_resolved_allowlist(tmp_path):
    """Entry shape: env carries exactly the allowlist (every var set in
    the installer env), each value a ${NAME} reference — no literal
    values, no role overrides, no unrelated variables."""
    values = {name: f"value-{name.lower()}-zzz" for name in HERMES_ALLOWLIST}
    env = hermes_script_env(
        tmp_path,
        {
            **values,
            HERMES_JUNK_VAR: "junk-value",
            "AGENTTEAMS_AGENT_ROLE": "override-leader",
            "AGENTTEAMS_WORKER_ROLE": "override-worker",
        },
    )
    config = tmp_path / "config.yaml"
    plugin_home = tmp_path / "plugin-home"
    result = install_hermes(config, plugin_home, env)
    assert result.returncode == 0, result.stderr
    projected = yaml.safe_load(config.read_text())["mcp_servers"]["teamharness"]["env"]
    assert set(projected) == set(HERMES_ALLOWLIST)
    for name in HERMES_ALLOWLIST:
        assert projected[name] == "${" + name + "}"
    raw = config.read_text()
    for name in HERMES_ALLOWLIST:
        assert values[name] not in raw, f"literal value of {name} written into config"
    for name in HERMES_EXCLUDED:
        assert name not in raw, f"excluded variable {name} projected into the entry"


def test_hermes_env_resolution_rule_installer_env_and_dotenv_keys(tmp_path):
    """Resolution rule: a var is projected when set (non-empty) in the
    installer env OR its key appears in $HERMES_HOME/.env — keys parsed,
    values never read, non-allowlisted dotenv keys never projected."""
    home = tmp_path / "home"
    (home / ".hermes").mkdir(parents=True)
    (home / ".hermes" / ".env").write_text(
        "# hermes dotenv\n"
        "AGENTTEAMS_MATRIX_URL=https://hs.example.org\n"
        "export AGENTTEAMS_WORKER_MATRIX_TOKEN='tok-dotenv-abc'\n"
        "AGENTTEAMS_FS_SECRET_KEY=\n"
        "TH_TEST_UNRELATED_JUNK=junk-in-dotenv\n"
    )
    env = hermes_script_env(tmp_path, {ENV_VAR: str(tmp_path / "runtime.yaml")})
    config = tmp_path / "config.yaml"
    plugin_home = tmp_path / "plugin-home"
    result = install_hermes(config, plugin_home, env)
    assert result.returncode == 0, result.stderr
    projected = yaml.safe_load(config.read_text())["mcp_servers"]["teamharness"]["env"]
    expected = {
        ENV_VAR,  # set (non-empty) in the installer env
        "AGENTTEAMS_MATRIX_URL",  # dotenv key
        "AGENTTEAMS_WORKER_MATRIX_TOKEN",  # dotenv key, export-prefixed
        "AGENTTEAMS_FS_SECRET_KEY",  # dotenv key with an empty value
    }
    assert set(projected) == expected
    for name in expected:
        assert projected[name] == "${" + name + "}"
    raw = config.read_text()
    for value in ("https://hs.example.org", "tok-dotenv-abc", "junk-in-dotenv"):
        assert value not in raw, "dotenv value leaked into the config"
    assert HERMES_JUNK_VAR not in raw


def test_hermes_install_preserves_foreign_mcp_servers(tmp_path):
    """No regression: an mcp_servers entry the installer does not own is
    preserved next to the projected entry."""
    env = hermes_script_env(
        tmp_path,
        {
            ENV_VAR: str(tmp_path / "runtime.yaml"),
            "AGENTTEAMS_MATRIX_URL": "https://hs.example.org",
            "AGENTTEAMS_WORKER_MATRIX_TOKEN": "synthetic-token",
        },
    )
    config = tmp_path / "config.yaml"
    config.write_text(
        yaml.safe_dump(
            {
                "mcp_servers": {"other": {"command": "other-cmd", "args": ["--x"]}},
                "tools": {"enabled": ["kept"]},
            }
        )
    )
    plugin_home = tmp_path / "plugin-home"
    result = install_hermes(config, plugin_home, env)
    assert result.returncode == 0, result.stderr
    data = yaml.safe_load(config.read_text())
    assert data["mcp_servers"]["other"] == {"command": "other-cmd", "args": ["--x"]}
    assert data["mcp_servers"]["teamharness"]["env"] == {
        ENV_VAR: "${" + ENV_VAR + "}",
        "AGENTTEAMS_MATRIX_URL": "${AGENTTEAMS_MATRIX_URL}",
        "AGENTTEAMS_WORKER_MATRIX_TOKEN": "${AGENTTEAMS_WORKER_MATRIX_TOKEN}",
    }
    assert data["tools"] == {"enabled": ["kept"]}


def test_hermes_scripts_pass_bash_syntax_check():
    for script in (HERMES_ADAPTER / "install.sh", HERMES_ADAPTER / "uninstall.sh"):
        result = subprocess.run(
            ["bash", "-n", str(script)],
            capture_output=True,
            text=True,
            timeout=60,
        )
        assert result.returncode == 0, f"bash -n {script.name}: {result.stderr}"
