#!/usr/bin/env bash
# Idempotent config projection of the agentteams-matrix bundle contract
# onto the Hermes runtime.
#
# The adapter reads the bundle contract (contract.yaml, matrix: section)
# and merges its settings into the platforms.matrix block of the Hermes
# config file, nested under the block's explicit `extra:` map — the one
# shape both supported runtime generations consume (see README.md,
# "Config shape and runtime capabilities"). It is a pure file projector:
# no network access is performed. The only runtime interactions are a
# best-effort, read-only tree probe of the Hermes install and an
# optional `hermes --version` probe, both used to resolve the target
# runtime's capability.
#
# Runtime capabilities (verified on upstream tags, 2026-10-30):
#
#   * full  — the plugin-API generation whose Matrix adapter
#     (plugins/platforms/matrix/adapter.py) reads allowed_rooms,
#     allowed_users and require_mention from the config's extra map.
#     Verified on tag v2026.9.24 (package version 0.21.5) and tag
#     v0.22.5 (package version 0.0.0).
#   * basic — the older generation whose Matrix adapter
#     (gateway/platforms/matrix.py) has no allowlist feature and takes
#     require_mention only from the MATRIX_REQUIRE_MENTION env var.
#     Verified on the Worker pin tag v2026.4.16 (package version 0.10.0).
#
# Tag strings and package versions are NOT comparable across
# generations: the calendar tag v2026.9.24 is a newer generation than
# the semver-shaped tag v0.22.5, and the latter declares package
# version 0.0.0 in its pyproject.toml. The gate therefore never compares
# version numbers; it resolves the target runtime's capability in this
# order (the first decisive source wins):
#
#   1. tree probe — a located Hermes SDK root (--hermes-root, or an
#      implicitly discovered install: an importable `gateway` package, the
#      `hermes` executable's venv, or a conventional Worker venv). Matrix
#      adapter source
#      detects as full or basic. Ground truth: it outranks
#      --hermes-capabilities and the version table. A root that exists
#      but has no detectable Matrix shape is not a successful probe —
#      the installer never guesses (the explicit --hermes-capabilities
#      declaration still applies; the version table does not, because
#      the version string may describe a different install than the
#      located tree).
#   2. --hermes-capabilities=full|basic — an explicit declaration.
#   3. verified table — the version from --hermes-version /
#      HERMES_VERSION / `hermes --version`, matched by tag or by
#      package version: v2026.4.16 / 0.10.0 -> basic; v2026.9.24 /
#      0.21.5 -> full; v0.22.5 / 0.0.0 -> full.
#   4. unknown — fail closed: when the contract requires
#      full-capability settings the installer exits non-zero with
#      guidance (never a silent downgrade); otherwise the projection is
#      safe as-is, and --allow-partial accepts a reduced projection.
#
# Usage:
#   install.sh [--config <path>] [--dry-run] [--with-credentials]
#              [--hermes-version <version>] [--hermes-root <path>]
#              [--hermes-capabilities full|basic] [--allow-partial]
#
#   --config <path>        target config file
#                          (default: $HOME/.hermes/config.yaml)
#   --dry-run              print the resulting YAML to stdout, write nothing
#   --with-credentials     also write the access token read from
#                          $AGENTTEAMS_MATRIX_ACCESS_TOKEN; the config
#                          file is written 0600 whenever it carries
#                          credential values
#   --hermes-version <v>   target hermes-agent version, as a tag
#                          (v2026.9.24) or a package version (0.21.5).
#                          Overrides the HERMES_VERSION env var and the
#                          `hermes --version` probe. Consulted only when
#                          the tree probe and --hermes-capabilities do
#                          not decide (see the resolution order above).
#   --hermes-root <path>   root of the target Hermes SDK install to probe.
#                          The directory must exist; its Matrix adapter
#                          source decides the capability. HERMES_HOME is
#                          NOT a source root: the Worker sets it to its
#                          state directory (${WORKSPACE}/.hermes).
#   --hermes-capabilities <full|basic>
#                          explicit capability declaration for the
#                          target runtime. Outranks the verified
#                          version table; a successful tree probe
#                          outranks it.
#   --allow-partial        on a basic-capability runtime (or an unknown
#                          one), project only the settings that runtime
#                          can (or is verified to be able to) honor and
#                          record the dropped ones in
#                          <config dir>/.agentteams-matrix/hermes-install.json
#                          (without it, the installer exits non-zero when
#                          the contract requires settings the runtime
#                          cannot honor)
#
# Only the keys this adapter owns are merged into the platforms.matrix
# block; every other key in the config file is preserved, including
# foreign keys already present inside the block or inside its existing
# extra map. Re-running with an unchanged contract produces a
# byte-identical file.
# Previews (--dry-run) always redact credential values in the rendered
# document: any non-empty string under a key ending in token/secret/
# password/passwd/apikey/credentials (case-insensitive) is printed as
# <redacted>, including credentials in blocks this adapter does not own;
# the file itself is written unredacted. The atomic write never loosens
# permissions: a document that carries credential values lands at 0600,
# and an existing restrictive mode survives every rewrite. By default no
# credential material is written at all (the contract's
# access_token_ref has no Hermes key — see README.md).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"

