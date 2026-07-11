import asyncio
import json
from pathlib import Path
import sys
import types

import pytest

from copaw_worker.config import WorkerConfig
from copaw_worker.health import ComponentHealth
from copaw_worker.worker import Worker


class _FakeWorkerAPIServer:
    instances = []

    def __init__(self, *, host, port, liveness_handler, readiness_handler):
        self.host = host
        self.port = port
        self.liveness_handler = liveness_handler
        self.readiness_handler = readiness_handler
        self.started = False
        self.stopped = False
        self.__class__.instances.append(self)

    async def start(self):
        self.started = True

    async def stop(self):
        self.stopped = True


@pytest.fixture(autouse=True)
def fake_worker_api(monkeypatch):
    _FakeWorkerAPIServer.instances = []
    monkeypatch.setattr("copaw_worker.worker.WorkerAPIServer", _FakeWorkerAPIServer)
    return _FakeWorkerAPIServer


@pytest.fixture
def anyio_backend():
    return "asyncio"


def _config(tmp_path):
    return WorkerConfig(
        worker_name="alice",
        minio_endpoint="http://minio:9000",
        minio_access_key="minio",
        minio_secret_key="password",
        install_dir=tmp_path,
    )


def _health_json(tmp_path):
    return json.loads((tmp_path / "alice" / ".copaw" / "health.json").read_text())


def test_worker_port_defaults_to_console_port_plus_one(tmp_path):
    config = WorkerConfig(
        worker_name="alice",
        minio_endpoint="http://minio:9000",
        minio_access_key="minio",
        minio_secret_key="password",
        install_dir=tmp_path,
        console_port=18088,
    )

    assert config.worker_port == 18089


def test_worker_port_can_be_explicit(tmp_path):
    config = WorkerConfig(
        worker_name="alice",
        minio_endpoint="http://minio:9000",
        minio_access_key="minio",
        minio_secret_key="password",
        install_dir=tmp_path,
        console_port=18088,
        worker_port=19090,
    )

    assert config.worker_port == 19090


async def _finished_push_loop(*_args, **_kwargs):
    return None


async def _finished_pull_loop(*_args, **_kwargs):
    return None


@pytest.mark.anyio
async def test_worker_marks_sync_healthy_after_startup_mirror(tmp_path, monkeypatch, fake_worker_api):
    push_loop_args = {}
    pull_loop_args = {}

    async def wait_forever():
        await asyncio.Event().wait()

    def capture_push_loop(*_args, **kwargs):
        push_loop_args.update(kwargs)
        return wait_forever()

    def capture_pull_loop(*_args, **kwargs):
        pull_loop_args.update(kwargs)
        return wait_forever()

    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.get_config", lambda _self: {})
    monkeypatch.setattr("copaw_worker.sync.FileSync.list_skills", lambda _self: [])
    monkeypatch.setattr(Worker, "_matrix_relogin", lambda _self, cfg: cfg)
    monkeypatch.setattr(
        "copaw_worker.worker.check_model_service",
        lambda _cfg: ComponentHealth("healthy", "model provider reachable"),
    )
    monkeypatch.setattr("copaw_worker.worker.bridge_standard_to_runtime", lambda *_args, **_kwargs: None)
    monkeypatch.setattr("copaw_worker.worker.push_loop", capture_push_loop)
    monkeypatch.setattr("copaw_worker.worker.sync_loop", capture_pull_loop)

    worker = Worker(_config(tmp_path))

    assert await worker.start() is True
    await worker.stop()

    data = _health_json(tmp_path)
    assert data["components"]["sync"]["healthiness"] == "healthy"
    assert data["components"]["sync"]["message"] == "startup mirror restored"
    assert data["components"]["bridge"]["healthiness"] == "healthy"
    assert data["components"]["bridge"]["message"] == "standard-to-copaw bridge completed"
    assert data["components"]["model"]["healthiness"] == "healthy"
    assert data["components"]["model"]["message"] == "model provider reachable"
    assert "health" in push_loop_args
    assert pull_loop_args["interval"] == 60
    assert pull_loop_args["on_pull"] == worker._on_files_pulled
    assert "health" in pull_loop_args
    assert len(fake_worker_api.instances) == 1
    worker_api = fake_worker_api.instances[0]
    assert worker_api.host == "0.0.0.0"
    assert worker_api.port == 8089
    assert worker_api.liveness_handler == worker.build_worker_liveness
    assert worker_api.readiness_handler == worker.build_worker_readiness
    assert worker_api.started is True
    assert worker_api.stopped is True


