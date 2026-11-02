"""Capability-gate and real-parser tests for the hermes adapter (#1458).

The gate follows verified capabilities, not tag strings: a located
runtime tree is probed first (its Matrix adapter source is ground
truth), then an explicit --hermes-capabilities declaration, then a
verified table keyed by tag or package version. Tag strings and
package versions are not comparable across generations (the calendar
tag v2026.9.24 is a newer generation than the semver-shaped v0.22.5,
which declares package version 0.0.0). These tests cover:

  * the shell-level strict/partial capability gate: a basic-capability
    or unknown runtime must never exit 0 while the contract requires
    settings that runtime silently ignores or is not verified to
    honor; unknown fails closed with guidance; --allow-partial
    downgrades that to a warning plus a marker file;
  * the verified table (v2026.9.24 / 0.21.5 full, v2026.4.16 / 0.10.0
    basic), the --hermes-capabilities override, and the tree probe
    (fake trees), including their precedence;
  * uninstall removing exactly what install writes, in both the
    nested (current) and the legacy flat shape, plus the marker;
  * loading the installer's output with the ACTUAL parser of each
    verified runtime generation (no fakes), proving the projected
    settings reach the attributes the adapters read.

Tree resolution per generation (first match wins):

  1. HERMES_PINNED_ROOT / HERMES_20269_ROOT / HERMES_NEW_ROOT — path
     to a hermes-agent checkout (v2026.4.16 / v2026.9.24 / v0.22.5).
  2. HERMES_SDK_CLONE=1 — shallow-clone the tag from
     HERMES_REPO_URL (default: the Dockerfile's
     https://github.com/NousResearch/hermes-agent.git) into a temp dir.
  3. otherwise — the real-parser tests skip with this reason; the
     shell-level gate tests run unconditionally.

The shell-level gate tests assume the test interpreter has no
`gateway` package importable and no `hermes` binary on PATH, so the
installer's tree probe finds no root.

When a runtime's parser cannot be imported in the test interpreter
(missing third-party dependencies), the pinned-tree test falls back
to source-level assertions on the same checkout; the v0.22.5 test
skips with the import error; the v2026.9.24 test still asserts the
installer output, then skips the real read-back with the import error.
"""

import ast
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE_DIR = REPO_ROOT / "plugins" / "agentteams-matrix"
INSTALL = BUNDLE_DIR / "adapters" / "hermes" / "install.sh"
UNINSTALL = BUNDLE_DIR / "adapters" / "hermes" / "uninstall.sh"
CONTRACT_MATRIX = yaml.safe_load(
    (BUNDLE_DIR / "contract.yaml").read_text(encoding="utf-8")
)["matrix"]

HERMES_REPO_URL = "https://github.com/NousResearch/hermes-agent.git"
# kind -> (env var, tag, human label)
TREE_SPECS = {
    "pinned": ("HERMES_PINNED_ROOT", "v2026.4.16", "the pinned v2026.4.16 runtime"),
    "v20269": ("HERMES_20269_ROOT", "v2026.9.24", "the v2026.9.24 runtime (package 0.21.5)"),
    "new": ("HERMES_NEW_ROOT", "v0.22.5", "the v0.22.5 runtime"),
}
MARKER_REL = Path(".agentteams-matrix") / "hermes-install.json"
# The contract settings a basic-capability runtime cannot honor (sorted).
DROPPABLE = ["allowed_rooms", "allowed_users", "require_mention"]
# The complete extra map a full-capability projection carries.
FULL_EXTRA_KEYS = {"homeserver", "user_id", "allowed_users", "allowed_rooms", "require_mention"}


def _bash() -> str:
    return shutil.which("bash") or "/bin/bash"


def _python() -> str:
    return sys.executable or shutil.which("python3") or "python3"


def _env(
    tmp_path: Path,
    bundle_dir: Path | None = None,
    path: str | None = None,
    **overrides: str,
) -> dict:
    env = dict(os.environ)
    env["HOME"] = str(tmp_path)
    if bundle_dir is not None:
        env["AGENTTEAMS_BUNDLE_DIR"] = str(bundle_dir)
    env.pop("AGENTTEAMS_MATRIX_ACCESS_TOKEN", None)
    env.pop("HERMES_VERSION", None)
    env.pop("HERMES_HOME", None)
    if path is not None:
        env["PATH"] = path
    for key, value in overrides.items():
        if value is not None:
            env[key] = value
    return env