CONFIG_PATH="${HOME}/.hermes/config.yaml"
DRY_RUN=0
WITH_CREDENTIALS=0
ALLOW_PARTIAL=0
HERMES_VERSION_ARG=""
HERMES_ROOT_ARG=""
HERMES_CAPABILITIES_ARG=""

usage() {
  cat <<'EOF'
Usage: install.sh [--config <path>] [--dry-run] [--with-credentials]
                  [--hermes-version <version>] [--hermes-root <path>]
                  [--hermes-capabilities full|basic] [--allow-partial]

Projects the agentteams-matrix contract onto the platforms.matrix block
of the Hermes config file (default: $HOME/.hermes/config.yaml).

Settings are written into the block's explicit `extra:` map, which both
supported runtime generations consume. Basic-capability runtimes
(including the Worker pin v2026.4.16) cannot honor allowed_users /
allowed_rooms / require_mention; when the contract requires them the
installer exits non-zero unless --allow-partial is passed, in which
case the dropped settings are recorded in
<config dir>/.agentteams-matrix/hermes-install.json.

Runtime capability is resolved in this order (first decisive source
wins):
  1. tree probe — a located Hermes SDK root (--hermes-root, or an
     implicitly discovered install: an importable `gateway` package, the
     `hermes` binary's venv, or a conventional Worker venv) whose Matrix
     adapter source detects as full or basic (ground truth; outranks
     everything below). HERMES_HOME is a state directory, never a source;
  2. --hermes-capabilities full|basic — an explicit declaration;
  3. verified table — the version from --hermes-version /
     HERMES_VERSION / `hermes --version`, matched by tag or by package
     version (v2026.4.16 / 0.10.0 = basic; v2026.9.24 / 0.21.5 = full;
     v0.22.5 / 0.0.0 = full);
  4. unknown — fails closed with guidance instead of silently dropping
     contract settings; --allow-partial accepts a reduced projection.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --config)
      if [ $# -lt 2 ]; then
        echo "ERROR: --config requires a path" >&2
        exit 2
      fi
      CONFIG_PATH="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --with-credentials)
      WITH_CREDENTIALS=1
      shift
      ;;
    --hermes-version)
      if [ $# -lt 2 ]; then
        echo "ERROR: --hermes-version requires a value" >&2
        exit 2
      fi
      HERMES_VERSION_ARG="$2"
      shift 2
      ;;
    --hermes-root)
      if [ $# -lt 2 ]; then
        echo "ERROR: --hermes-root requires a path" >&2
        exit 2
      fi
      HERMES_ROOT_ARG="$2"
      shift 2
      ;;
    --hermes-capabilities)
      if [ $# -lt 2 ]; then
        echo "ERROR: --hermes-capabilities requires a value (full or basic)" >&2
        exit 2
      fi
      case "$2" in
        full|basic) ;;
        *)
          echo "ERROR: --hermes-capabilities takes full|basic (got: $2)" >&2
          exit 2
          ;;
      esac
      HERMES_CAPABILITIES_ARG="$2"
      shift 2
      ;;
    --allow-partial)
      ALLOW_PARTIAL=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

CONTRACT_PATH="${AGENTTEAMS_BUNDLE_DIR}/contract.yaml"
if [ ! -f "${CONTRACT_PATH}" ]; then
  echo "ERROR: bundle contract not found at ${CONTRACT_PATH}; set AGENTTEAMS_BUNDLE_DIR to the agentteams-matrix bundle directory" >&2
  exit 1
