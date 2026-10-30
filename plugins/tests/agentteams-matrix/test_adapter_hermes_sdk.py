"""Real-SDK compatibility tests for the Hermes platform adapter.

``test_adapter_hermes.py`` is the behaviour suite: it injects fake
``gateway.*`` modules so the adapter logic can run without a Hermes
runtime.  This module is the compatibility half of that contract: it
loads the adapter against a **real Hermes source tree**, so a missing
SDK symbol, an unknown ``PlatformEntry`` kwarg, a
``PluginContext.register_platform`` call that never lands in the
registry, or a ``Platform`` member that never resolves is caught here
instead of at deploy time (maintainer P1 on #1469: the pinned
``v2026.4.16`` Worker runtime does not have the API surface this
adapter imports).

SDK tree resolution (never faked):

* ``HERMES_SDK_ROOT`` — explicit path to a hermes-agent checkout;
* otherwise ``HERMES_SDK_CLONE=1`` — shallow-clone
  ``https://github.com/NousResearch/hermes-agent.git`` at
  ``HERMES_SDK_REF`` (default ``v0.22.5``, the declared minimum) into a
  temp dir;
* otherwise the sibling local checkout ``SC/Hermes`` when it exists.

No tree available → the whole module skips with a clear reason; a tree
available but a symbol missing → the tests fail (that is the point).
"""

from __future__ import annotations

import ast
import asyncio
import dataclasses
import importlib
import importlib.util
import inspect
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
ADAPTER_PATH = REPO_ROOT / "plugins" / "agentteams-matrix" / "adapters" / "hermes" / "adapter.py"

DEFAULT_SDK_REF = "v0.22.5"
SDK_REPO_URL = "https://github.com/NousResearch/hermes-agent.git"

# The declared API surface: every SDK symbol the adapter relies on,
# including the shared helpers it calls at runtime (the import list in
# adapter.py is checked separately, AST-driven, below).
EXPECTED_SDK_SURFACE = {
    "gateway.config": ("Platform",),
    "gateway.platforms._shared": (
        "get_scoped_secret",
        "extra_or_secret",
        "seed_extra_from_env",
        "apply_yaml_bridge",
        "env_is_connected",
        "send_error",
    ),
    "gateway.platforms.base": ("BasePlatformAdapter", "SendResult"),
    "gateway.platforms.event": ("MessageEvent", "MessageType"),
}

# The exact kwargs the adapter's register() passes to
# PluginContext.register_platform (see adapters/hermes/adapter.py).
EXPECTED_REGISTER_KWARGS = frozenset({
    "name", "label", "adapter_factory", "check_fn", "validate_config",
    "is_connected", "required_env", "install_hint", "env_enablement_fn",
    "cron_deliver_env_var", "standalone_sender_fn", "allowed_users_env",
    "allow_all_env", "max_message_length", "emoji", "pii_safe",
    "allow_update_command", "platform_hint",
})

# Every env var the adapter reads; the unconfigured-connect test scrubs all of them.
ADAPTER_ENV_VARS = (
    "AGENTTEAMS_MATRIX_SERVER",
    "AGENTTEAMS_MATRIX_USER_ID",
    "AGENTTEAMS_MATRIX_TOKEN",
    "AGENTTEAMS_MATRIX_DEVICE_ID",
    "AGENTTEAMS_MATRIX_STATE_DIR",
    "AGENTTEAMS_MATRIX_ENCRYPTION",
    "AGENTTEAMS_MATRIX_HISTORY_LIMIT",
    "AGENTTEAMS_MATRIX_SYNC_TIMEOUT_MS",
    "AGENTTEAMS_MATRIX_HOME_ROOM",
    "AGENTTEAMS_MATRIX_ALLOWED_USERS",
    "AGENTTEAMS_MATRIX_ALLOW_ALL_USERS",
)


def _looks_like_hermes_root(path: Path) -> bool:
    return (path / "gateway" / "platforms" / "_shared.py").is_file() and (
        path / "hermes_cli" / "plugins.py"
    ).is_file()


def _describe_ref(root: Path) -> str:
    """Best-effort git ref description for reporting; never fails."""
    try:
        head = (root / ".git" / "HEAD").read_text(encoding="utf-8").strip()
    except OSError:
        return "unknown"
    if head.startswith("ref: "):
        return head[len("ref: "):].strip()
    return f"detached@{head[:12]}"


def _local_sdk_root():
    """The sibling local checkout (``SC/Hermes``), when it exists.  A
    worktree checkout adds ``.worktrees/<name>`` levels below the repo's
    parent, so walk the repo's ancestors and take the first ``Hermes``
    child that looks like a hermes-agent root."""
    for parent in REPO_ROOT.parents:
        candidate = parent / "Hermes"
        if _looks_like_hermes_root(candidate):
            return candidate
    return None


