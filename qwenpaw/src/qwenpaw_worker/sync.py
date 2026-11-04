"""Object-storage restore and runtime-state persistence for qwenpaw-worker."""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import os
from pathlib import Path
import shutil
import subprocess
import time
from typing import Iterable, List, Optional

logger = logging.getLogger(__name__)

DEFAULT_MC_ALIAS = "agentteams"
COMPARE_CONTENT_MAX_BYTES = 20 * 1024 * 1024

# Keep at least this much free space after restoring protected files; below
# this boundary the pull fails closed before the destructive mirror starts.
PROTECTED_RESTORE_HEADROOM_BYTES = 1024 * 1024

#: Persisted push manifest (#1391): the authority for "what did we push
#: last", so a restart can tell unpushed local writes from pulled state
#: without trusting mtimes.
SYNC_STATE_VERSION = 1


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _to_text(value: object) -> str:
    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace")
    return str(value or "")


def _looks_like_missing_object_error(stderr: Optional[str] | bytes) -> bool:
    text = _to_text(stderr)
    return "Object does not exist" in text or "The specified key does not exist" in text


def _mc_error_message(exc: subprocess.CalledProcessError) -> str:
    text = _to_text(exc.stderr or exc.stdout).strip()
    return text or f"mc exited with status {exc.returncode}"


def _redact_url_userinfo(value: str) -> str:
    if "://" not in value or "@" not in value:
        return value
    scheme, rest = value.split("://", 1)
    return f"{scheme}://<redacted>@{rest.split('@', 1)[1]}"


def _preview_list(values: List[str], limit: int = 20) -> List[str]:
    if len(values) <= limit:
        return values
    return [*values[:limit], f"...({len(values) - limit} more)"]


def _storage_alias() -> str:
    value = os.getenv("AGENTTEAMS_STORAGE_ALIAS", "").strip().strip("/")
    if value:
        return value
    prefix = os.getenv("AGENTTEAMS_STORAGE_PREFIX", "").strip().strip("/")
    if "/" in prefix:
        return prefix.split("/", 1)[0]
    return DEFAULT_MC_ALIAS