fi

python3 - "${CONFIG_PATH}" "${CONTRACT_PATH}" "${DRY_RUN}" "${WITH_CREDENTIALS}" "${ALLOW_PARTIAL}" "${HERMES_VERSION_ARG}" "${HERMES_ROOT_ARG}" "${HERMES_CAPABILITIES_ARG}" <<'PY'
import importlib.util
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

config_path = Path(sys.argv[1])
contract_path = Path(sys.argv[2])
dry_run = sys.argv[3] == "1"
with_credentials = sys.argv[4] == "1"
allow_partial = sys.argv[5] == "1"
version_arg = sys.argv[6]
hermes_root_arg = sys.argv[7]
capabilities_arg = sys.argv[8]

TOKEN_KEY = "token"
MARKER_DIR_NAME = ".agentteams-matrix"
MARKER_NAME = "hermes-install.json"
# Contract settings a basic-capability runtime cannot honor from the
# config file: the user/room allowlists do not exist in that
# generation and require_mention is env-only (MATRIX_REQUIRE_MENTION).
UNHONORABLE_SETTINGS = ("allowed_users", "allowed_rooms", "require_mention")
# The same settings, as the full-capability generation's adapter reads
# them from its extra map (used by the tree probe).
FULL_CAPABILITY_KEYS = ("allowed_rooms", "allowed_users", "require_mention")
# Verified runtime capability table, keyed by the normalised version
# string (tag with the leading "v" stripped, or the pyproject.toml
# package version). Verified against upstream tags on 2026-10-30:
#   tag v2026.4.16 (Worker pin) -> package 0.10.0: basic
#   tag v2026.9.24              -> package 0.21.5: full
#   tag v0.22.5                 -> package 0.0.0:  full
# Tag strings and package versions are not comparable across
# generations (a calendar tag can be the newest generation and a
# semver-looking tag can declare package version 0.0.0), so the gate
# matches this table instead of comparing version numbers. It is
# consulted only when the tree probe found no root and no explicit
# --hermes-capabilities declaration was given.
VERIFIED_CAPABILITIES = {
    "2026.4.16": "basic",  # tag v2026.4.16 (Worker pin)
    "0.10.0": "basic",     # package version declared by tag v2026.4.16
    "2026.9.24": "full",   # tag v2026.9.24
    "0.21.5": "full",      # package version declared by tag v2026.9.24
    "0.22.5": "full",      # tag v0.22.5
    "0.0.0": "full",       # package version declared by tag v0.22.5
}

CREDENTIAL_SUFFIXES = ("token", "secret", "password", "passwd", "apikey", "credentials")
REDACTED = "<redacted>"


def fail(message):
    print(f"ERROR: {message}", file=sys.stderr)
    sys.exit(1)


def _is_credential_key(key):
    normalized = "".join(ch for ch in str(key).lower() if ch.isalnum())
    return normalized.endswith(CREDENTIAL_SUFFIXES)


def redact(document):
    """Deep copy with credential values replaced by REDACTED (previews only)."""
    if isinstance(document, dict):
        out = {}
        for key, value in document.items():
            if _is_credential_key(key) and isinstance(value, str) and value:
                out[key] = REDACTED
            else:
                out[key] = redact(value)
        return out
    if isinstance(document, list):
        return [redact(item) for item in document]
    return document


def _document_has_credentials(document):
    if isinstance(document, dict):
        return any(
            (_is_credential_key(key) and isinstance(value, str) and value)
            or _document_has_credentials(value)
            for key, value in document.items()
        )
    if isinstance(document, list):
        return any(_document_has_credentials(item) for item in document)
    return False


def write_atomic(path, text, document):
    """Atomic write. 0600 whenever the document carries credentials, otherwise
    preserve the target's mode (0600 for a new file). The temp file is created
    0600 by mkstemp and only then chmod-ed to the final mode."""
    path.parent.mkdir(parents=True, exist_ok=True)
    if _document_has_credentials(document):
        mode = 0o600
    elif path.exists():
        mode = stat.S_IMODE(path.stat().st_mode)
    else:
        mode = 0o600
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=path.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(text)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


try:
    import yaml
except ImportError:
    fail("pyyaml is required to read the bundle contract; install pyyaml>=6.0")