@pytest.mark.anyio
async def test_worker_marks_sync_unhealthy_when_startup_mirror_fails(tmp_path, monkeypatch):
    def fail_mirror(_self):
        raise RuntimeError("minio unavailable")

    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", fail_mirror)

    worker = Worker(_config(tmp_path))

    assert await worker.start() is False

    data = _health_json(tmp_path)
    sync = data["components"]["sync"]
    assert sync["healthiness"] == "unhealthy"
    assert sync["message"] == "startup mirror failed: minio unavailable"
    assert sync["details"]["operation"] == "mirror_all"
    assert sync["details"]["error_type"] == "RuntimeError"


@pytest.mark.anyio
async def test_worker_marks_bridge_unhealthy_when_standard_to_runtime_fails(tmp_path, monkeypatch):
    def fail_bridge(*_args, **_kwargs):
        raise ValueError("bad openclaw config")

    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.get_config", lambda _self: {})
    monkeypatch.setattr("copaw_worker.sync.FileSync.list_skills", lambda _self: [])
    monkeypatch.setattr(Worker, "_matrix_relogin", lambda _self, cfg: cfg)
    monkeypatch.setattr(
        "copaw_worker.worker.check_model_service",
        lambda _cfg: ComponentHealth("healthy", "model provider reachable"),
    )
    monkeypatch.setattr("copaw_worker.worker.bridge_standard_to_runtime", fail_bridge)

    worker = Worker(_config(tmp_path))

    assert await worker.start() is False

    data = _health_json(tmp_path)
    bridge = data["components"]["bridge"]
    assert bridge["healthiness"] == "unhealthy"
    assert bridge["message"] == "standard-to-copaw bridge failed: bad openclaw config"
    assert bridge["details"]["operation"] == "bridge_standard_to_runtime"
    assert bridge["details"]["error_type"] == "ValueError"


@pytest.mark.anyio
async def test_worker_records_unhealthy_model_preflight_without_blocking_start(tmp_path, monkeypatch):
    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.get_config", lambda _self: {})
    monkeypatch.setattr("copaw_worker.sync.FileSync.list_skills", lambda _self: [])
    monkeypatch.setattr(Worker, "_matrix_relogin", lambda _self, cfg: cfg)
    monkeypatch.setattr(
        "copaw_worker.worker.check_model_service",
        lambda _cfg: ComponentHealth(
            "unhealthy",
            "model provider is unreachable",
            {"operation": "model_preflight", "error_type": "TimeoutError"},
        ),
    )
    monkeypatch.setattr("copaw_worker.worker.bridge_standard_to_runtime", lambda *_args, **_kwargs: None)
    monkeypatch.setattr("copaw_worker.worker.push_loop", _finished_push_loop)
    monkeypatch.setattr("copaw_worker.worker.sync_loop", _finished_pull_loop)

    worker = Worker(_config(tmp_path))

    assert await worker.start() is True
    await worker.stop()

    data = _health_json(tmp_path)
    model = data["components"]["model"]
    assert model["healthiness"] == "unhealthy"
    assert model["message"] == "model provider is unreachable"
    assert model["details"]["operation"] == "model_preflight"
    assert model["details"]["error_type"] == "TimeoutError"


