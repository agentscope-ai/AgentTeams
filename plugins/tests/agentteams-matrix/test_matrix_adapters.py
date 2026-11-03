"""Adapter tests for the agentteams-matrix bundle.

The OpenClaw adapter projects the bundle contract.yaml onto its
runtime config file (openclaw.json). The Hermes adapter is a native
platform plugin: its installer stages the payload under
$HERMES_HOME/plugins/agentteams-matrix with a per-file ownership
marker, and that installer behaviour is covered by
test_adapter_hermes.py, not here. This suite therefore covers:

  1. OpenClaw --dry-run exits 0 and stdout parses as the target format
  2. a real install writes all required keys at the key paths the
     upstream runtime actually reads (evidence below, per key in
     _expected_projection and _assert_openclaw_nested)
  3. two installs in a row leave a byte-identical file
  4. uninstall removes only the adapter-owned keys, keeps foreign
     keys (including foreign sub-keys of the deep-merged blocks)
  5. without --with-credentials no token literal appears anywhere
  6. a missing contract.yaml fails with a non-zero exit and a clear
     error

plus bundle-level dispatch cases: the selected adapter is invoked
(--adapter openclaw projects, --adapter hermes stages the native
plugin under a temp HERMES_HOME, and the uninstaller removes it
again), an unknown adapter is rejected, and the removed Hermes
projector flags are rejected with a clear error.

Upstream evidence for the projected key paths:

OpenClaw (v2026.10.6, fe06181d) — extensions/matrix/src/
config-schema.ts: enabled:103, homeserver:127, userId:130,
groupPolicy:142, historyLimit:160, autoJoin:161,
autoJoinAllowlist:162, groupAllowFrom:163, groups map:169; the
nested dm block {enabled, policy, allowFrom} is defined in
src/channels/plugins/config-schema.ts:89-96; per-room
requireMention is a group-entry field at
src/channels/plugins/config-schema.ts:62; dm policy values
(pairing|allowlist|open|disabled) at
src/config/zod-schema.core.ts:436.

No network access is involved and the adapters are pure file
operations; the OpenClaw adapter writes its config file and the
Hermes installer copies the payload.
"""

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE_DIR = REPO_ROOT / "plugins" / "agentteams-matrix"
CONTRACT = BUNDLE_DIR / "contract.yaml"
BUNDLE_INSTALL = BUNDLE_DIR / "scripts" / "install.sh"
BUNDLE_UNINSTALL = BUNDLE_DIR / "scripts" / "uninstall.sh"
TOKEN = "sentinel-matrix-token-not-a-real-secret-0123456789"

CONTRACT_MATRIX = yaml.safe_load(CONTRACT.read_text(encoding="utf-8"))["matrix"]

ADAPTERS = {
    "openclaw": {
        "install": BUNDLE_DIR / "adapters" / "openclaw" / "install.sh",
        "uninstall": BUNDLE_DIR / "adapters" / "openclaw" / "uninstall.sh",
        # accessToken — extensions/matrix/src/config-schema.ts:131
        "token_key": "accessToken",
    },
    "hermes": {
        # Native platform plugin: the scripts stage/remove the plugin
        # payload, there is no config projection (and no token key).
        "install": BUNDLE_DIR / "adapters" / "hermes" / "install.sh",
        "uninstall": BUNDLE_DIR / "adapters" / "hermes" / "uninstall.sh",
    },
}


def _expected_projection(adapter: str) -> dict:
    """Flat keys the adapter writes, at the upstream key paths.

    OpenClaw channels.matrix (extensions/matrix/src/
    config-schema.ts): enabled:103, homeserver:127, userId:130,
    historyLimit:160, groupPolicy:142, groupAllowFrom:163,
    autoJoin:161, autoJoinAllowlist:162. The nested dm/ and groups/
    blocks are asserted in _assert_openclaw_nested (dm:
    src/channels/plugins/config-schema.ts:89-96; per-room
    requireMention: src/channels/plugins/config-schema.ts:62).

    OpenClaw-only: the Hermes adapter is a native platform plugin
    with no config projection. The contract's access_token_ref and
    history_limit / history_mode are deliberately NOT projected: no
    upstream key exists for them (see the adapter READMEs).
    """
    assert adapter == "openclaw"
    m = CONTRACT_MATRIX
    return {
        "enabled": True,
        "homeserver": m["homeserver"],
        "userId": m["user_id"],
        "historyLimit": m["history_limit"],
        "groupPolicy": "allowlist",
        "groupAllowFrom": list(m["group_allow_from"]),
        "autoJoin": "allowlist",
        "autoJoinAllowlist": list(m["rooms"]),
    }