def render(document):
    return yaml.safe_dump(
        document,
        sort_keys=False,
        allow_unicode=True,
        default_flow_style=False,
    )


try:
    contract = yaml.safe_load(contract_path.read_text(encoding="utf-8"))
except yaml.YAMLError as exc:
    fail(f"bundle contract {contract_path} is not valid YAML: {exc}")
if not isinstance(contract, dict):
    fail(f"bundle contract {contract_path} must be a YAML mapping")
matrix = contract.get("matrix")
if not isinstance(matrix, dict) or not matrix:
    fail(f"bundle contract {contract_path} has no 'matrix' section")

# contract.yaml -> config.yaml platforms.matrix block. The settings are
# projected into the block's explicit `extra:` map — the one shape both
# supported generations consume:
#
#   * basic generation (the Worker pin v2026.4.16):
#     PlatformConfig.from_dict reads extra = data.get("extra", {}) and
#     ignores every other non-typed top-level key — flat settings are
#     silently dropped (#1458);
#   * full generation (verified on v2026.9.24 and v0.22.5):
#     PlatformConfig.from_dict promotes non-typed top-level keys into
#     extra, but an explicit `extra:` map wins on a clash, so the
#     nested shape is authoritative there too.
#
# Flat keys are therefore NOT written (see README.md, "Config shape and
# runtime capabilities").
projected_extra = {}
# homeserver — consumed via config.extra on both generations.
if matrix.get("homeserver") is not None:
    projected_extra["homeserver"] = matrix["homeserver"]
# user_id — consumed via config.extra on both generations.
if matrix.get("user_id") is not None:
    projected_extra["user_id"] = matrix["user_id"]
# allow_from + group_allow_from -> allowed_users: the adapter has a
# single user allowlist for DMs and groups, so the two contract lists
# are merged (deduped, contract order preserved). YAML lists are
# accepted by the adapter's CSV-set reader.
allowed_users = list(
    dict.fromkeys(list(matrix.get("allow_from") or []) + list(matrix.get("group_allow_from") or []))
)
if allowed_users:
    projected_extra["allowed_users"] = allowed_users
# rooms -> allowed_rooms — room-ID gate, DMs exempt (full-capability
# runtimes only).
rooms = list(matrix.get("rooms") or [])
if rooms:
    projected_extra["allowed_rooms"] = rooms
# require_mention — declared value projected as-is (full-capability
# runtimes only; basic runtimes take it from MATRIX_REQUIRE_MENTION).
if matrix.get("require_mention") is not None:
    projected_extra["require_mention"] = bool(matrix["require_mention"])

# NOT projected (no upstream key — see README.md):
#   access_token_ref — Hermes credentials are the typed token field
#     or the MATRIX_ACCESS_TOKEN env var; there is no secret-reference key.
#   history_limit / history_mode — no equivalent in the matrix adapter.

access_token = None
if with_credentials:
    access_token = os.environ.get("AGENTTEAMS_MATRIX_ACCESS_TOKEN", "")
    if not access_token:
        fail(
            "--with-credentials requires the AGENTTEAMS_MATRIX_ACCESS_TOKEN "
            "environment variable to be set"
        )


# --- Runtime capability gate -------------------------------------------------
# Resolution order (documented in the header comment): a successful
# tree probe (ground truth) -> --hermes-capabilities (explicit
# declaration) -> the verified version table -> unknown (fail closed).