def _resolve_sdk_root():
    """(root, ref, temp_dir) — root is None when there is no tree; then
    ref carries the skip reason."""
    ref = os.environ.get("HERMES_SDK_REF", "").strip() or DEFAULT_SDK_REF
    explicit = os.environ.get("HERMES_SDK_ROOT", "").strip()
    if explicit:
        root = Path(explicit).expanduser()
        if _looks_like_hermes_root(root):
            return root, ref, None
        return None, f"HERMES_SDK_ROOT={explicit} is not a hermes-agent checkout", None
    if os.environ.get("HERMES_SDK_CLONE", "").strip() == "1":
        temp_dir = Path(tempfile.mkdtemp(prefix="hermes-sdk-"))
        proc = subprocess.run(
            ["git", "clone", "--depth", "1", "--branch", ref, SDK_REPO_URL, str(temp_dir)],
            capture_output=True,
            text=True,
        )
        if proc.returncode != 0 or not _looks_like_hermes_root(temp_dir):
            shutil.rmtree(temp_dir, ignore_errors=True)
            return None, f"git clone of {SDK_REPO_URL} @ {ref} failed: {proc.stderr.strip()[:300]}", None
        return temp_dir, ref, temp_dir
    local_root = _local_sdk_root()
    if local_root is not None:
        return local_root, ref, None
    return None, (
        "no real Hermes SDK tree available: set HERMES_SDK_ROOT to a hermes-agent checkout, "
        f"or HERMES_SDK_CLONE=1 to clone {SDK_REPO_URL} @ {ref}, or provide a local "
        f"checkout (SC/Hermes) next to this repo"
    ), None


@pytest.fixture(scope="module")
def sdk():
    """Load the real Hermes SDK modules plus the adapter from file with
    the SDK tree first on ``sys.path``; restore process state after the
    module so the fake-SDK behaviour suite is unaffected."""
    root, ref, temp_dir = _resolve_sdk_root()
    if root is None:
        pytest.skip(ref)
    saved_path = list(sys.path)
    saved_modules = set(sys.modules)
    sys.path.insert(0, str(root))
    try:
        config_mod = importlib.import_module("gateway.config")
        shared_mod = importlib.import_module("gateway.platforms._shared")
        base_mod = importlib.import_module("gateway.platforms.base")
        event_mod = importlib.import_module("gateway.platforms.event")
        registry_mod = importlib.import_module("gateway.platform_registry")
        plugins_mod = importlib.import_module("hermes_cli.plugins")
        spec = importlib.util.spec_from_file_location(
            "agentteams_hermes_adapter_sdk_under_test", ADAPTER_PATH
        )
        adapter_mod = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = adapter_mod
        spec.loader.exec_module(adapter_mod)
        yield SimpleNamespace(
            root=root,
            ref=ref or _describe_ref(root),
            config_mod=config_mod,
            shared_mod=shared_mod,
            base_mod=base_mod,
            event_mod=event_mod,
            registry_mod=registry_mod,
            plugins_mod=plugins_mod,
            adapter_mod=adapter_mod,
        )
    finally:
        sys.path[:] = saved_path
        # Drop every module this fixture imported (SDK closure + adapter)
        # so the rest of the session sees the pre-fixture process state.
        for name in set(sys.modules) - saved_modules:
            sys.modules.pop(name, None)
        if temp_dir is not None:
            shutil.rmtree(temp_dir, ignore_errors=True)


class _StubManifest:
    name = "agentteams-matrix"
    source = "user"


def _stub_context(registry):
    """Minimal plugin context carrying only what the real
    ``PluginContext.register_platform`` touches: ``manifest.name`` /
    ``manifest.source`` and the manager's scope bookkeeping.  The
    ``register_platform`` method itself is the real one (unbound call
    on this stub), as is the ``platform_registry`` it writes to."""

    class _StubManager:
        def __init__(self):
            self.scope_key = registry.current_scope_key()
            self._plugin_platform_names = set()

        def _track_scoped_registration(self, *args, **kwargs):
            return None

        def _remove_platform_name_if_unowned(self, name):
            self._plugin_platform_names.discard(name)

    return SimpleNamespace(manifest=_StubManifest(), _manager=_StubManager())


@pytest.fixture(scope="module")
def registered(sdk):
    """Register the adapter's platform through the REAL
    ``PluginContext.register_platform`` (the adapter's own
    ``register()`` entry point, 18 kwargs, real ``PlatformEntry``, real
    ``platform_registry``); unregister on teardown."""
    registry = sdk.registry_mod.platform_registry
    name = sdk.adapter_mod.PLATFORM_NAME
    assert not registry.is_registered(name), "test isolation: name already registered"
    sdk.adapter_mod.register(_stub_context(registry))
    try:
        yield sdk, name
    finally:
        registry.unregister(name, scope=registry.current_scope_key())