def _assert_openclaw_nested(block: dict) -> None:
    """Nested channels.matrix blocks, at the upstream key paths."""
    m = CONTRACT_MATRIX
    # dm.allowFrom lives in the nested dm block {enabled, policy,
    # allowFrom} (src/channels/plugins/config-schema.ts:89-96); the
    # runtime's default dm policy is "pairing" (monitor/index.ts:264),
    # so the explicit allowlist policy is part of the projection.
    assert block["dm"]["policy"] == "allowlist"
    assert block["dm"]["allowFrom"] == list(m["allow_from"])
    # groupPolicy accompanies groupAllowFrom so the list is a hard
    # gate (config-schema.ts:142,163; values at
    # src/config/zod-schema.core.ts:434).
    assert block["groupPolicy"] == "allowlist"
    # One groups.<room>.requireMention entry per contract room:
    # requireMention is a per-room group-entry field
    # (src/channels/plugins/config-schema.ts:62; groups map at
    # config-schema.ts:169) — there is no top-level requireMention.
    for room in m["rooms"]:
        assert block["groups"][room]["requireMention"] == m["require_mention"]


def _run(
    script: Path,
    args: list,
    env: dict,
    umask: int | None = None,
) -> subprocess.CompletedProcess:
    kwargs = {}
    if umask is not None:
        # POSIX-only: the adapter scripts only run on POSIX.
        kwargs["preexec_fn"] = lambda: os.umask(umask)
    return subprocess.run(
        ["bash", str(script), *args],
        env=env,
        cwd=REPO_ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
        **kwargs,
    )


def _env(tmp_path: Path, bundle_dir: Path | None = None) -> dict:
    env = dict(os.environ)
    env["HOME"] = str(tmp_path)
    if bundle_dir is not None:
        env["AGENTTEAMS_BUNDLE_DIR"] = str(bundle_dir)
    env.pop("AGENTTEAMS_MATRIX_ACCESS_TOKEN", None)
    env.pop("HERMES_VERSION", None)
    return env


def _load(adapter: str, text: str):
    assert adapter == "openclaw"
    return json.loads(text)


def _load_file(adapter: str, path: Path):
    return _load(adapter, path.read_text(encoding="utf-8"))


def _matrix_block(adapter: str, config: dict) -> dict:
    assert adapter == "openclaw"
    return config["channels"]["matrix"]