@pytest.mark.anyio
async def test_worker_notifies_matrix_on_model_preflight_failure(tmp_path, monkeypatch):
    notified = []
    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.get_config", lambda _self: {})
    monkeypatch.setattr("copaw_worker.sync.FileSync.list_skills", lambda _self: [])
    monkeypatch.setattr(Worker, "_matrix_relogin", lambda _self, cfg: cfg)
    monkeypatch.setattr(
        "copaw_worker.worker.check_model_service",
        lambda _cfg: ComponentHealth(
            "unhealthy",
            "model chat completion preflight returned HTTP 400",
            {"operation": "model_preflight", "provider": "qwen", "model": "qwen-max"},
        ),
    )
    monkeypatch.setattr(Worker, "_notify_matrix", lambda _self, msg, cfg: notified.append(msg))
    monkeypatch.setattr("copaw_worker.worker.bridge_standard_to_runtime", lambda *_args, **_kwargs: None)
    monkeypatch.setattr("copaw_worker.worker.push_loop", _finished_push_loop)
    monkeypatch.setattr("copaw_worker.worker.sync_loop", _finished_pull_loop)

    worker = Worker(_config(tmp_path))
    assert await worker.start() is True
    await worker.stop()

    assert len(notified) == 1
    assert "Model service check failed" in notified[0]
    assert "qwen" in notified[0]
    assert "qwen-max" in notified[0]


@pytest.mark.anyio
async def test_notify_matrix_sends_to_joined_rooms(tmp_path, monkeypatch):
    import json as json_mod
    import urllib.request

    sent_requests = []

    class FakeResponse:
        def __init__(self, data):
            self._data = data

        def __enter__(self):
            return self

        def __exit__(self, *_):
            return False

        def read(self):
            return json_mod.dumps(self._data).encode()

    def fake_urlopen(req, timeout=None):
        url = req.full_url if hasattr(req, "full_url") else req
        sent_requests.append((req.get_method(), url))
        if "/sync" in url:
            return FakeResponse({"rooms": {"invite": {}}})
        if "joined_rooms" in url:
            return FakeResponse({"joined_rooms": ["!room1:test", "!room2:test"]})
        return FakeResponse({})

    monkeypatch.setattr(urllib.request, "urlopen", fake_urlopen)

    worker = Worker(_config(tmp_path))
    openclaw_cfg = {
        "channels": {"matrix": {"homeserver": "http://matrix:6167", "accessToken": "tok"}}
    }
    worker._notify_matrix("test message", openclaw_cfg)

    methods = [(m, u) for m, u in sent_requests]
    assert methods[0][0] == "GET"
    assert "/sync?" in methods[0][1]
    assert methods[1][0] == "GET"
    assert "joined_rooms" in methods[1][1]
    assert methods[2][0] == "PUT"
    assert methods[3][0] == "PUT"


@pytest.mark.anyio
async def test_notify_matrix_accepts_invites_before_sending(tmp_path, monkeypatch):
    import json as json_mod
    import urllib.request

    sent_requests = []
    joined_call_count = [0]

    class FakeResponse:
        def __init__(self, data):
            self._data = data

        def __enter__(self):
            return self

        def __exit__(self, *_):
            return False

        def read(self):
            return json_mod.dumps(self._data).encode()

    def fake_urlopen(req, timeout=None):
        url = req.full_url if hasattr(req, "full_url") else req
        method = req.get_method()
        sent_requests.append((method, url))
        if "/sync" in url:
            return FakeResponse({
                "rooms": {
                    "invite": {
                        "!dm:test": {},
                        "!team:test": {},
                    }
                }
            })
        if "joined_rooms" in url:
            joined_call_count[0] += 1
            return FakeResponse({"joined_rooms": ["!dm:test", "!team:test"]})
        if "/join/" in url:
            return FakeResponse({"room_id": "joined"})
        return FakeResponse({})

    monkeypatch.setattr(urllib.request, "urlopen", fake_urlopen)

    worker = Worker(_config(tmp_path))
    openclaw_cfg = {
        "channels": {"matrix": {"homeserver": "http://matrix:6167", "accessToken": "tok"}}
    }
    worker._notify_matrix("test message", openclaw_cfg)

    methods = [m for m, _ in sent_requests]
    urls = [u for _, u in sent_requests]
    # sync → join × 2 → joined_rooms → send × 2
    assert methods[0] == "GET"  # sync
    assert "/sync" in urls[0]
    assert methods[1] == "POST"  # join !dm:test
    assert methods[2] == "POST"  # join !team:test
    assert methods[3] == "GET"   # joined_rooms
    assert methods[4] == "PUT"   # send to !dm:test
    assert methods[5] == "PUT"   # send to !team:test