def detect_version():
    if version_arg:
        return version_arg.strip()
    env_version = os.environ.get("HERMES_VERSION", "").strip()
    if env_version:
        return env_version
    try:
        proc = subprocess.run(
            ["hermes", "--version"],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return ""
    return (proc.stdout or "").strip()


def normalize_version(raw):
    """First dotted numeric run of *raw* (a leading "v" is irrelevant)."""
    if not raw:
        return None
    match = re.search(r"\d+(?:\.\d+)+", raw)
    return match.group(0) if match else None


def probe_tree_capabilities(root):
    """Derive the capability of a located Hermes root; never guess.

    "full"  — plugins/platforms/matrix/adapter.py exists and its source
              reads all of allowed_rooms / allowed_users /
              require_mention (the plugin-API generation, verified on
              tags v2026.9.24 and v0.22.5);
    "basic" — only the legacy gateway/platforms/matrix.py exists, or
              the new-layout adapter does not read the three settings;
    None    — neither known Matrix shape is detectable."""
    adapter = root / "plugins" / "platforms" / "matrix" / "adapter.py"
    if adapter.is_file():
        source = adapter.read_text(encoding="utf-8", errors="replace")
        if all(key in source for key in FULL_CAPABILITY_KEYS):
            return "full"
        return "basic"
    if (root / "gateway" / "platforms" / "matrix.py").is_file():
        return "basic"
    return None


def _has_known_matrix_shape(root):
    return (
        (root / "plugins" / "platforms" / "matrix" / "adapter.py").is_file()
        or (root / "gateway" / "platforms" / "matrix.py").is_file()
    )


def implicit_hermes_roots():
    """Candidate SDK roots discovered without an explicit override.

    HERMES_HOME is deliberately absent: the Worker entrypoint sets it to
    ${WORKSPACE}/.hermes, the runtime's STATE directory (config.yaml,
    sessions). It is never an SDK tree."""
    try:
        spec = importlib.util.find_spec("gateway")
    except Exception:
        spec = None
    if spec is not None and spec.submodule_search_locations:
        yield Path(next(iter(spec.submodule_search_locations))).resolve().parent
    hermes_bin = shutil.which("hermes")
    if hermes_bin:
        # <venv>/bin/hermes -> <venv>; the site-packages candidate below is
        # the one that usually carries the SDK tree.
        yield Path(hermes_bin).resolve().parent.parent
    for site_packages in sorted(Path("/opt/venv/hermes").glob("lib/python*/site-packages")):
        yield site_packages


def locate_hermes_root():
    """(root, explicit): an SDK root to probe, or (None, False).

    Only --hermes-root is explicit (a missing directory is a hard error).
    Implicit candidates (importable package, the hermes executable's venv, a
    conventional Worker venv) count only when a known Matrix shape is
    detectable; otherwise there is simply no evidence and the version table
    decides."""
    explicit = (hermes_root_arg or "").strip()
    if explicit:
        path = Path(explicit).expanduser()
        if not path.is_dir():
            fail(f"hermes root {explicit} (from --hermes-root) is not a directory")
        return path, True
    for candidate in implicit_hermes_roots():
        if _has_known_matrix_shape(candidate):
            return candidate, False
    return None, False


def resolve_capabilities(runtime_version):
    """(capability, source, root): capability is "full", "basic" or
    None (unknown); source is the human-readable evidence."""
    root, explicit = locate_hermes_root()
    if root is not None:
        capability = probe_tree_capabilities(root)
        if capability is not None:
            return capability, f"tree probe {root}", root
        if capabilities_arg:
            return capabilities_arg, "--hermes-capabilities", root
        if explicit:
            # An operator-supplied root without a detectable Matrix shape is
            # not a successful probe: never guess.
            return None, f"tree probe {root} (no known Matrix shape)", root
        # An implicit candidate without a shape proves nothing (the runtime
        # may simply not be importable from this interpreter): fall through
        # to the version evidence instead of calling the generation unknown.
    if capabilities_arg:
        return capabilities_arg, "--hermes-capabilities", None
    normalized = normalize_version(runtime_version)
    if normalized in VERIFIED_CAPABILITIES:
        return VERIFIED_CAPABILITIES[normalized], f"verified table for {normalized}", None
    if runtime_version:
        return None, f"version {runtime_version}", None
    return None, "no version declared or detected", None


marker_path = config_path.parent / MARKER_DIR_NAME / MARKER_NAME
required = [key for key in UNHONORABLE_SETTINGS if key in projected_extra]
runtime_version = detect_version()
capability, capability_source, probed_root = resolve_capabilities(runtime_version)
dropped = []

if capability in ("basic", None) and required:
    version_label = f'"{runtime_version}"' if runtime_version else '"undetermined"'
    if capability == "basic":
        subject = runtime_version or (f"tree at {probed_root}" if probed_root else "unknown")
        gate = f"hermes runtime {subject} is a basic-capability runtime"
        if capability_source.startswith("tree probe"):
            gate += f" (evidence: {capability_source})"
    else:
        gate = f"unknown Hermes generation {version_label}"
    if not allow_partial:
        if capability == "basic":
            fail(
                f"{gate}: its Matrix adapter cannot honor these contract "
                f"settings: {', '.join(required)}. The installer refuses "
                "to write a config that would silently ignore required "
                "settings. Target a full-capability runtime (verified: "
                "tag v2026.9.24 / package 0.21.5, tag v0.22.5) or pass "
                "--allow-partial to project the remaining settings and "
                f"record the dropped ones in {marker_path}."
            )
        fail(
            f"{gate}; the contract requires full-capability settings "
            f"({', '.join(required)}) that this runtime is not verified "
            "to honor. Pass --hermes-capabilities=full|basic (or "
            "--hermes-root) to state what it supports, or --allow-partial "
            f"to accept a reduced projection (recorded in {marker_path})."
        )
    dropped = list(required)
    for key in required:
        del projected_extra[key]
    if capability == "basic":
        print(
            f"WARNING: {gate}; these contract settings are not honored "
            f"by it and were not written: {', '.join(required)} (partial "
            f"projection recorded in {marker_path})",
            file=sys.stderr,
        )
    else:
        print(
            f"WARNING: {gate}; these contract settings are not verified "
            f"to be honored and were not written: {', '.join(required)} "
            f"(partial projection recorded in {marker_path})",
            file=sys.stderr,
        )

# --- Merge into the target config ------------------------------------------

config = None
if config_path.is_file():
    raw = config_path.read_text(encoding="utf-8")
    if raw.strip():
        try:
            config = yaml.safe_load(raw)
        except yaml.YAMLError as exc:
            fail(f"existing config {config_path} is not valid YAML: {exc}")
if config is None:
    config = {}
if not isinstance(config, dict):
    fail(f"existing config {config_path} must be a YAML mapping at the top level")

platforms = config.get("platforms")
if platforms is None:
    platforms = {}
if not isinstance(platforms, dict):
    fail(f"'platforms' in {config_path} is not a YAML mapping")
matrix_cfg = platforms.get("matrix")
if matrix_cfg is None:
    matrix_cfg = {}
if not isinstance(matrix_cfg, dict):
    fail(f"'platforms.matrix' in {config_path} is not a YAML mapping")

block = dict(matrix_cfg)
# enabled — typed PlatformConfig field (both generations).
block["enabled"] = True
# token — typed PlatformConfig field, written only with --with-credentials.
if access_token:
    block[TOKEN_KEY] = access_token
# Merge into the block's existing extra map; foreign extra keys survive.
existing_extra = block.get("extra")
if existing_extra is not None and not isinstance(existing_extra, dict):
    fail(f"'platforms.matrix.extra' in {config_path} is not a YAML mapping")
if existing_extra is None:
    existing_extra = {}
block["extra"] = {**existing_extra, **projected_extra}
platforms["matrix"] = block
config["platforms"] = platforms

if dry_run:
    sys.stdout.write(render(redact(config)))
    print(
        f"agentteams-matrix hermes adapter: dry run, no files written "
        f"(target: {config_path})",
        file=sys.stderr,
    )
    sys.exit(0)

write_atomic(config_path, render(config), config)

marker_dir = marker_path.parent
if dropped:
    marker_dir.mkdir(parents=True, exist_ok=True)
    marker = {
        "adapter": "hermes",
        "bundle": "agentteams-matrix",
        "capabilities": capability or "unknown",
        "capability_source": capability_source,
        "config": str(config_path),
        "dropped": sorted(dropped),
        "runtime_version": runtime_version or "unknown",
    }
    write_atomic(
        marker_path,
        json.dumps(marker, indent=2, sort_keys=True) + "\n",
        marker,
    )
    print(
        f"agentteams-matrix hermes adapter: partial projection into "
        f"platforms.matrix in {config_path} (runtime "
        f"{runtime_version or 'unknown'}, capabilities {capability or 'unknown'} via "
        f"{capability_source}; dropped {', '.join(sorted(dropped))})"
    )
else:
    # A full projection supersedes any stale partial-projection marker.
    if marker_path.is_file():
        marker_path.unlink()
        try:
            marker_dir.rmdir()
        except OSError:
            pass
    print(
        f"agentteams-matrix hermes adapter: wrote platforms.matrix to "
        f"{config_path} (runtime {runtime_version or 'unknown'}, "
        f"capabilities {capability or 'unknown'} via {capability_source}; "
        "full projection)"
    )
PY