def _seed_config(adapter: str, path: Path) -> None:
    """Write an OpenClaw config file with foreign keys the adapter
    must preserve (OpenClaw-only: the Hermes adapter has no config
    file)."""
    assert adapter == "openclaw"
    config = {
        "agents": {"defaults": {"timeoutSeconds": 1800}},
        "channels": {
            "whatsapp": {"enabled": True},
            "matrix": {
                "streaming": "partial",
                "deviceName": "AgentTeams-adapter-test",
                "dm": {"threadReplies": "off"},
                "groups": {"!foreign:agentteams.local": {"autoReply": True}},
            },
        },
        "session": {"resetByType": {"dm": {"mode": "daily"}}},
    }
    path.write_text(
        json.dumps(config, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def _write_config(adapter: str, path: Path, config: dict) -> None:
    """Write the config back in OpenClaw's on-disk format."""
    assert adapter == "openclaw"
    path.write_text(
        json.dumps(config, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def _foreign_keys_ok(adapter: str, config: dict) -> None:
    assert adapter == "openclaw"
    assert config["agents"] == {"defaults": {"timeoutSeconds": 1800}}
    assert config["channels"]["whatsapp"] == {"enabled": True}
    assert config["channels"]["matrix"]["streaming"] == "partial"
    assert config["channels"]["matrix"]["deviceName"] == "AgentTeams-adapter-test"
    # Foreign sub-keys of the deep-merged dm/ and groups/ blocks
    # survive.
    assert config["channels"]["matrix"]["dm"]["threadReplies"] == "off"
    assert config["channels"]["matrix"]["groups"]["!foreign:agentteams.local"] == {
        "autoReply": True
    }
    assert config["session"] == {"resetByType": {"dm": {"mode": "daily"}}}


@pytest.mark.parametrize("adapter", ["openclaw"])
class TestMatrixAdapters:
    def test_dry_run_parses_and_writes_nothing(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        before = config_path.read_bytes()

        result = _run(
            specs["install"],
            ["--dry-run", "--config", str(config_path)],
            _env(tmp_path),
        )
        assert result.returncode == 0, result.stderr

        config = _load(adapter, result.stdout)
        block = _matrix_block(adapter, config)
        for key, value in _expected_projection(adapter).items():
            assert block[key] == value
        if adapter == "openclaw":
            _assert_openclaw_nested(block)
        _foreign_keys_ok(adapter, config)

        assert config_path.read_bytes() == before, "dry run must not write"

    def test_dry_run_without_existing_file(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "home" / "nested" / "runtime-config"

        result = _run(
            specs["install"],
            ["--dry-run", "--config", str(config_path)],
            _env(tmp_path),
        )
        assert result.returncode == 0, result.stderr
        config = _load(adapter, result.stdout)
        block = _matrix_block(adapter, config)
        for key, value in _expected_projection(adapter).items():
            assert block[key] == value
        if adapter == "openclaw":
            _assert_openclaw_nested(block)
        assert not config_path.exists(), "dry run must not create the file"

    def test_install_projects_contract_and_preserves_foreign_keys(
        self, tmp_path, adapter
    ):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)

        result = _run(
            specs["install"], ["--config", str(config_path)], _env(tmp_path)
        )
        assert result.returncode == 0, result.stderr

        config = _load_file(adapter, config_path)
        block = _matrix_block(adapter, config)
        for key, value in _expected_projection(adapter).items():
            assert block[key] == value
        if adapter == "openclaw":
            _assert_openclaw_nested(block)
        _foreign_keys_ok(adapter, config)
        # Default mode writes no credential material at all (the
        # contract's access_token_ref has no key on either runtime).
        assert specs["token_key"] not in block

    def test_install_twice_is_byte_identical(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]

        # Seeded config: merge path stays stable across runs.
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        _run(specs["install"], ["--config", str(config_path)], _env(tmp_path))
        first = config_path.read_bytes()
        result = _run(
            specs["install"],
            ["--config", str(config_path)],
            _env(tmp_path),
        )
        assert result.returncode == 0, result.stderr
        assert config_path.read_bytes() == first

        # Fresh config (file does not exist yet): same guarantee.
        fresh = tmp_path / "fresh" / "runtime-config"
        _run(specs["install"], ["--config", str(fresh)], _env(tmp_path))
        first_fresh = fresh.read_bytes()
        _run(specs["install"], ["--config", str(fresh)], _env(tmp_path))
        assert fresh.read_bytes() == first_fresh

    def test_uninstall_removes_owned_keys_only(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        _run(specs["install"], ["--config", str(config_path)], _env(tmp_path))

        result = _run(
            specs["uninstall"], ["--config", str(config_path)], _env(tmp_path)
        )
        assert result.returncode == 0, result.stderr

        assert config_path.is_file(), "uninstall must not delete the config file"
        config = _load_file(adapter, config_path)
        owned = set(_expected_projection(adapter)) | {specs["token_key"]}
        if adapter == "openclaw":
            block = config["channels"]["matrix"]
            assert not owned.intersection(block)
            # Foreign keys inside channels.matrix survive, including the
            # foreign sub-keys of the deep-merged dm/ and groups/ blocks
            # (the per-room entry the installer created is dropped once
            # empty; the foreign room entry is untouched).
            assert block == {
                "streaming": "partial",
                "deviceName": "AgentTeams-adapter-test",
                "dm": {"threadReplies": "off"},
                "groups": {"!foreign:agentteams.local": {"autoReply": True}},
            }
        else:
            block = config["platforms"]["matrix"]
            assert not owned.intersection(block)
            # Only the foreign key remains in the block.
            assert block == {"reply_to_mode": "first"}
        _foreign_keys_ok(adapter, config)

    def test_uninstall_is_idempotent(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        _run(specs["install"], ["--config", str(config_path)], _env(tmp_path))
        _run(specs["uninstall"], ["--config", str(config_path)], _env(tmp_path))
        after_first = config_path.read_bytes()

        result = _run(
            specs["uninstall"], ["--config", str(config_path)], _env(tmp_path)
        )
        assert result.returncode == 0, result.stderr
        assert config_path.read_bytes() == after_first

    def test_uninstall_after_fresh_install(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"

        _run(specs["install"], ["--config", str(config_path)], _env(tmp_path))
        _run(specs["uninstall"], ["--config", str(config_path)], _env(tmp_path))

        assert config_path.is_file(), "uninstall must not delete the config file"
        config = _load_file(adapter, config_path)
        if adapter == "openclaw":
            assert "channels" not in config
        else:
            assert "platforms" not in config

    def test_token_never_leaked_without_flag(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        env = _env(tmp_path)
        env["AGENTTEAMS_MATRIX_ACCESS_TOKEN"] = TOKEN

        # Real install without the flag: no token in file or output.
        result = _run(specs["install"], ["--config", str(config_path)], env)
        assert result.returncode == 0, result.stderr
        assert TOKEN not in config_path.read_text(encoding="utf-8")
        assert specs["token_key"] not in _matrix_block(
            adapter, _load_file(adapter, config_path)
        )
        assert TOKEN not in result.stdout
        assert TOKEN not in result.stderr

        # Dry run: stdout is the whole document, so it must be clean too.
        dry = _run(
            specs["install"],
            ["--dry-run", "--config", str(config_path)],
            env,
        )
        assert dry.returncode == 0, dry.stderr
        assert TOKEN not in dry.stdout
        assert TOKEN not in dry.stderr

    def test_with_credentials_writes_token_and_chmod600(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        env = _env(tmp_path)
        env["AGENTTEAMS_MATRIX_ACCESS_TOKEN"] = TOKEN

        result = _run(
            specs["install"],
            ["--with-credentials", "--config", str(config_path)],
            env,
        )
        assert result.returncode == 0, result.stderr
        block = _matrix_block(adapter, _load_file(adapter, config_path))
        assert block[specs["token_key"]] == TOKEN
        # The contract's access_token_ref must not leak in as a dead
        # key (no such key exists on either runtime).
        if adapter == "openclaw":
            assert "accessTokenRef" not in block
        else:
            assert "access_token_ref" not in block
        assert (config_path.stat().st_mode & 0o777) == 0o600
        assert TOKEN not in result.stdout
        assert TOKEN not in result.stderr

        # Dry run redacts the token instead of printing it.
        dry = _run(
            specs["install"],
            ["--with-credentials", "--dry-run", "--config", str(config_path)],
            env,
        )
        assert dry.returncode == 0, dry.stderr
        assert TOKEN not in dry.stdout
        assert TOKEN not in dry.stderr
        preview = _load(adapter, dry.stdout)
        assert (
            _matrix_block(adapter, preview)[specs["token_key"]] == "<redacted>"
        )

    def test_preview_redacts_existing_token_without_flag(self, tmp_path, adapter):
        """A token already stored in the config file must never be
        printed by a plain --dry-run (no flag, no env)."""
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        # The "existing credential" state: a token already stored in the
        # file, under a restrictive mode.
        config = _load_file(adapter, config_path)
        _matrix_block(adapter, config)[specs["token_key"]] = TOKEN
        _write_config(adapter, config_path, config)
        config_path.chmod(0o600)
        before = config_path.read_bytes()

        result = _run(
            specs["install"],
            ["--dry-run", "--config", str(config_path)],
            _env(tmp_path),  # no AGENTTEAMS_MATRIX_ACCESS_TOKEN
        )
        assert result.returncode == 0, result.stderr
        assert TOKEN not in result.stdout
        assert TOKEN not in result.stderr
        preview = _load(adapter, result.stdout)
        assert _matrix_block(adapter, preview)[specs["token_key"]] == "<redacted>"
        assert config_path.read_bytes() == before

    def test_with_credentials_then_plain_preview_and_reinstall(self, tmp_path, adapter):
        """The PR #1458 review sequence: --with-credentials install, then a
        plain dry-run (defect: the stored token leaked into the preview),
        then a plain reinstall under umask 022 (defect: the file came back
        0644 even though it carries the token)."""
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        env = _env(tmp_path)
        env["AGENTTEAMS_MATRIX_ACCESS_TOKEN"] = TOKEN

        result = _run(
            specs["install"],
            ["--with-credentials", "--config", str(config_path)],
            env,
        )
        assert result.returncode == 0, result.stderr
        assert (config_path.stat().st_mode & 0o777) == 0o600
        assert TOKEN in config_path.read_text(encoding="utf-8")

        # Plain preview, no flag, no env: the stored token must not be
        # printed.
        clean = _env(tmp_path)
        dry = _run(
            specs["install"],
            ["--dry-run", "--config", str(config_path)],
            clean,
        )
        assert dry.returncode == 0, dry.stderr
        assert TOKEN not in dry.stdout
        assert TOKEN not in dry.stderr

        # Plain reinstall under umask 022: token retained, mode must stay
        # 0600, and no temp-file leftovers.
        result = _run(
            specs["install"],
            ["--config", str(config_path)],
            clean,
            umask=0o022,
        )
        assert result.returncode == 0, result.stderr
        assert TOKEN in config_path.read_text(encoding="utf-8")
        assert (config_path.stat().st_mode & 0o777) == 0o600
        leftovers = [p for p in tmp_path.rglob("*") if p.name.endswith(".tmp")]
        assert not leftovers

    def test_uninstall_preview_redacts_foreign_credentials(self, tmp_path, adapter):
        """The uninstall preview renders the whole document, so a
        credential in a sibling block the adapter does not own must be
        redacted there too."""
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        _run(specs["install"], ["--config", str(config_path)], _env(tmp_path))
        config = _load_file(adapter, config_path)
        if adapter == "openclaw":
            config["channels"]["telegram"] = {"botToken": TOKEN}
        else:
            config["platforms"]["slack"] = {"token": TOKEN}
        _write_config(adapter, config_path, config)
        before = config_path.read_bytes()

        result = _run(
            specs["uninstall"],
            ["--dry-run", "--config", str(config_path)],
            _env(tmp_path),
        )
        assert result.returncode == 0, result.stderr
        assert TOKEN not in result.stdout
        assert TOKEN not in result.stderr
        assert config_path.read_bytes() == before

    def test_uninstall_after_with_credentials_keeps_restrictive_mode(
        self, tmp_path, adapter
    ):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        _seed_config(adapter, config_path)
        env = _env(tmp_path)
        env["AGENTTEAMS_MATRIX_ACCESS_TOKEN"] = TOKEN

        result = _run(
            specs["install"],
            ["--with-credentials", "--config", str(config_path)],
            env,
        )
        assert result.returncode == 0, result.stderr
        assert (config_path.stat().st_mode & 0o777) == 0o600

        result = _run(specs["uninstall"], ["--config", str(config_path)], _env(tmp_path))
        assert result.returncode == 0, result.stderr
        config = _load_file(adapter, config_path)
        assert specs["token_key"] not in _matrix_block(adapter, config)
        assert (config_path.stat().st_mode & 0o777) == 0o600
        leftovers = [p for p in tmp_path.rglob("*") if p.name.endswith(".tmp")]
        assert not leftovers

    def test_with_credentials_requires_env(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]
        config_path = tmp_path / "runtime-config"
        env = _env(tmp_path)  # no AGENTTEAMS_MATRIX_ACCESS_TOKEN

        result = _run(
            specs["install"],
            ["--with-credentials", "--config", str(config_path)],
            env,
        )
        assert result.returncode != 0
        assert "AGENTTEAMS_MATRIX_ACCESS_TOKEN" in result.stderr
        assert not config_path.exists()

    def test_missing_contract_fails_with_clear_error(self, tmp_path, adapter):
        specs = ADAPTERS[adapter]
        empty_bundle = tmp_path / "empty-bundle"
        empty_bundle.mkdir()
        env = _env(tmp_path, bundle_dir=empty_bundle)
        config_path = tmp_path / "runtime-config"

        result = _run(specs["install"], ["--config", str(config_path)], env)
        assert result.returncode != 0
        assert "contract" in (result.stderr + result.stdout).lower()
        assert not config_path.exists()


def test_openclaw_groups_entry_per_room(tmp_path):
    """One groups.<room>.requireMention entry per contract room.

    requireMention is a per-room group-entry field (src/channels/
    plugins/config-schema.ts:62; groups map at config-schema.ts:169)
    and the room list rides on autoJoinAllowlist
    (config-schema.ts:161-162), so a two-room contract must produce
    exactly two per-room entries — and the uninstaller must remove
    exactly those.
    """
    specs = ADAPTERS["openclaw"]
    bundle = tmp_path / "bundle"
    bundle.mkdir()
    contract = yaml.safe_load(CONTRACT.read_text(encoding="utf-8"))
    contract["matrix"]["rooms"] = [
        "!room-a:agentteams.local",
        "!room-b:agentteams.local",
    ]
    (bundle / "contract.yaml").write_text(
        yaml.safe_dump(contract, sort_keys=False, allow_unicode=True),
        encoding="utf-8",
    )

    config_path = tmp_path / "openclaw.json"
    env = _env(tmp_path, bundle_dir=bundle)
    result = _run(specs["install"], ["--config", str(config_path)], env)
    assert result.returncode == 0, result.stderr

    block = _load_file("openclaw", config_path)["channels"]["matrix"]
    assert block["autoJoinAllowlist"] == contract["matrix"]["rooms"]
    for room in contract["matrix"]["rooms"]:
        assert block["groups"][room] == {"requireMention": True}

    result = _run(specs["uninstall"], ["--config", str(config_path)], env)
    assert result.returncode == 0, result.stderr
    config = _load_file("openclaw", config_path)
    assert "channels" not in config


def test_openclaw_uninstall_missing_contract_fails(tmp_path):
    specs = ADAPTERS["openclaw"]
    empty_bundle = tmp_path / "empty-bundle"
    empty_bundle.mkdir()
    env = _env(tmp_path, bundle_dir=empty_bundle)
    config_path = tmp_path / "openclaw.json"
    config_path.write_text(
        json.dumps({"channels": {"matrix": {"enabled": True}}}) + "\n",
        encoding="utf-8",
    )

    result = _run(specs["uninstall"], ["--config", str(config_path)], env)
    assert result.returncode != 0
    assert "contract" in result.stderr
    # Nothing was removed: the config is untouched.
    assert json.loads(config_path.read_text(encoding="utf-8")) == {
        "channels": {"matrix": {"enabled": True}}
    }


OLD_HOMESERVER = "https://matrix.stale.example"


class TestOpenclawEffectiveConfig:
    """The real Worker entrypoint layout: the canonical config is
    $HOME/openclaw.json and $HOME/.openclaw/openclaw.json is a symlink
    to it, with OPENCLAW_CONFIG_PATH=$HOME/openclaw.json exported. The
    adapter must operate on the runtime's *effective* config target
    (explicit --config, then OPENCLAW_CONFIG_PATH, then the legacy
    OPENCLAW_CONFIG, then the normal default) and must never replace a
    symlink with a regular file — the write goes through the link to
    the canonical target.
    """

    def _worker_layout(self, tmp_path: Path) -> tuple[Path, Path]:
        """Canonical $HOME/openclaw.json holding a stale homeserver,
        plus the $HOME/.openclaw/openclaw.json symlink to it. Returns
        (canonical, link)."""
        canonical = tmp_path / "openclaw.json"
        canonical.write_text(
            json.dumps(
                {"channels": {"matrix": {"homeserver": OLD_HOMESERVER}}},
                indent=2,
                sort_keys=True,
            )
            + "\n",
            encoding="utf-8",
        )
        link_dir = tmp_path / ".openclaw"
        link_dir.mkdir()
        link = link_dir / "openclaw.json"
        link.symlink_to(canonical)
        return canonical, link

    def _worker_env(self, tmp_path: Path, canonical: Path) -> dict:
        """The Worker's environment: OPENCLAW_CONFIG_PATH at the
        canonical file, legacy OPENCLAW_CONFIG unset."""
        env = _env(tmp_path)
        env["OPENCLAW_CONFIG_PATH"] = str(canonical)
        env.pop("OPENCLAW_CONFIG", None)
        return env

    def test_default_call_writes_through_worker_symlink(self, tmp_path):
        """Default call (no --config): OPENCLAW_CONFIG_PATH selects the
        canonical file; the projection lands there, the symlink
        survives, and reading through the link (the native readback)
        returns the new value."""
        specs = ADAPTERS["openclaw"]
        canonical, link = self._worker_layout(tmp_path)
        env = self._worker_env(tmp_path, canonical)

        result = _run(specs["install"], [], env)
        assert result.returncode == 0, result.stderr

        block = _load_file("openclaw", canonical)["channels"]["matrix"]
        for key, value in _expected_projection("openclaw").items():
            assert block[key] == value
        assert block["homeserver"] == CONTRACT_MATRIX["homeserver"]

        # The link was written through, not replaced by a regular file.
        assert link.is_symlink()
        assert os.readlink(link) == str(canonical)
        assert (
            _load_file("openclaw", link)["channels"]["matrix"]["homeserver"]
            == CONTRACT_MATRIX["homeserver"]
        )

    def test_default_symlink_path_written_through_when_env_unset(self, tmp_path):
        """Same layout with OPENCLAW_CONFIG_PATH unset: the normal
        default ($HOME/.openclaw/openclaw.json) is selected — it is the
        symlink — and the write still goes through it."""
        specs = ADAPTERS["openclaw"]
        canonical, link = self._worker_layout(tmp_path)
        env = _env(tmp_path)
        env.pop("OPENCLAW_CONFIG_PATH", None)
        env.pop("OPENCLAW_CONFIG", None)

        result = _run(specs["install"], [], env)
        assert result.returncode == 0, result.stderr

        assert (
            _load_file("openclaw", canonical)["channels"]["matrix"]["homeserver"]
            == CONTRACT_MATRIX["homeserver"]
        )
        assert link.is_symlink()
        assert os.readlink(link) == str(canonical)
        assert (
            _load_file("openclaw", link)["channels"]["matrix"]["homeserver"]
            == CONTRACT_MATRIX["homeserver"]
        )

    def test_explicit_config_symlink_preserved(self, tmp_path):
        """Explicit --config pointing at the symlink wins over the
        environment: written through, link intact."""
        specs = ADAPTERS["openclaw"]
        canonical, link = self._worker_layout(tmp_path)
        env = self._worker_env(tmp_path, canonical)

        result = _run(specs["install"], ["--config", str(link)], env)
        assert result.returncode == 0, result.stderr

        assert (
            _load_file("openclaw", canonical)["channels"]["matrix"]["homeserver"]
            == CONTRACT_MATRIX["homeserver"]
        )
        assert link.is_symlink()
        assert os.readlink(link) == str(canonical)

    def test_legacy_openclaw_config_env_honoured(self, tmp_path):
        """Legacy OPENCLAW_CONFIG alone (no OPENCLAW_CONFIG_PATH) still
        selects its path (backward compatibility)."""
        specs = ADAPTERS["openclaw"]
        legacy = tmp_path / "legacy-openclaw.json"
        legacy.write_text(
            json.dumps(
                {"channels": {"matrix": {"homeserver": OLD_HOMESERVER}}},
                indent=2,
                sort_keys=True,
            )
            + "\n",
            encoding="utf-8",
        )
        env = _env(tmp_path)
        env.pop("OPENCLAW_CONFIG_PATH", None)
        env["OPENCLAW_CONFIG"] = str(legacy)

        result = _run(specs["install"], [], env)
        assert result.returncode == 0, result.stderr

        assert (
            _load_file("openclaw", legacy)["channels"]["matrix"]["homeserver"]
            == CONTRACT_MATRIX["homeserver"]
        )
        # The normal default was not touched.
        assert not (tmp_path / ".openclaw" / "openclaw.json").exists()

    def test_uninstall_with_worker_layout(self, tmp_path):
        """Install then uninstall under the Worker layout: the owned
        keys are removed from the canonical file, the symlink stays
        intact."""
        specs = ADAPTERS["openclaw"]
        canonical, link = self._worker_layout(tmp_path)
        env = self._worker_env(tmp_path, canonical)

        result = _run(specs["install"], [], env)
        assert result.returncode == 0, result.stderr
        result = _run(specs["uninstall"], [], env)
        assert result.returncode == 0, result.stderr

        config = _load_file("openclaw", canonical)
        assert "channels" not in config
        assert link.is_symlink()
        assert os.readlink(link) == str(canonical)

    def test_dry_run_changes_nothing(self, tmp_path):
        """--dry-run under the Worker layout: the canonical bytes are
        unchanged and the symlink is intact, while stdout still carries
        the projection."""
        specs = ADAPTERS["openclaw"]
        canonical, link = self._worker_layout(tmp_path)
        env = self._worker_env(tmp_path, canonical)
        before = canonical.read_bytes()

        result = _run(specs["install"], ["--dry-run"], env)
        assert result.returncode == 0, result.stderr

        preview = json.loads(result.stdout)
        assert (
            preview["channels"]["matrix"]["homeserver"]
            == CONTRACT_MATRIX["homeserver"]
        )
        assert canonical.read_bytes() == before
        assert link.is_symlink()
        assert os.readlink(link) == str(canonical)

    def test_bundle_installer_detects_openclaw_via_config(self, tmp_path):
        """Entrypoint: no runtime binary on PATH and no config at the
        normal default — only the Worker's OPENCLAW_CONFIG_PATH is
        set, so the package installer must detect OpenClaw through its
        effective config and project the canonical file."""
        canonical = tmp_path / "openclaw.json"
        canonical.write_text(
            json.dumps(
                {"channels": {"matrix": {"homeserver": OLD_HOMESERVER}}},
                indent=2,
                sort_keys=True,
            )
            + "\n",
            encoding="utf-8",
        )
        bin_dir = tmp_path / "bin"
        bin_dir.mkdir()
        env = self._worker_env(tmp_path, canonical)
        # The adapter payload runs `python3` with pyyaml. Wrap this very
        # interpreter (which has pyyaml) in a private bin dir, keeping
        # its own directory — which may hold runtime binaries such as
        # qwenpaw — off PATH.
        interpreter = sys.executable or shutil.which("python3")
        assert interpreter, "no python3 interpreter for the subprocess PATH"
        wrapper = bin_dir / "python3"
        wrapper.write_text(f"#!/bin/sh\nexec {interpreter} \"$@\"\n", encoding="utf-8")
        wrapper.chmod(0o755)
        env["PATH"] = f"{bin_dir}{os.pathsep}/usr/bin{os.pathsep}/bin"
        for name in ("qwenpaw", "openclaw", "hermes"):
            assert shutil.which(name, path=env["PATH"]) is None

        result = _run(BUNDLE_INSTALL, [], env)
        assert result.returncode == 0, result.stderr

        block = _load_file("openclaw", canonical)["channels"]["matrix"]
        for key, value in _expected_projection("openclaw").items():
            assert block[key] == value


def _make_fake_runtime(bin_dir: Path, name: str) -> None:
    fake = bin_dir / name
    fake.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
    fake.chmod(0o755)


def test_bundle_installer_dispatches_selected_adapter(tmp_path):
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    _make_fake_runtime(bin_dir, "openclaw")
    env = _env(tmp_path)
    env["PATH"] = f"{bin_dir}{os.pathsep}{env.get('PATH', '')}"

    config_path = tmp_path / "openclaw.json"
    result = _run(
        BUNDLE_INSTALL,
        ["--adapter", "openclaw", "--config", str(config_path)],
        env,
    )
    assert result.returncode == 0, result.stderr
    config = json.loads(config_path.read_text(encoding="utf-8"))
    block = config["channels"]["matrix"]
    for key, value in _expected_projection("openclaw").items():
        assert block[key] == value


def test_bundle_installer_rejects_unknown_adapter(tmp_path):
    result = _run(BUNDLE_INSTALL, ["--adapter", "nosuch"], _env(tmp_path))
    assert result.returncode != 0
    assert "unknown adapter" in result.stderr


def test_bundle_installer_hermes_stages_native_plugin(tmp_path):
    """--adapter hermes stages the native platform plugin payload
    under HERMES_HOME with the ownership marker (the installer's own
    idempotence/reconciliation coverage lives in test_adapter_hermes.py;
    this is the bundle dispatch)."""
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    _make_fake_runtime(bin_dir, "hermes")
    env = _env(tmp_path)
    env["PATH"] = f"{bin_dir}{os.pathsep}{env.get('PATH', '')}"
    hermes_home = tmp_path / "hermes-home"
    env["HERMES_HOME"] = str(hermes_home)

    result = _run(BUNDLE_INSTALL, ["--adapter", "hermes"], env)
    assert result.returncode == 0, result.stderr

    plugin_dir = hermes_home / "plugins" / "agentteams-matrix"
    for name in ("plugin.yaml", "adapter.py", "__init__.py"):
        assert (plugin_dir / name).is_file(), name
    # The runtime-neutral core ships next to the staged plugin.
    assert (plugin_dir / "core" / "core.py").is_file()
    # Installer artifacts are not part of the staged payload.
    for name in ("install.sh", "uninstall.sh", "README.md"):
        assert not (plugin_dir / name).exists(), name

    marker_path = hermes_home / ".agentteams" / "agentteams-matrix" / "installed.json"
    marker = json.loads(marker_path.read_text(encoding="utf-8"))
    assert marker["runtime"] == "hermes"
    assert marker["files"]["plugin.yaml"]
    assert "install.sh" not in marker["files"]


def test_bundle_installer_hermes_rejects_projector_flags(tmp_path):
    """The removed Hermes projector flags are rejected with a clear
    error instead of being silently ignored (the native adapter takes
    no options)."""
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    _make_fake_runtime(bin_dir, "hermes")
    env = _env(tmp_path)
    env["PATH"] = f"{bin_dir}{os.pathsep}{env.get('PATH', '')}"
    hermes_home = tmp_path / "hermes-home"
    env["HERMES_HOME"] = str(hermes_home)

    for flag, value in [
        ("--hermes-version", "0.22.5"),
        ("--hermes-root", str(tmp_path)),
        ("--hermes-capabilities", "full"),
        ("--allow-partial", None),
    ]:
        args = ["--adapter", "hermes", flag]
        if value is not None:
            args.append(value)
        result = _run(BUNDLE_INSTALL, args, env)
        assert result.returncode != 0, (flag, result.stderr)
        assert flag in result.stderr
    # The rejected calls staged nothing.
    assert not (hermes_home / "plugins").exists()


def test_bundle_uninstaller_dispatches_selected_adapter(tmp_path):
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    _make_fake_runtime(bin_dir, "hermes")
    env = _env(tmp_path)
    env["PATH"] = f"{bin_dir}{os.pathsep}{env.get('PATH', '')}"
    hermes_home = tmp_path / "hermes-home"
    env["HERMES_HOME"] = str(hermes_home)

    specs = ADAPTERS["hermes"]
    _run(specs["install"], [], env)
    plugin_dir = hermes_home / "plugins" / "agentteams-matrix"
    assert (plugin_dir / "plugin.yaml").is_file()

    result = _run(BUNDLE_UNINSTALL, ["--adapter", "hermes"], env)
    assert result.returncode == 0, result.stderr
    assert not plugin_dir.exists()
    # The ownership marker and its state tree go with it.
    assert not (hermes_home / ".agentteams").exists()


def test_bundle_uninstaller_rejects_unknown_adapter(tmp_path):
    result = _run(BUNDLE_UNINSTALL, ["--adapter", "nosuch"], _env(tmp_path))
    assert result.returncode != 0
    assert "unknown adapter" in result.stderr