def test_wait_for_matrix_rooms_retries_until_invite_arrives(tmp_path, monkeypatch):
    import json as json_mod
    import urllib.request

    poll_count = [0]

    class FakeResponse:
        def __init__(self, data):
            self._data = data

        def __enter__(self):
            return self

        def __exit__(self, *_):
            return False

        def read(self):
            return json_mod.dumps(self._data).encode()

    def fake_urlopen(req, timeout=None):
        url = req.full_url if hasattr(req, "full_url") else req
        if "/sync" in url:
            if poll_count[0] < 2:
                return FakeResponse({"rooms": {"invite": {}}})
            return FakeResponse({
                "rooms": {"invite": {"!late:test": {}}}
            })
        if "/join/" in url:
            return FakeResponse({})
        if "joined_rooms" in url:
            poll_count[0] += 1
            if poll_count[0] >= 3:
                return FakeResponse({"joined_rooms": ["!late:test"]})
            return FakeResponse({"joined_rooms": []})
        return FakeResponse({})

    monkeypatch.setattr(urllib.request, "urlopen", fake_urlopen)
    monkeypatch.setattr("time.sleep", lambda _: None)

    worker = Worker(_config(tmp_path))
    headers = {"Authorization": "Bearer tok"}
    rooms = worker._wait_for_matrix_rooms(
        "http://matrix:6167", headers, timeout=120, poll_interval=3,
    )

    assert rooms == ["!late:test"]
    assert poll_count[0] == 3


@pytest.mark.anyio
async def test_worker_marks_matrix_healthy_after_startup_relogin(tmp_path, monkeypatch):
    class FakeLoginResponse:
        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

        def read(self):
            return b'{"access_token":"new-token","device_id":"DEV1"}'

    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr(
        "copaw_worker.sync.FileSync.get_config",
        lambda _self: {
            "channels": {
                "matrix": {
                    "homeserver": "http://matrix:6167",
                    "accessToken": "old-token",
                }
            }
        },
    )
    monkeypatch.setattr("copaw_worker.sync.FileSync._cat", lambda _self, _key: "password")
    monkeypatch.setattr("urllib.request.urlopen", lambda *_args, **_kwargs: FakeLoginResponse())
    monkeypatch.setattr("copaw_worker.sync.FileSync.list_skills", lambda _self: [])
    monkeypatch.setattr(
        "copaw_worker.worker.check_model_service",
        lambda _cfg: ComponentHealth("healthy", "model provider reachable"),
    )
    monkeypatch.setattr("copaw_worker.worker.bridge_standard_to_runtime", lambda *_args, **_kwargs: None)
    monkeypatch.setattr("copaw_worker.worker.push_loop", _finished_push_loop)
    monkeypatch.setattr("copaw_worker.worker.sync_loop", _finished_pull_loop)

    worker = Worker(_config(tmp_path))

    assert await worker.start() is True
    await worker.stop()

    data = _health_json(tmp_path)
    matrix = data["components"]["matrix"]
    assert matrix["healthiness"] == "healthy"
    assert matrix["message"] == "matrix re-login succeeded"
    assert matrix["details"]["operation"] == "matrix_relogin"
    assert matrix["details"]["device_id"] == "DEV1"
    assert json.loads((Path(tmp_path) / "alice" / "openclaw.json").read_text())[
        "channels"
    ]["matrix"]["accessToken"] == "new-token"