class FileSync:
    def __init__(
        self,
        *,
        endpoint: str,
        access_key: str,
        secret_key: str,
        bucket: str,
        worker_name: str,
        local_dir: Path,
        shared_dir: Path,
        remote_prefix: Optional[str] = None,
        shared_prefix: Optional[str] = None,
    ) -> None:
        self.endpoint = endpoint
        self.access_key = access_key
        self.secret_key = secret_key
        self.bucket = bucket
        self.worker_name = worker_name
        self.local_dir = local_dir
        self.shared_dir = shared_dir
        self.remote_prefix = (remote_prefix or f"agents/{worker_name}").strip("/")
        self.shared_prefix = (shared_prefix or "shared").strip("/")
        self.mc_alias = _storage_alias()
        self._alias_set = False
        # The manifest lives OUTSIDE the synced roots: a file inside
        # local_dir would be mirrored to the remote as worker state.
        self.sync_state_path = Path(local_dir).parent / (
            f".{worker_name}-sync-state.json"
        )

    def _mc(self, *args: str, check: bool = True, text: bool = True) -> subprocess.CompletedProcess:
        mc_bin = shutil.which("mc")
        if not mc_bin:
            raise RuntimeError("mc binary not found")
        return subprocess.run(
            [mc_bin, *args],
            check=check,
            capture_output=True,
            text=text,
        )

    def ensure_alias(self) -> None:
        if self._alias_set:
            return
        if os.getenv(f"MC_HOST_{self.mc_alias}"):
            self._alias_set = True
            logger.info("storage alias ready component=sync worker=%s mode=env", self.worker_name)
            return
        if (
            os.getenv("AGENTTEAMS_RUNTIME") == "k8s"
            and os.getenv("AGENTTEAMS_STORAGE_PROVIDER", "").strip().lower() == "oss"
        ):
            raise RuntimeError(
                f"OSS storage requires controller-issued MC_HOST_{self.mc_alias} credentials"
            )
        missing = [
            name
            for name, value in (
                ("endpoint", self.endpoint),
                ("access_key", self.access_key),
                ("secret_key", self.secret_key),
            )
            if not value
        ]
        if missing:
            raise RuntimeError(f"missing storage config: {', '.join(missing)}")

        endpoint = self.endpoint.rstrip("/")
        if not endpoint.startswith(("http://", "https://")):
            endpoint = f"http://{endpoint}"
        logger.info(
            "configuring storage alias component=sync worker=%s endpoint=%s bucket=%s",
            self.worker_name,
            _redact_url_userinfo(endpoint),
            self.bucket,
        )
        try:
            self._mc("alias", "set", self.mc_alias, endpoint, self.access_key, self.secret_key)
        except subprocess.CalledProcessError as exc:
            raise RuntimeError(f"configure storage alias failed: {_mc_error_message(exc)}") from None
        self._alias_set = True
        logger.info("storage alias ready component=sync worker=%s mode=static", self.worker_name)

    def _object_path(self, key: str) -> str:
        return f"{self.mc_alias}/{self.bucket}/{key.strip('/')}"

    def _cat(self, key: str) -> Optional[str]:
        self.ensure_alias()
        result = self._mc("cat", self._object_path(key), check=False)
        if result.returncode == 0:
            return result.stdout
        if _looks_like_missing_object_error(result.stderr):
            return None
        logger.debug("mc cat failed component=sync key=%s returncode=%s", key, result.returncode)
        return None

    def _cat_bytes(self, key: str) -> Optional[bytes]:
        self.ensure_alias()
        try:
            result = self._mc("cat", self._object_path(key), check=False, text=False)
        except Exception as exc:
            logger.debug("mc cat failed component=sync key=%s error_type=%s", key, type(exc).__name__)
            return None
        if result.returncode == 0:
            stdout = result.stdout
            if isinstance(stdout, bytes):
                return stdout
            return _to_text(stdout).encode("utf-8")
        if _looks_like_missing_object_error(result.stderr):
            return None
        logger.debug("mc cat failed component=sync key=%s returncode=%s", key, result.returncode)
        return None

    # ---- push manifest / durability (#1391) ---------------------------

    def _root_key(self, local_dir: Path) -> str:
        base = Path(local_dir).resolve()
        if base == self.local_dir.resolve():
            return "worker"
        if base == self.shared_dir.resolve():
            return "shared"
        return f"custom:{base}"

    def _load_sync_state(self) -> dict:
        try:
            data = json.loads(self.sync_state_path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return {"version": SYNC_STATE_VERSION, "roots": {}}
        if not isinstance(data, dict):
            return {"version": SYNC_STATE_VERSION, "roots": {}}
        if not isinstance(data.get("roots"), dict):
            data["roots"] = {}
        return data

    def _save_sync_state(self, state: dict) -> None:
        try:
            self.sync_state_path.parent.mkdir(parents=True, exist_ok=True)
            tmp = self.sync_state_path.with_suffix(
                self.sync_state_path.suffix + ".tmp"
            )
            tmp.write_text(
                json.dumps(state, ensure_ascii=False, indent=2) + "\n",
                encoding="utf-8",
            )
            os.replace(tmp, self.sync_state_path)
        except OSError as exc:
            logger.warning(
                "sync manifest not written component=sync worker=%s error=%s",
                self.worker_name,
                exc,
            )

    def _record_pushed(self, root: str, rel: str, sha256: str) -> None:
        # #1391: the baseline must be the content that was actually pushed
        # (uploaded bytes, or the remote copy verified as equal), never a
        # re-read of the local file at record time — a write landing in
        # that window would be recorded as pushed and silently overwritten
        # by the next pull.
        state = self._load_sync_state()
        files = state.setdefault("roots", {}).setdefault(root, {})
        files[rel] = {
            "sha256": sha256,
            "pushed_at": time.time(),
        }
        state["updated_at"] = time.time()
        self._save_sync_state(state)

    def _protected_files(self, root: str, base: Path) -> dict:
        """Local files that changed since their last push.

        A pull must not overwrite them with the older remote copy. Only
        files the manifest already knows are considered, so a first run
        (empty manifest) still pulls normally.
        """
        state = self._load_sync_state()
        files = (state.get("roots") or {}).get(root) or {}
        if not files:
            return {}
        protected: dict = {}
        for rel, entry in files.items():
            if not isinstance(entry, dict):
                continue
            path = base / rel
            if not path.is_file() or path.is_symlink():
                continue
            try:
                if _sha256_file(path) != entry.get("sha256"):
                    protected[rel] = path.read_bytes()
            except OSError:
                continue
        return protected

    def _restore_protected(self, base: Path, protected: dict) -> None:
        for rel, content in protected.items():
            path = base / rel
            try:
                # With the --exclude above this is the normal case: mc never
                # touched the dirty file, so writing it again would at best
                # waste space and at worst truncate it on a full disk.
                if path.is_file() and path.read_bytes() == content:
                    continue
            except OSError:
                pass
            tmp_path = path.with_name(
                f".{path.name}.sync-restore-{os.getpid()}"
            )
            try:
                path.parent.mkdir(parents=True, exist_ok=True)
                tmp_path.write_bytes(content)
                os.replace(tmp_path, path)
            except OSError as exc:
                try:
                    tmp_path.unlink()
                except OSError:
                    pass
                logger.warning(
                    "unpushed local write not restored component=sync "
                    "worker=%s path=%s error=%s",
                    self.worker_name,
                    rel,
                    exc,
                )
        if protected:
            logger.warning(
                "pull skipped %s unpushed local write(s) component=sync "
                "worker=%s paths=%s",
                len(protected),
                self.worker_name,
                ",".join(sorted(protected)[:10]),
            )

    def _directory_conflicts(self, directory: Path) -> List[str]:
        """Files whose local AND remote copies changed since the last push."""
        state = self._load_sync_state()
        files = (state.get("roots") or {}).get("worker") or {}
        local_root = self.local_dir.resolve()
        conflicts: List[str] = []
        for path in sorted(directory.rglob("*")):
            if not path.is_file() or path.is_symlink():
                continue
            try:
                rel = path.resolve().relative_to(local_root).as_posix()
            except ValueError:
                continue
            entry = files.get(rel)
            if not isinstance(entry, dict) or not entry.get("sha256"):
                continue  # never pushed: no base to conflict against
            if path.stat().st_size > COMPARE_CONTENT_MAX_BYTES:
                continue
            local_sha = _sha256_file(path)
            if local_sha == entry.get("sha256"):
                continue  # unchanged locally
            remote = self._cat_bytes(f"{self.remote_prefix}/{rel}")
            if remote is None:
                continue
            remote_sha = hashlib.sha256(remote).hexdigest()
            if (
                remote_sha != entry.get("sha256")
                and remote_sha != local_sha
            ):
                conflicts.append(path.relative_to(directory).as_posix())
        return conflicts

    def sync_status(self) -> dict:
        """Push-state snapshot: what is tracked and what is not pushed yet."""
        state = self._load_sync_state()
        roots = state.get("roots") or {}
        unpushed: List[dict] = []
        for root, base in (("worker", self.local_dir), ("shared", self.shared_dir)):
            for rel, entry in (roots.get(root) or {}).items():
                if not isinstance(entry, dict):
                    continue
                path = base / rel
                if not path.is_file():
                    unpushed.append(
                        {"root": root, "path": rel, "status": "deleted"}
                    )
                else:
                    try:
                        changed = _sha256_file(path) != entry.get("sha256")
                    except OSError:
                        changed = True
                    if changed:
                        unpushed.append(
                            {"root": root, "path": rel, "status": "modified"}
                        )
        return {
            "version": SYNC_STATE_VERSION,
            "tracked": {root: len(files) for root, files in roots.items()},
            "unpushed": unpushed,
            "last_error": state.get("last_error"),
            "updated_at": state.get("updated_at"),
        }

    def _mirror_prefix(self, remote_prefix: str, local_dir: Path) -> None:
        local_dir.mkdir(parents=True, exist_ok=True)
        remote = f"{self.mc_alias}/{self.bucket}/{remote_prefix.strip('/')}/"
        # #1391: an overwrite mirror must not replace unpushed local work
        # with the older remote copy. Files whose content differs from the
        # last pushed manifest are captured here and restored afterwards.
        root_key = self._root_key(local_dir)
        protected = self._protected_files(root_key, local_dir)
        if protected:
            # The old implementation captured dirty files in memory, started
            # the destructive mirror, and only restored in ``finally``.  If
            # the mirror filled the disk, the restore write hit ENOSPC and
            # truncated the only copy.  Refuse to start in that case.
            protected_bytes = sum(len(content) for content in protected.values())
            try:
                free_bytes = shutil.disk_usage(local_dir).free
            except OSError:
                free_bytes = None
            if (
                free_bytes is not None
                and free_bytes < protected_bytes + PROTECTED_RESTORE_HEADROOM_BYTES
            ):
                raise RuntimeError(
                    "refusing destructive mirror: insufficient free space for "
                    f"{len(protected)} unpushed file(s) "
                    f"({protected_bytes} bytes required, {free_bytes} free)"
                )
        logger.info(
            "mirroring storage prefix component=sync worker=%s remote=%s local=%s"
            " protected=%s",
            self.worker_name,
            remote,
            local_dir,
            len(protected),
        )
        # Exclude dirty paths from mc itself.  Restoring from memory is only a
        # fallback for an mc implementation that ignores excludes; it must not
        # be the only thing standing between a disk-full pull and data loss.
        exclude_args: List[str] = ["--exclude", "credentials/**"]
        for rel in sorted(protected):
            exclude_args.extend(("--exclude", rel))
        try:
            self._mc(
                "mirror",
                remote,
                str(local_dir) + "/",
                "--overwrite",
                *exclude_args,
            )
        except subprocess.CalledProcessError as exc:
            if _looks_like_missing_object_error(exc.stderr):
                logger.info("storage prefix is empty component=sync remote=%s", remote)
                return
            raise RuntimeError(f"mirror storage failed: {_mc_error_message(exc)}") from None
        finally:
            # #1391: restore on every outcome — a partial mirror failure or
            # the empty-prefix branch must not leave the older remote copy
            # over an unpushed local write.
            self._restore_protected(local_dir, protected)
        logger.info(
            "mirrored storage prefix component=sync worker=%s remote=%s local=%s",
            self.worker_name,
            remote,
            local_dir,
        )

    def mirror_all(self) -> None:
        self.ensure_alias()
        self._mirror_prefix(self.remote_prefix, self.local_dir)
        self._mirror_prefix(self.shared_prefix, self.shared_dir)

    def mirror_prefix(self, remote_prefix: str, local_dir: Path) -> None:
        self.ensure_alias()
        self._mirror_prefix(remote_prefix, local_dir)

    def pull_runtime_config(self, local_path: Path, remote_key: Optional[str] = None) -> bool:
        self.ensure_alias()
        key = remote_key or f"{self.remote_prefix}/runtime/runtime.yaml"
        local_path.parent.mkdir(parents=True, exist_ok=True)
        result = self._mc("cp", self._object_path(key), str(local_path), check=False)
        if result.returncode == 0:
            return True
        if _looks_like_missing_object_error(result.stderr):
            logger.info("runtime config not found in storage component=sync key=%s", key)
            return False
        raise RuntimeError(f"pull runtime config failed: {_mc_error_message(result)}")

    def push_paths(self, paths: Iterable[Path]) -> List[str]:
        """Persist selected worker files, raising on the first upload failure."""

        self.ensure_alias()
        pushed = []
        local_root = self.local_dir.resolve()
        for path in paths:
            if path.is_symlink() or not path.is_file():
                continue
            try:
                rel = path.resolve().relative_to(local_root)
            except ValueError:
                raise ValueError(f"path is outside worker storage root: {path}") from None
            key = f"{self.remote_prefix}/{rel.as_posix()}"
            data: Optional[bytes] = None
            if path.stat().st_size <= COMPARE_CONTENT_MAX_BYTES:
                # #1391: snapshot the bytes before any compare or upload;
                # the baseline is derived from this content, never from a
                # re-read at record time.
                data = path.read_bytes()
                local_sha = hashlib.sha256(data).hexdigest()
                remote = self._cat_bytes(key)
                if remote == data:
                    # #1391: the remote already holds this exact content, so
                    # the upload is a no-op — but the manifest still needs the
                    # baseline, otherwise the next local edit looks untracked
                    # and a pull may overwrite it.
                    self._record_pushed("worker", rel.as_posix(), local_sha)
                    continue
            else:
                # Large file: no content compare or post-upload read-back —
                # downloading a big object just to hash it is what the size
                # threshold exists to avoid. The pre-upload snapshot is the
                # best available baseline; a write landing during the upload
                # can still mix into the uploaded object, a known window this
                # change does not close.
                local_sha = _sha256_file(path)
            try:
                self._mc("cp", str(path), self._object_path(key), check=True)
            except subprocess.CalledProcessError as exc:
                raise RuntimeError(f"persist migrated state failed: {_mc_error_message(exc)}") from None
            if data is not None:
                # #1391: bind the baseline to what the remote actually holds
                # after the upload. A local write that landed while `mc cp`
                # ran is not in the remote and must stay dirty; a failed or
                # empty read-back falls back to the pre-upload snapshot.
                remote_after = self._cat_bytes(key)
                if remote_after is not None and remote_after != data:
                    local_sha = hashlib.sha256(remote_after).hexdigest()
            pushed.append(rel.as_posix())
            self._record_pushed("worker", rel.as_posix(), local_sha)
        return pushed

    def push_directories(self, directories: Iterable[Path]) -> List[str]:
        """Persist selected worker directories with one mirror per directory."""

        self.ensure_alias()
        mirrored = []
        local_root = self.local_dir.resolve()
        for directory in directories:
            if not directory.is_dir() or directory.is_symlink():
                continue
            try:
                rel = directory.resolve().relative_to(local_root)
            except ValueError:
                raise ValueError(f"directory is outside worker storage root: {directory}") from None
            remote = self._object_path(f"{self.remote_prefix}/{rel.as_posix()}")
            # #1391: a directory push must not overwrite a newer remote
            # object. Files changed on both sides since the last push are
            # excluded from the mirror and reported as conflicts; the rest
            # of the directory still pushes.
            conflicts = self._directory_conflicts(directory)
            # #1391: capture each file's hash before the mirror runs so the
            # baseline is the content that gets uploaded, not a re-read
            # after the mirror (a write landing in the mirror window would
            # otherwise be recorded as pushed).
            snapshots: List[tuple[str, str]] = []
            for path in sorted(directory.rglob("*")):
                if not path.is_file() or path.is_symlink():
                    continue
                rel_to_dir = path.relative_to(directory).as_posix()
                snapshots.append((rel_to_dir, _sha256_file(path)))
            extra_args: List[str] = []
            for conflict in conflicts:
                extra_args.extend(["--exclude", conflict])
            try:
                self._mc(
                    "mirror",
                    f"{directory}/",
                    f"{remote}/",
                    "--overwrite",
                    *extra_args,
                )
            except subprocess.CalledProcessError as exc:
                state = self._load_sync_state()
                state["last_error"] = _mc_error_message(exc)
                self._save_sync_state(state)
                raise RuntimeError(f"persist migrated state failed: {_mc_error_message(exc)}") from None
            if conflicts:
                logger.warning(
                    "push conflicts excluded component=sync worker=%s dir=%s "
                    "paths=%s",
                    self.worker_name,
                    rel.as_posix(),
                    ",".join(conflicts[:10]),
                )
            conflict_set = set(conflicts)
            for rel_to_dir, sha256 in snapshots:
                if rel_to_dir in conflict_set:
                    continue
                self._record_pushed(
                    "worker",
                    f"{rel.as_posix()}/{rel_to_dir}",
                    sha256,
                )
            mirrored.append(rel.as_posix())
        return mirrored


def _skip_background_push(rel: Path) -> bool:
    rel_path = rel.as_posix()
    excluded_prefixes = (
        "credentials",
        "runtime",
        "shared",
        "global-shared",
        ".qwenpaw/workspaces/default/shared",
        ".qwenpaw/workspaces/default/global-shared",
        ".qwenpaw/workspaces/default/tool_result",
        ".qwenpaw/workspaces/default/file_store",
        ".qwenpaw/workspaces/default/media",
        ".qwenpaw/workspaces/default/embedding_cache",
    )
    if any(rel_path == prefix or rel_path.startswith(f"{prefix}/") for prefix in excluded_prefixes):
        return True
    excluded_dirs = {".cache", ".local", ".mc", "__pycache__", "logs"}
    if any(part in excluded_dirs for part in rel.parts):
        return True
    excluded_files = {".DS_Store", "qwenpaw.log", "heartbeat.json", "token_usage.json"}
    if rel.name in excluded_files:
        return True
    return rel.suffix in {".lock", ".pyc"}


def push_local(sync: FileSync, since: float = 0) -> List[str]:
    pushed = []
    local_dir = sync.local_dir
    if not local_dir.exists():
        return pushed

    sync.ensure_alias()
    for path in local_dir.rglob("*"):
        if not path.is_file():
            continue
        try:
            stat = path.stat()
            if stat.st_mtime <= since:
                continue
        except OSError:
            continue

        rel = path.relative_to(local_dir)
        if _skip_background_push(rel):
            continue

        key = f"{sync.remote_prefix}/{rel.as_posix()}"
        try:
            data: Optional[bytes] = None
            if stat.st_size <= COMPARE_CONTENT_MAX_BYTES:
                # #1391: snapshot the bytes before any compare or upload;
                # the baseline is derived from this content, never from a
                # re-read at record time.
                data = path.read_bytes()
                local_sha = hashlib.sha256(data).hexdigest()
                remote = sync._cat_bytes(key)
                if remote == data:
                    # #1391: no upload needed, but the background push is the
                    # only writer that knows this content is now the remote
                    # baseline; record it so a later local edit survives the
                    # next mirror.
                    sync._record_pushed("worker", rel.as_posix(), local_sha)
                    continue
            else:
                # Large file: no content compare or post-upload read-back;
                # the pre-upload snapshot is the best available baseline
                # (a write landing during the upload is a known window
                # this change does not close).
                local_sha = _sha256_file(path)
            sync._mc("cp", str(path), sync._object_path(key), check=True)
            if data is not None:
                # #1391: bind the baseline to what the remote actually holds
                # after the upload; a failed or empty read-back falls back
                # to the pre-upload snapshot.
                remote_after = sync._cat_bytes(key)
                if remote_after is not None and remote_after != data:
                    local_sha = hashlib.sha256(remote_after).hexdigest()
            pushed.append(rel.as_posix())
            # #1391: background uploads are pushes too — without the manifest
            # entry the next startup mirror cannot tell a later unpushed edit
            # from stale pulled state.
            sync._record_pushed("worker", rel.as_posix(), local_sha)
        except Exception as exc:
            logger.debug("push_local failed component=sync file=%s error_type=%s", rel, type(exc).__name__)

    return pushed


async def push_loop(sync: FileSync, check_interval: float = 5) -> None:
    # Startup state has just been mirrored from object storage. Only watch
    # changes made after the loop starts; a since=0 scan can spend minutes
    # comparing a QwenPaw 2 workdir and starve newly-created files.
    last_push_time = time.time()
    logger.info(
        "qwenpaw FileSync push loop started component=sync worker=%s interval_seconds=%s",
        sync.worker_name,
        check_interval,
    )
    while True:
        try:
            await asyncio.sleep(check_interval)
            now = time.time()
            pushed = await asyncio.to_thread(push_local, sync, last_push_time)
            last_push_time = now
            if pushed:
                logger.info(
                    "qwenpaw FileSync push uploaded component=sync worker=%s count=%d files=%s",
                    sync.worker_name,
                    len(pushed),
                    _preview_list(pushed),
                )
        except asyncio.CancelledError:
            logger.info("qwenpaw FileSync push loop stopped component=sync worker=%s", sync.worker_name)
            break
        except Exception as exc:
            logger.warning(
                "qwenpaw FileSync push failed component=sync worker=%s error_type=%s",
                sync.worker_name,
                type(exc).__name__,
            )