def _adapter_register_kwargs() -> dict:
    """The exact kwargs the adapter's register() passes to
    ``ctx.register_platform`` — parsed from the adapter source, not
    re-typed."""
    tree = ast.parse(ADAPTER_PATH.read_text(encoding="utf-8"))
    for node in ast.walk(tree):
        if isinstance(node, ast.FunctionDef) and node.name == "register":
            for sub in ast.walk(node):
                if (
                    isinstance(sub, ast.Call)
                    and isinstance(sub.func, ast.Attribute)
                    and sub.func.attr == "register_platform"
                    and all(kw.arg is not None for kw in sub.keywords)
                ):
                    return {kw.arg: kw.value for kw in sub.keywords}
    raise AssertionError("register_platform call not found in adapter.py")


def test_sdk_tree_available(sdk):
    print(f"real-SDK tree: {sdk.root} (ref: {sdk.ref})")
    assert _looks_like_hermes_root(sdk.root)


def test_expected_sdk_surface_exists(sdk):
    for module_name, symbols in EXPECTED_SDK_SURFACE.items():
        module = importlib.import_module(module_name)
        for symbol in symbols:
            assert hasattr(module, symbol), (
                f"{module_name}.{symbol} missing from the real SDK at {sdk.root}"
            )


def test_adapter_gateway_imports_resolve(sdk):
    """Every ``from gateway...`` import in the adapter source resolves
    on the real SDK (AST-driven, so a new adapter import is covered
    automatically)."""
    tree = ast.parse(ADAPTER_PATH.read_text(encoding="utf-8"))
    checked = 0
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom) and node.module and node.module.startswith("gateway"):
            module = importlib.import_module(node.module)
            for alias in node.names:
                assert hasattr(module, alias.name), (
                    f"adapter import {node.module}.{alias.name} missing from the real SDK"
                )
                checked += 1
    assert checked >= 8, f"expected the adapter's gateway imports to be checked, saw {checked}"


def test_platformentry_binds_adapter_kwargs(sdk):
    kwargs = _adapter_register_kwargs()
    assert set(kwargs) == EXPECTED_REGISTER_KWARGS, (
        f"adapter register() kwarg set changed: {sorted(set(kwargs) ^ EXPECTED_REGISTER_KWARGS)}"
    )
    entry_fields = {f.name for f in dataclasses.fields(sdk.registry_mod.PlatformEntry)}
    missing = set(kwargs) - entry_fields
    assert not missing, f"PlatformEntry lacks field(s): {sorted(missing)}"
    # Name-level bind against the real signature (values irrelevant to
    # binding; the registered fixture does the real-value bind).
    inspect.signature(sdk.registry_mod.PlatformEntry).bind_partial(
        **{name: object() for name in kwargs}
    )


def test_register_platform_signature_binds(sdk):
    """The adapter's exact call shape binds to the real
    ``PluginContext.register_platform`` signature: the first kwargs are
    real parameters, the rest must fit ``**entry_kwargs``."""
    kwargs = _adapter_register_kwargs()
    signature = inspect.signature(sdk.plugins_mod.PluginContext.register_platform)
    signature.bind_partial(None, **{name: object() for name in kwargs})


def test_register_and_platform_member(registered):
    sdk, name = registered
    registry = sdk.registry_mod.platform_registry
    assert registry.is_registered(name)
    # The dynamic-member path the maintainer flagged: the real enum's
    # _missing_ must resolve the runtime-registered name.
    member = sdk.config_mod.Platform(name)
    assert member is not None
    assert member.value == name
    # Pseudo members are cached: same object on repeat lookup.
    assert sdk.config_mod.Platform(name) is member
    entry = registry.get(name)
    assert entry is not None
    assert entry.adapter_factory is sdk.adapter_mod.AgentTeamsMatrixAdapter


def test_factory_returns_real_base_adapter(registered):
    sdk, name = registered
    entry = sdk.registry_mod.platform_registry.get(name)
    config = sdk.config_mod.PlatformConfig()
    adapter = entry.adapter_factory(config)
    assert isinstance(adapter, sdk.base_mod.BasePlatformAdapter)
    assert adapter.platform.value == name
    assert inspect.iscoroutinefunction(adapter.connect)
    assert inspect.iscoroutinefunction(adapter.disconnect)


def test_connect_unconfigured_fails_clean(registered, monkeypatch):
    """With no env configured, connect() fails fast with a fatal error
    and no network; disconnect() is a clean no-op."""
    sdk, _ = registered
    for var in ADAPTER_ENV_VARS:
        monkeypatch.delenv(var, raising=False)
    entry = sdk.registry_mod.platform_registry.get(sdk.adapter_mod.PLATFORM_NAME)
    adapter = entry.adapter_factory(sdk.config_mod.PlatformConfig())
    assert asyncio.run(adapter.connect()) is False
    assert adapter.fatal_error_code == "config_missing"
    assert sdk.adapter_mod.ENV_SERVER in adapter.fatal_error_message
    assert not adapter.fatal_error_retryable
    asyncio.run(adapter.disconnect())
    assert not adapter._running