@pytest.mark.anyio
async def test_worker_marks_matrix_unhealthy_when_startup_relogin_cannot_run(tmp_path, monkeypatch):
    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr(
        "copaw_worker.sync.FileSync.get_config",
        lambda _self: {"channels": {"matrix": {"homeserver": "http://matrix:6167"}}},
    )
    monkeypatch.setattr("copaw_worker.sync.FileSync._cat", lambda _self, _key: "")
    monkeypatch.setattr("copaw_worker.sync.FileSync.list_skills", lambda _self: [])
    monkeypatch.setattr(
        "copaw_worker.worker.check_model_service",
        lambda _cfg: ComponentHealth("healthy", "model provider reachable"),
    )
    monkeypatch.setattr("copaw_worker.worker.bridge_standard_to_runtime", lambda *_args, **_kwargs: None)
    monkeypatch.setattr("copaw_worker.worker.push_loop", _finished_push_loop)
    monkeypatch.setattr("copaw_worker.worker.sync_loop", _finished_pull_loop)

    worker = Worker(_config(tmp_path))

    assert await worker.start() is True
    await worker.stop()

    data = _health_json(tmp_path)
    matrix = data["components"]["matrix"]
    assert matrix["healthiness"] == "unhealthy"
    assert matrix["message"] == "matrix re-login skipped: missing homeserver or password"
    assert matrix["details"]["operation"] == "matrix_relogin"
    assert matrix["details"]["has_homeserver"] is True
    assert matrix["details"]["has_password"] is False


@pytest.mark.anyio
async def test_worker_builds_readiness_from_on_demand_checks(tmp_path, monkeypatch):
    worker = Worker(_config(tmp_path))
    worker._copaw_working_dir = tmp_path / "alice" / ".copaw"
    from copaw_worker.health import HealthState

    worker._health = HealthState(worker._copaw_working_dir / "health.json")
    worker._health.persist()
    worker._openclaw_cfg = {
        "channels": {"matrix": {"homeserver": "http://matrix:6167"}},
        "models": {"providers": {}},
    }

    monkeypatch.setattr(
        "copaw_worker.worker.check_copaw_service",
        lambda port: ComponentHealth(
            "healthy",
            "copaw health endpoint reachable",
            {"operation": "copaw_health_probe", "port": port},
        ),
    )
    monkeypatch.setattr(
        "copaw_worker.worker.check_model_service",
        lambda cfg: ComponentHealth(
            "unhealthy",
            "model provider is unreachable",
            {"operation": "model_preflight", "cfg_keys": sorted(cfg.keys())},
        ),
    )
    monkeypatch.setattr(
        "copaw_worker.worker.check_matrix_service",
        lambda homeserver: ComponentHealth(
            "healthy",
            "matrix homeserver reachable",
            {"operation": "matrix_endpoint_probe", "homeserver": homeserver},
        ),
    )

    snapshot = await worker.build_worker_readiness()
    data = _health_json(tmp_path)
    assert snapshot["readiness"] == "not_ready"
    assert snapshot["healthiness"] == "unhealthy"
    assert snapshot["components"]["copaw"]["healthiness"] == "healthy"
    assert snapshot["components"]["model"]["healthiness"] == "unhealthy"
    assert data["components"]["matrix"]["healthiness"] == "healthy"
    assert data["components"]["matrix"]["message"] == "matrix homeserver reachable"
    assert data["components"]["matrix"]["details"]["operation"] == "matrix_endpoint_probe"


@pytest.mark.anyio
async def test_worker_stops_worker_api_on_stop(tmp_path):
    worker = Worker(_config(tmp_path))
    worker_api = _FakeWorkerAPIServer(
        host="127.0.0.1",
        port=0,
        liveness_handler=worker.build_worker_liveness,
        readiness_handler=worker.build_worker_readiness,
    )
    worker._worker_api = worker_api

    await worker.stop()

    assert worker_api.stopped is True
    assert worker._worker_api is None