def _run(script: Path, args: list, env: dict) -> subprocess.CompletedProcess:
    return subprocess.run(
        [_bash(), str(script), *args],
        env=env,
        cwd=REPO_ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def _config_path(tmp_path: Path) -> Path:
    return tmp_path / "config.yaml"


def _marker_path(tmp_path: Path) -> Path:
    """The install marker the adapter writes next to the runtime config."""
    return _config_path(tmp_path).parent / MARKER_REL


def _bare_path(tmp_path: Path) -> str:
    """A PATH holding only what the installer scripts themselves need
    (bash, python3, dirname) and nothing else, so the version gate's
    `hermes --version` probe finds no binary."""
    bin_dir = tmp_path / "bare-bin"
    bin_dir.mkdir(exist_ok=True)
    for prog in (shutil.which("bash"), shutil.which("python3") or sys.executable, shutil.which("dirname")):
        if prog:
            link = bin_dir / os.path.basename(prog)
            if not link.exists():
                os.symlink(prog, link)
    # The installer invokes `python3` by name; make sure that name
    # resolves (a symlinked venv interpreter may drop its venv context,
    # so _hermetic_env backstops package discovery via PYTHONPATH).
    python3 = bin_dir / "python3"
    if not python3.exists():
        os.symlink(sys.executable, python3)
    return str(bin_dir)


def _hermetic_env(tmp_path: Path, extra_bin: Path | None = None) -> dict:
    """Env for hermetic version-gate tests: a bare PATH (no `hermes`)
    plus a PYTHONPATH pointing at the yaml package's site directory,
    so the interpreter invoked through the bare PATH can import it."""
    path = _bare_path(tmp_path)
    if extra_bin is not None:
        path = f"{extra_bin}{os.pathsep}{path}"
    env = _env(tmp_path, path=path)
    env["PYTHONPATH"] = str(Path(yaml.__file__).resolve().parent.parent)
    return env


def _install(tmp_path: Path, args: list, **env_kw) -> subprocess.CompletedProcess:
    return _run(INSTALL, ["--config", str(_config_path(tmp_path)), *args], _env(tmp_path, **env_kw))


# ---------------------------------------------------------------------------
# Version gate (shell level, no runtime checkout required)
# ---------------------------------------------------------------------------


def test_strict_gate_old_runtime_names_unhonorables(tmp_path):
    result = _install(tmp_path, ["--hermes-version", "v2026.4.16"])
    assert result.returncode != 0, result.stdout
    assert not _config_path(tmp_path).exists(), "strict failure must write nothing"
    for setting in DROPPABLE:
        assert setting in result.stderr, f"gate must name {setting!r}: {result.stderr}"
    assert "basic" in result.stderr, f"gate must name the capability: {result.stderr}"
    assert "0.22.5" in result.stderr
    assert "--allow-partial" in result.stderr


def test_unknown_version_fails_closed_with_guidance(tmp_path):
    result = _run(INSTALL, ["--config", str(_config_path(tmp_path))], _hermetic_env(tmp_path))
    assert result.returncode != 0, result.stdout
    for setting in DROPPABLE:
        assert setting in result.stderr, f"gate must name {setting!r}: {result.stderr}"
    assert "unknown" in result.stderr.lower()
    assert "--hermes-capabilities" in result.stderr, f"gate must offer the override: {result.stderr}"
    assert "--allow-partial" in result.stderr
    assert not _config_path(tmp_path).exists()


def test_calendar_tag_is_treated_as_basic(tmp_path):
    # 2026.4.16 without the v prefix is the same verified basic
    # generation as tag v2026.4.16: the table keys on the normalised
    # string.
    result = _install(tmp_path, ["--hermes-version", "2026.4.16"])
    assert result.returncode != 0, result.stdout
    for setting in DROPPABLE:
        assert setting in result.stderr


def test_flag_version_wins_over_env(tmp_path):
    env = _env(tmp_path)
    env["HERMES_VERSION"] = "v2026.4.16"
    result = _run(
        INSTALL,
        ["--hermes-version", "0.22.5", "--config", str(_config_path(tmp_path))],
        env,
    )
    assert result.returncode == 0, result.stderr
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == {
        "homeserver", "user_id", "allowed_users", "allowed_rooms", "require_mention"
    }


def test_env_version_used_when_flag_absent(tmp_path):
    env = _env(tmp_path)
    env["HERMES_VERSION"] = "0.22.5"
    result = _run(INSTALL, ["--config", str(_config_path(tmp_path))], env)
    assert result.returncode == 0, result.stderr
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == {
        "homeserver", "user_id", "allowed_users", "allowed_rooms", "require_mention"
    }


def test_env_version_old_fails_strict(tmp_path):
    env = _env(tmp_path)
    env["HERMES_VERSION"] = "v2026.4.16"
    result = _run(INSTALL, ["--config", str(_config_path(tmp_path))], env)
    assert result.returncode != 0
    for setting in DROPPABLE:
        assert setting in result.stderr


def test_allow_partial_exits_zero_warns_and_records_marker(tmp_path):
    args = ["--hermes-version", "v2026.4.16", "--allow-partial"]
    result = _run(INSTALL, ["--config", str(_config_path(tmp_path)), *args], _env(tmp_path))
    assert result.returncode == 0, result.stderr
    # The warning names exactly the dropped settings.
    assert "WARNING" in result.stderr
    for setting in DROPPABLE:
        assert setting in result.stderr
    # Only the settings the old runtime can honor were written.
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert block["enabled"] is True
    assert block["extra"] == {
        "homeserver": CONTRACT_MATRIX["homeserver"],
        "user_id": CONTRACT_MATRIX["user_id"],
    }
    # The marker records the partial projection.
    marker_path = _config_path(tmp_path).parent / MARKER_REL
    marker = json.loads(marker_path.read_text(encoding="utf-8"))
    assert marker["dropped"] == DROPPABLE
    assert marker["runtime_version"] == "v2026.4.16"
    # Idempotent: a second partial run is byte-identical.
    config_first = _config_path(tmp_path).read_bytes()
    marker_first = marker_path.read_bytes()
    result = _run(INSTALL, ["--config", str(_config_path(tmp_path)), *args], _env(tmp_path))
    assert result.returncode == 0, result.stderr
    assert _config_path(tmp_path).read_bytes() == config_first
    assert marker_path.read_bytes() == marker_first


def test_old_runtime_without_unhonorables_succeeds_without_marker(tmp_path):
    # A contract without allowlists / require_mention has nothing the
    # old runtime would silently ignore: full success, no marker.
    bundle = tmp_path / "bundle"
    bundle.mkdir()
    contract = yaml.safe_load((BUNDLE_DIR / "contract.yaml").read_text(encoding="utf-8"))
    matrix = contract["matrix"]
    for key in ("allow_from", "group_allow_from", "rooms", "require_mention"):
        del matrix[key]
    (bundle / "contract.yaml").write_text(
        yaml.safe_dump(contract, sort_keys=False, allow_unicode=True), encoding="utf-8"
    )
    result = _run(
        INSTALL,
        ["--hermes-version", "v2026.4.16", "--config", str(_config_path(tmp_path))],
        _env(tmp_path, bundle_dir=bundle),
    )
    assert result.returncode == 0, result.stderr
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert block["extra"] == {
        "homeserver": matrix["homeserver"],
        "user_id": matrix["user_id"],
    }
    assert not (_config_path(tmp_path).parent / MARKER_REL).exists()


def test_full_projection_removes_stale_partial_marker(tmp_path):
    partial_args = ["--hermes-version", "v2026.4.16", "--allow-partial"]
    result = _run(INSTALL, ["--config", str(_config_path(tmp_path)), *partial_args], _env(tmp_path))
    assert result.returncode == 0, result.stderr
    marker_path = _config_path(tmp_path).parent / MARKER_REL
    assert marker_path.is_file()
    result = _run(
        INSTALL,
        ["--hermes-version", "0.22.5", "--config", str(_config_path(tmp_path))],
        _env(tmp_path),
    )
    assert result.returncode == 0, result.stderr
    assert not marker_path.exists(), "stale partial marker must be removed"
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == {
        "homeserver", "user_id", "allowed_users", "allowed_rooms", "require_mention"
    }


def _fake_hermes(bin_dir: Path, version_output: str) -> None:
    fake = bin_dir / "hermes"
    fake.write_text(
        "#!/usr/bin/env bash\n"
        + (f'if [ "${{1:-}}" = "--version" ]; then echo "{version_output}"; fi\n' if version_output else "")
        + "exit 0\n",
        encoding="utf-8",
    )
    fake.chmod(0o755)


def test_binary_probe_reports_new_runtime(tmp_path):
    bin_dir = tmp_path / "probe-bin"
    bin_dir.mkdir()
    _fake_hermes(bin_dir, "hermes 0.22.5")
    env = _hermetic_env(tmp_path, bin_dir)
    result = _run(INSTALL, ["--config", str(_config_path(tmp_path))], env)
    assert result.returncode == 0, result.stderr
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == {
        "homeserver", "user_id", "allowed_users", "allowed_rooms", "require_mention"
    }


def test_binary_probe_silent_output_fails_strict(tmp_path):
    bin_dir = tmp_path / "probe-bin"
    bin_dir.mkdir()
    _fake_hermes(bin_dir, "")
    env = _hermetic_env(tmp_path, bin_dir)
    result = _run(INSTALL, ["--config", str(_config_path(tmp_path))], env)
    assert result.returncode != 0, result.stdout
    for setting in DROPPABLE:
        assert setting in result.stderr


# ---------------------------------------------------------------------------
# Uninstall: both shapes + marker
# ---------------------------------------------------------------------------


def test_uninstall_removes_both_flat_and_nested_shapes(tmp_path):
    config_path = _config_path(tmp_path)
    seeded = {
        "platforms": {
            "matrix": {
                # Legacy flat shape (pre-#1458 installer).
                "enabled": True,
                "token": "legacy-token-value",
                "homeserver": "https://legacy.example",
                "user_id": "@legacy:example",
                "allowed_users": ["@op:example"],
                "allowed_rooms": ["!room:example"],
                "require_mention": True,
                # Nested shape (#1458 installer), incl. a foreign key.
                "extra": {
                    "homeserver": "https://legacy.example",
                    "user_id": "@legacy:example",
                    "allowed_users": ["@op:example"],
                    "allowed_rooms": ["!room:example"],
                    "require_mention": True,
                    "foreign_extra_key": "keep",
                },
                "reply_to_mode": "first",
            }
        }
    }
    config_path.write_text(
        yaml.safe_dump(seeded, sort_keys=False, allow_unicode=True, default_flow_style=False),
        encoding="utf-8",
    )
    result = _run(UNINSTALL, ["--config", str(config_path)], _env(tmp_path))
    assert result.returncode == 0, result.stderr
    block = yaml.safe_load(config_path.read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert block == {"reply_to_mode": "first", "extra": {"foreign_extra_key": "keep"}}


def test_uninstall_removes_partial_marker_and_empty_dir(tmp_path):
    config_path = _config_path(tmp_path)
    partial_args = ["--hermes-version", "v2026.4.16", "--allow-partial"]
    result = _run(INSTALL, ["--config", str(config_path), *partial_args], _env(tmp_path))
    assert result.returncode == 0, result.stderr
    marker_path = config_path.parent / MARKER_REL
    assert marker_path.is_file()

    result = _run(UNINSTALL, ["--config", str(config_path)], _env(tmp_path))
    assert result.returncode == 0, result.stderr
    assert not marker_path.exists()
    assert not marker_path.parent.exists(), "empty marker dir must be removed"
    config = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    assert "platforms" not in config


# ---------------------------------------------------------------------------
# Real-parser tests (require a hermes-agent checkout per generation)
# ---------------------------------------------------------------------------

_PROBE_TEMPLATE = '''
import importlib
import inspect
import json
import sys

block = json.loads(sys.stdin.read())
result = {}
try:
    from gateway.config import PlatformConfig
except Exception as exc:
    print(json.dumps({"import_error": repr(exc)}))
    raise SystemExit(0)
try:
    cfg = PlatformConfig.from_dict(block)
    result["enabled"] = getattr(cfg, "enabled", None)
    result["token"] = getattr(cfg, "token", None)
    result["extra"] = getattr(cfg, "extra", None)
except Exception as exc:
    print(json.dumps({"from_dict_error": repr(exc)}))
    raise SystemExit(0)
if __BUILD_ADAPTER__:
    try:
        mod = importlib.import_module(__ADAPTER_MODULE__)
        built = None
        built_name = None
        for name in dir(mod):
            cls = getattr(mod, name)
            if not (isinstance(cls, type) and name.lower().endswith("adapter")):
                continue
            try:
                params = list(inspect.signature(cls).parameters.values())
            except (TypeError, ValueError):
                continue
            required = [p for p in params if p.default is inspect.Parameter.empty]
            if len(required) != 1:
                continue
            try:
                built = cls(cfg)
                built_name = name
                break
            except Exception:
                continue
        if built is not None:
            result["built"] = built_name
            result["homeserver"] = getattr(built, "_homeserver", None)
            result["user_id"] = getattr(built, "_user_id", None)
            result["require_mention"] = getattr(built, "_require_mention", None)
            result["allowed_rooms"] = sorted(getattr(built, "_allowed_rooms", []) or [])
            result["allowed_user_ids"] = sorted(
                getattr(built, "_allowed_user_ids", []) or []
            )
        else:
            result["build_error"] = "no single-required-arg *Adapter class could be constructed"
    except Exception as exc:
        result["build_error"] = repr(exc)
print(json.dumps(result, default=repr))
'''


def _resolve_tree(kind: str, tmp_path_factory) -> Path:
    env_name, tag, label = TREE_SPECS[kind]
    root = os.environ.get(env_name, "").strip()
    if root:
        path = Path(root)
        if not (path / "gateway" / "config.py").is_file():
            pytest.skip(
                f"{env_name}={root} is not a hermes-agent checkout "
                "(missing gateway/config.py)"
            )
        return path
    if os.environ.get("HERMES_SDK_CLONE") == "1":
        dest = tmp_path_factory.mktemp(f"hermes-{kind}") / "hermes-agent"
        repo_url = os.environ.get("HERMES_REPO_URL", HERMES_REPO_URL)
        proc = subprocess.run(
            ["git", "clone", "--depth", "1", "--branch", tag, repo_url, str(dest)],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if proc.returncode != 0 or not (dest / "gateway" / "config.py").is_file():
            pytest.skip(
                f"could not shallow-clone {tag} for {label} from {repo_url}: "
                f"{proc.stderr.strip()[-200:]}"
            )
        return dest
    pytest.skip(
        f"no hermes-agent checkout for {label}: set {env_name} to a {tag} "
        f"checkout, or run with HERMES_SDK_CLONE=1 to shallow-clone {tag} "
        f"from {os.environ.get('HERMES_REPO_URL', HERMES_REPO_URL)}"
    )


def _parser_probe(
    tree: Path,
    block: dict,
    build_adapter: bool,
    adapter_module: str = "gateway.platforms.matrix",
) -> dict:
    """Run the real PlatformConfig.from_dict of *tree* on *block*.

    A subprocess keeps the runtime trees out of the test interpreter's
    sys.modules; PYTHONPATH points only at the tree. Returns
    {"import_error"/"from_dict_error"/"error": ...} when the real
    parser cannot run here, otherwise the parsed view plus — when
    build_adapter — the constructed adapter's read attributes
    (homeserver, user_id, and, on full-capability adapters,
    require_mention / allowed_rooms / allowed_user_ids).
    *adapter_module* selects the adapter module: gateway.platforms.
    matrix on the basic generation, plugins.platforms.matrix.adapter
    on the full generations.
    """
    probe = _PROBE_TEMPLATE.replace("__BUILD_ADAPTER__", "True" if build_adapter else "False")
    probe = probe.replace("__ADAPTER_MODULE__", repr(adapter_module))
    proc = subprocess.run(
        [_python(), "-c", probe],
        input=json.dumps(block),
        env={**os.environ, "PYTHONPATH": str(tree)},
        cwd=str(tree),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
        timeout=60,
    )
    if proc.returncode != 0:
        return {"error": f"probe exited {proc.returncode}: {proc.stderr.strip()[-400:]}"}
    stdout = proc.stdout.strip()
    if not stdout:
        return {"error": "probe printed nothing"}
    return json.loads(stdout.splitlines()[-1])


def _assert_assign_reads_extra(source: str, attr_name: str, key: str) -> None:
    """Assert some assignment to self.<attr_name> reads config.extra[key]."""
    module = ast.parse(source)
    for node in ast.walk(module):
        targets = []
        if isinstance(node, ast.Assign):
            targets = node.targets
        elif isinstance(node, ast.AnnAssign) and node.value is not None:
            targets = [node.target]
        for target in targets:
            if isinstance(target, ast.Attribute) and target.attr == attr_name:
                segment = ast.get_source_segment(source, node) or ""
                assert key in segment and "extra" in segment, (
                    f"self.{attr_name} is not read from config.extra[{key!r}]: {segment!r}"
                )
                return
    pytest.fail(f"no assignment to self.{attr_name} found in the adapter source")


def _assert_pinned_parser_shape(tree: Path) -> None:
    """Source-level proof for the pinned tree: the parser reads the
    explicit extra map, and the adapter reads homeserver / user_id
    from it."""
    config_source = (tree / "gateway" / "config.py").read_text(encoding="utf-8")
    assert re.search(r'data\.get\(\s*["\']extra["\']\s*,\s*\{\}\s*\)', config_source), (
        "pinned gateway/config.py does not read the explicit extra map "
        '(expected: extra = data.get("extra", {}))'
    )
    adapter_source = (tree / "gateway" / "platforms" / "matrix.py").read_text(encoding="utf-8")
    _assert_assign_reads_extra(adapter_source, "_homeserver", "homeserver")
    _assert_assign_reads_extra(adapter_source, "_user_id", "user_id")


def test_pinned_runtime_parser_consumes_projected_extra(tmp_path, tmp_path_factory):
    """The pinned v2026.4.16 parser must read the projected settings
    from the explicit extra map — the pre-#1458 flat shape lands in
    extra == {} there and is silently ignored."""
    tree = _resolve_tree("pinned", tmp_path_factory)
    config_path = _config_path(tmp_path)
    result = _run(
        INSTALL,
        ["--hermes-version", "v2026.4.16", "--allow-partial", "--config", str(config_path)],
        _env(tmp_path),
    )
    assert result.returncode == 0, result.stderr
    block = yaml.safe_load(config_path.read_text(encoding="utf-8"))["platforms"]["matrix"]

    probe = _parser_probe(tree, block, build_adapter=True)
    if any(key in probe for key in ("import_error", "from_dict_error", "error")):
        # The pinned parser cannot run in this interpreter (missing
        # third-party dependencies); prove the shape at source level.
        _assert_pinned_parser_shape(tree)
        return
    assert probe["enabled"] is True
    assert probe["extra"] == {
        "homeserver": CONTRACT_MATRIX["homeserver"],
        "user_id": CONTRACT_MATRIX["user_id"],
    }, f"pinned parser did not consume the projected extra map: {probe['extra']!r}"
    for setting in DROPPABLE:
        assert setting not in probe["extra"]
    if "built" in probe:
        assert probe["homeserver"], f"adapter _homeserver is empty: {probe!r}"
        assert probe["user_id"], f"adapter _user_id is empty: {probe!r}"
    elif "build_error" in probe:
        # Adapter construction needs dependencies unavailable here;
        # prove the attribute reads at the source level.
        adapter_source = (tree / "gateway" / "platforms" / "matrix.py").read_text(encoding="utf-8")
        _assert_assign_reads_extra(adapter_source, "_homeserver", "homeserver")
        _assert_assign_reads_extra(adapter_source, "_user_id", "user_id")


def test_new_runtime_parser_consumes_all_projected_settings(tmp_path, tmp_path_factory):
    """The v0.22.5 parser must consume all five projected settings from
    the explicit extra map, and the documented precedence (explicit
    extra: wins over promoted flat keys) must hold."""
    tree = _resolve_tree("new", tmp_path_factory)
    config_path = _config_path(tmp_path)
    result = _run(
        INSTALL,
        ["--hermes-version", "0.22.5", "--config", str(config_path)],
        _env(tmp_path),
    )
    assert result.returncode == 0, result.stderr
    block = yaml.safe_load(config_path.read_text(encoding="utf-8"))["platforms"]["matrix"]

    probe = _parser_probe(tree, block, build_adapter=False)
    if any(key in probe for key in ("import_error", "from_dict_error", "error")):
        pytest.skip(
            "v0.22.5 gateway.config cannot be imported in this container: "
            + str(probe.get("import_error") or probe.get("from_dict_error") or probe.get("error"))
        )
    m = CONTRACT_MATRIX
    expected_extra = {
        "homeserver": m["homeserver"],
        "user_id": m["user_id"],
        "allowed_users": list(
            dict.fromkeys(list(m["allow_from"]) + list(m["group_allow_from"]))
        ),
        "allowed_rooms": list(m["rooms"]),
        "require_mention": bool(m["require_mention"]),
    }
    assert probe["enabled"] is True
    for key, value in expected_extra.items():
        assert probe["extra"].get(key) == value, f"v0.22.5 parser did not consume extra.{key}"

    # Source-level: the v0.22.5 matrix adapter reads exactly these keys
    # extra-first (no heavy plugin import needed).
    adapter_source = (tree / "plugins" / "platforms" / "matrix" / "adapter.py").read_text(
        encoding="utf-8"
    )
    assert 'config.extra.get("homeserver"' in adapter_source
    assert 'config.extra.get("user_id"' in adapter_source
    assert '_extra_csv_set(config, "allowed_rooms"' in adapter_source
    assert '_extra_csv_set(config, "allowed_users"' in adapter_source
    assert '_extra_or_secret(config.extra, "require_mention"' in adapter_source

    # Explicit extra: map wins over promoted flat keys, so a legacy
    # flat key beside our extra map cannot contradict the projection.
    conflicting = dict(block)
    conflicting["homeserver"] = "https://flat-must-lose.example"
    probe2 = _parser_probe(tree, conflicting, build_adapter=False)
    assert probe2["extra"].get("homeserver") == m["homeserver"]


# ---------------------------------------------------------------------------
# Capability gate: verified full-generation versions
# ---------------------------------------------------------------------------


def _projected_extra():
    m = CONTRACT_MATRIX
    return {
        "homeserver": m["homeserver"],
        "user_id": m["user_id"],
        "allowed_users": list(
            dict.fromkeys(list(m["allow_from"]) + list(m["group_allow_from"]))
        ),
        "allowed_rooms": list(m["rooms"]),
        "require_mention": bool(m["require_mention"]),
    }


def test_full_calendar_tag_v2026_9_24_projects_all_settings(tmp_path):
    # Maintainer P1 on 69028019: tag v2026.9.24 (package 0.21.5) has a
    # Matrix adapter that reads all three keys; the installer must
    # accept the tag and project the complete contract.
    result = _install(tmp_path, ["--hermes-version", "v2026.9.24"])
    assert result.returncode == 0, result.stdout
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert block["extra"] == _projected_extra()
    assert set(block["extra"]) == set(FULL_EXTRA_KEYS)
    assert not _marker_path(tmp_path).exists(), "full projection must not record a drop"
    assert "capabilities full" in result.stdout


def test_package_version_0_21_5_projects_all_settings(tmp_path):
    # Package versions are first-class table keys: 0.21.5 is the
    # release of tag v2026.9.24.
    result = _install(tmp_path, ["--hermes-version", "0.21.5"])
    assert result.returncode == 0, result.stdout
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == set(_projected_extra().keys())
    assert not _marker_path(tmp_path).exists()


def test_full_generation_with_allow_partial_drops_nothing(tmp_path):
    # A full-capability runtime honours every contract setting, so
    # --allow-partial must change nothing and record nothing.
    result = _install(
        tmp_path, ["--hermes-version", "v2026.9.24", "--allow-partial"]
    )
    assert result.returncode == 0, result.stdout
    assert "WARNING" not in result.stderr, result.stderr
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == set(_projected_extra().keys())
    assert not _marker_path(tmp_path).exists(), "full projection must not record a drop"


def test_unknown_version_string_fails_closed_with_guidance(tmp_path):
    result = _run(
        INSTALL,
        ["--config", str(_config_path(tmp_path)), "--hermes-version", "v2099.1.1"],
        _hermetic_env(tmp_path),
    )
    assert result.returncode != 0, result.stdout
    assert "unknown Hermes generation" in result.stderr
    assert "v2099.1.1" in result.stderr
    for setting in DROPPABLE:
        assert setting in result.stderr, f"gate must name {setting!r}: {result.stderr}"
    assert "--hermes-capabilities" in result.stderr
    assert "--allow-partial" in result.stderr
    assert not _config_path(tmp_path).exists()


def test_capabilities_basic_declaration_fails_strict(tmp_path):
    # Declared capabilities are the same gate as the table: a
    # basic declaration against the full contract is a hard failure.
    result = _run(
        INSTALL,
        [
            "--config",
            str(_config_path(tmp_path)),
            "--hermes-version",
            "v2026.9.24",
            "--hermes-capabilities",
            "basic",
        ],
        _hermetic_env(tmp_path),
    )
    assert result.returncode != 0, result.stdout
    for setting in DROPPABLE:
        assert setting in result.stderr, f"gate must name {setting!r}: {result.stderr}"
    assert "--allow-partial" in result.stderr
    assert not _config_path(tmp_path).exists()


def test_capabilities_full_declaration_succeeds_without_version(tmp_path):
    # The operator states what the runtime supports; no version needed.
    result = _run(
        INSTALL,
        ["--config", str(_config_path(tmp_path)), "--hermes-capabilities", "full"],
        _hermetic_env(tmp_path),
    )
    assert result.returncode == 0, result.stdout
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == set(_projected_extra().keys())
    assert "capabilities full" in result.stdout


def test_invalid_capabilities_value_is_rejected(tmp_path):
    result = _run(
        INSTALL,
        ["--config", str(_config_path(tmp_path)), "--hermes-capabilities", "maybe"],
        _hermetic_env(tmp_path),
    )
    assert result.returncode == 2
    assert "full|basic" in result.stderr


# ---------------------------------------------------------------------------
# Capability gate: read-only tree probe
# ---------------------------------------------------------------------------


def _fake_hermes_tree(tmp_path, layout, adapter_keys=()):
    """A minimal runtime tree whose shape the installer can probe.

    layout: "new" (plugins/platforms/matrix/adapter.py), "old"
    (gateway/platforms/matrix.py) or "none" (empty directory).
    adapter_keys: config.extra keys the new-layout adapter reads.
    """
    root = tmp_path / f"fake-{layout}"
    root.mkdir()
    if layout == "new":
        adapter_dir = root / "plugins" / "platforms" / "matrix"
        adapter_dir.mkdir(parents=True)
        lines = ["class MatrixAdapter:", "    def __init__(self, config):"]
        for key in adapter_keys:
            lines.append(
                '        self._%s = _extra_csv_set(config, "%s", "ENV_%s")'
                % (key, key, key.upper())
            )
        (adapter_dir / "adapter.py").write_text("\n".join(lines) + "\n")
    elif layout == "old":
        old_dir = root / "gateway" / "platforms"
        old_dir.mkdir(parents=True)
        (old_dir / "matrix.py").write_text("MATRIX_REQUIRE_MENTION = 1\n")
    return root


FULL_ADAPTER_KEYS = ("homeserver", "user_id", "allowed_rooms", "allowed_users", "require_mention")


def test_tree_probe_full_beats_verified_table(tmp_path):
    # Ground truth over the table: a full-capability tree with the
    # v2026.4.16 version string installed is full.
    tree = _fake_hermes_tree(tmp_path, "new", adapter_keys=FULL_ADAPTER_KEYS)
    result = _install(
        tmp_path,
        ["--hermes-version", "v2026.4.16", "--hermes-root", str(tree)],
    )
    assert result.returncode == 0, result.stdout
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == set(_projected_extra().keys())
    assert "tree probe" in result.stdout


def test_tree_probe_full_beats_capability_declaration(tmp_path):
    # Ground truth over the explicit declaration as well.
    tree = _fake_hermes_tree(tmp_path, "new", adapter_keys=FULL_ADAPTER_KEYS)
    result = _install(
        tmp_path,
        [
            "--hermes-version",
            "v2026.4.16",
            "--hermes-root",
            str(tree),
            "--hermes-capabilities",
            "basic",
        ],
    )
    assert result.returncode == 0, result.stdout
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == set(_projected_extra().keys())


def test_tree_probe_legacy_layout_is_basic(tmp_path):
    # A full-generation version string cannot paper over a
    # legacy-layout tree: the probe reports basic and the strict
    # gate holds.
    tree = _fake_hermes_tree(tmp_path, "old")
    result = _install(
        tmp_path,
        ["--hermes-version", "0.21.5", "--hermes-root", str(tree)],
    )
    assert result.returncode != 0, result.stdout
    assert "basic" in result.stderr
    for setting in DROPPABLE:
        assert setting in result.stderr
    assert "tree probe" in result.stderr
    assert not _config_path(tmp_path).exists()


def test_tree_probe_new_layout_without_keys_is_basic(tmp_path):
    # An adapter.py that reads none of the three keys is a
    # basic-capability runtime, even at a full version string.
    tree = _fake_hermes_tree(tmp_path, "new", adapter_keys=("homeserver",))
    result = _install(
        tmp_path,
        ["--hermes-version", "0.21.5", "--hermes-root", str(tree)],
    )
    assert result.returncode != 0, result.stdout
    assert "basic" in result.stderr
    assert not _config_path(tmp_path).exists()


def test_tree_probe_no_known_shape_fails_closed(tmp_path):
    # A located root with neither known shape is not a successful
    # probe: the gate stays unknown and fails closed ...
    tree = _fake_hermes_tree(tmp_path, "none")
    result = _install(
        tmp_path,
        ["--hermes-version", "0.21.5", "--hermes-root", str(tree)],
    )
    assert result.returncode != 0, result.stdout
    assert "unknown Hermes generation" in result.stderr
    assert "--hermes-capabilities" in result.stderr
    assert not _config_path(tmp_path).exists()

    # ... but the explicit declaration still applies as an escape hatch.
    result = _install(
        tmp_path,
        [
            "--hermes-version",
            "0.21.5",
            "--hermes-root",
            str(tree),
            "--hermes-capabilities",
            "full",
        ],
    )
    assert result.returncode == 0, result.stdout
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == set(_projected_extra().keys())


def test_explicit_missing_hermes_root_is_an_error(tmp_path):
    result = _install(
        tmp_path,
        ["--hermes-version", "v2026.9.24", "--hermes-root", str(tmp_path / "absent")],
    )
    assert result.returncode != 0, result.stdout
    assert "--hermes-root" in result.stderr
    assert not _config_path(tmp_path).exists()


def test_hermes_home_is_never_probed_as_an_sdk_root(tmp_path):
    """HERMES_HOME is the runtime STATE directory (Worker: ${WORKSPACE}/.hermes).

    A real SDK tree placed there is not evidence: with an unknown version the
    installer must still fail closed rather than probe the state path.
    """
    tree = _fake_hermes_tree(tmp_path, "new", adapter_keys=FULL_ADAPTER_KEYS)
    result = _install(tmp_path, ["--hermes-version", "v2099.1.1"], HERMES_HOME=str(tree))
    assert result.returncode != 0, result.stdout
    assert "unknown Hermes generation" in result.stderr, result.stderr


def test_allow_partial_marker_records_drop_and_capability(tmp_path):
    result = _install(
        tmp_path, ["--hermes-version", "v2026.4.16", "--allow-partial"]
    )
    assert result.returncode == 0, result.stdout
    marker = json.loads(_marker_path(tmp_path).read_text(encoding="utf-8"))
    for key in ("adapter", "bundle", "config", "dropped", "runtime_version", "capabilities", "capability_source"):
        assert key in marker, f"marker must record {key}: {marker}"
    assert marker["dropped"] == DROPPABLE
    assert marker["capabilities"] == "basic"
    assert marker["capability_source"].startswith("verified table")


# ---------------------------------------------------------------------------
# v2026.9.24 real tree: installer output + adapter read-back
# ---------------------------------------------------------------------------


def test_v20269_runtime_installer_and_adapter_readback(tmp_path, tmp_path_factory):
    # The tree is the v2026.9.24 source (package 0.21.5); its Matrix
    # adapter reads allowed_rooms, allowed_users and require_mention
    # from the extra map, so the installer must project everything.
    tree = _resolve_tree("v20269", tmp_path_factory)

    result = _install(tmp_path, ["--hermes-version", "v2026.9.24"])
    assert result.returncode == 0, result.stdout
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    m = CONTRACT_MATRIX
    assert block["extra"] == {
        "homeserver": m["homeserver"],
        "user_id": m["user_id"],
        "allowed_users": list(
            dict.fromkeys(list(m["allow_from"]) + list(m["group_allow_from"]))
        ),
        "allowed_rooms": list(m["rooms"]),
        "require_mention": bool(m["require_mention"]),
    }
    assert not _marker_path(tmp_path).exists()

    # Read-back: build the v2026.9.24 adapter from the projected block
    # and check the three settings survive into the adapter state.
    probe = _parser_probe(
        tree, block, build_adapter=True, adapter_module="plugins.platforms.matrix.adapter"
    )
    if probe.get("import_error") or probe.get("from_dict_error") or probe.get("error"):
        # Dependency unavailable in this environment; the installer
        # assertions above still stand, and the key reads are
        # verified at source level below.
        source = (tree / "plugins" / "platforms" / "matrix" / "adapter.py").read_text(
            encoding="utf-8"
        )
        assert '_extra_csv_set(config, "allowed_rooms"' in source
        assert '_extra_csv_set(config, "allowed_users"' in source
        assert 'config.extra.get("require_mention"' in source
        pytest.skip(
            "v2026.9.24 adapter cannot be built in this container: "
            + str(probe.get("import_error") or probe.get("from_dict_error") or probe.get("error"))
        )
    if "built" not in probe:
        pytest.skip(f"v2026.9.24 adapter could not be built: {probe.get('build_error')}")
    assert probe["homeserver"] == m["homeserver"]
    assert probe["user_id"] == m["user_id"]
    assert probe["require_mention"] is True
    assert probe["allowed_rooms"] == sorted(set(m["rooms"]))
    assert probe["allowed_user_ids"] == sorted(
        set(list(m["allow_from"]) + list(m["group_allow_from"]))
    )


# ---------------------------------------------------------------------------
# Worker environment: HERMES_HOME is a state directory, never an SDK root
# ---------------------------------------------------------------------------


def _fake_install_root(tmp_path: Path, shape: str) -> Path:
    """A directory that looks like an installed Hermes importable via PYTHONPATH."""
    root = tmp_path / "fake-install"
    (root / "gateway").mkdir(parents=True, exist_ok=True)
    (root / "gateway" / "__init__.py").write_text("", encoding="utf-8")
    if shape == "full":
        adapter = root / "plugins" / "platforms" / "matrix" / "adapter.py"
        adapter.parent.mkdir(parents=True, exist_ok=True)
        adapter.write_text(
            'config.extra.get("homeserver")\n'
            'config.extra.get("allowed_users")\n'
            'config.extra.get("allowed_rooms")\n'
            'config.extra.get("require_mention")\n',
            encoding="utf-8",
        )
    elif shape == "basic":
        legacy = root / "gateway" / "platforms" / "matrix.py"
        legacy.parent.mkdir(parents=True, exist_ok=True)
        legacy.write_text('config.extra.get("homeserver")\n', encoding="utf-8")
    return root


def test_worker_state_directory_is_not_an_sdk_root(tmp_path):
    """The Worker entrypoint sets HERMES_HOME to ${WORKSPACE}/.hermes (state).

    That directory must never be probed as an SDK tree: with a supported
    version the installer has to project the full contract from the verified
    version evidence even when the state directory exists.
    """
    state = tmp_path / "workspace" / ".hermes"
    state.mkdir(parents=True)
    (state / "config.yaml").write_text("platforms: {}\n", encoding="utf-8")

    result = _install(
        tmp_path,
        ["--hermes-version", "v2026.9.24"],
        HERMES_HOME=str(state),
        PYTHONPATH=str(tmp_path / "no-sdk-here"),
    )
    assert result.returncode == 0, result.stderr
    assert "unknown Hermes generation" not in result.stderr
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == set(_projected_extra())
    # The marker is written only for a reduced projection; a full projection
    # needs no record, so the success above is the evidence.


def test_shapeless_importable_package_falls_back_to_the_table(tmp_path):
    """An importable `gateway` without a Matrix shape is no evidence at all."""
    root = _fake_install_root(tmp_path, "none")
    result = _install(
        tmp_path,
        ["--hermes-version", "v2026.9.24"],
        PYTHONPATH=str(root),
    )
    assert result.returncode == 0, result.stderr
    assert "unknown Hermes generation" not in result.stderr


def test_implicit_install_probe_decides_a_calendar_tag(tmp_path):
    """A discovered install with the plugin adapter outranks the version table."""
    root = _fake_install_root(tmp_path, "full")
    result = _install(
        tmp_path,
        ["--hermes-version", "v2099.1.1"],
        PYTHONPATH=str(root),
    )
    assert result.returncode == 0, result.stderr
    block = yaml.safe_load(_config_path(tmp_path).read_text(encoding="utf-8"))["platforms"]["matrix"]
    assert set(block["extra"]) == set(_projected_extra())