@pytest.mark.anyio
async def test_worker_liveness_is_lightweight(tmp_path):
    worker = Worker(_config(tmp_path))

    assert await worker.build_worker_liveness() == {
        "liveness": "alive",
        "message": "worker api alive",
        "details": {"worker_port": 8089},
    }


@pytest.mark.anyio
async def test_worker_marks_copaw_healthy_after_startup_probe(tmp_path, monkeypatch):
    worker = Worker(_config(tmp_path))
    worker._copaw_working_dir = tmp_path / "alice" / ".copaw"
    from copaw_worker.health import HealthState

    worker._health = HealthState(worker._copaw_working_dir / "health.json")
    worker._health.persist()

    calls = []

    def fake_check(port):
        calls.append(port)
        return ComponentHealth(
            "healthy",
            "copaw health endpoint reachable",
            {"operation": "copaw_health_probe", "port": port},
        )

    monkeypatch.setattr("copaw_worker.worker.check_copaw_service", fake_check)

    await worker._mark_copaw_startup_health(timeout=1, interval=0)

    data = _health_json(tmp_path)
    copaw = data["components"]["copaw"]
    assert calls == [8088]
    assert copaw["healthiness"] == "healthy"
    assert copaw["message"] == "copaw health endpoint reachable"
    assert copaw["details"]["operation"] == "copaw_health_probe"


@pytest.mark.anyio
async def test_worker_marks_copaw_unhealthy_when_app_exits_unexpectedly(tmp_path, monkeypatch):
    class FakeConfig:
        def __init__(self, *_args, **_kwargs):
            pass

    class FakeServer:
        def __init__(self, _config):
            self.should_exit = False

        async def serve(self):
            return None

    fake_uvicorn = types.ModuleType("uvicorn")
    fake_uvicorn.Config = FakeConfig
    fake_uvicorn.Server = FakeServer
    monkeypatch.setitem(sys.modules, "uvicorn", fake_uvicorn)

    fake_registry = types.ModuleType("copaw.app.channels.registry")
    fake_registry.clear_builtin_channel_cache = lambda: None
    monkeypatch.setitem(sys.modules, "copaw", types.ModuleType("copaw"))
    monkeypatch.setitem(sys.modules, "copaw.app", types.ModuleType("copaw.app"))
    monkeypatch.setitem(sys.modules, "copaw.app.channels", types.ModuleType("copaw.app.channels"))
    monkeypatch.setitem(sys.modules, "copaw.app.channels.registry", fake_registry)

    fake_hooks = types.ModuleType("copaw_worker.hooks")
    fake_hooks.install_tool_hooks = lambda: None
    monkeypatch.setitem(sys.modules, "copaw_worker.hooks", fake_hooks)

    worker = Worker(_config(tmp_path))
    worker._copaw_working_dir = tmp_path / "alice" / ".copaw"
    from copaw_worker.health import HealthState

    worker._health = HealthState(worker._copaw_working_dir / "health.json")
    worker._health.persist()

    await worker._run_copaw()

    data = _health_json(tmp_path)
    copaw = data["components"]["copaw"]
    assert copaw["healthiness"] == "unhealthy"
    assert copaw["message"] == "CoPaw app exited unexpectedly"
    assert copaw["details"]["operation"] == "run_copaw"


@pytest.mark.anyio
async def test_worker_installs_hooks_before_copaw_runner(tmp_path, monkeypatch):
    calls = []

    fake_hooks = types.ModuleType("copaw_worker.hooks")
    fake_hooks.install_tool_hooks = lambda: calls.append("hooks")
    monkeypatch.setitem(sys.modules, "copaw_worker.hooks", fake_hooks)

    config = _config(tmp_path)
    config.console_port = None
    worker = Worker(config)

    async def fake_headless():
        calls.append("headless")

    monkeypatch.setattr(worker, "_run_copaw_headless", fake_headless)

    await worker._run_copaw()

    assert calls == ["hooks", "headless"]
