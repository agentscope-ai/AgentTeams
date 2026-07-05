import json

import pytest

from copaw_worker.health import (
    COMPONENTS,
    HealthState,
    check_copaw_service,
    check_matrix_service,
    check_model_service,
)


def test_health_state_starts_with_full_unhealthy_snapshot(tmp_path):
    state_path = tmp_path / ".copaw" / "health.json"

    health = HealthState(state_path)
    snapshot = health.persist()

    data = json.loads(state_path.read_text())
    assert snapshot.healthiness == "unhealthy"
    assert data["healthiness"] == "unhealthy"
    assert data["message"] == "copaw unhealthy: not checked yet"
    assert set(data["components"]) == set(COMPONENTS)
    assert all(
        component["healthiness"] == "unhealthy"
        for component in data["components"].values()
    )
    assert all(
        component["message"] == "not checked yet"
        for component in data["components"].values()
    )


def test_component_update_only_changes_that_component_and_reaggregates(tmp_path):
    state_path = tmp_path / ".copaw" / "health.json"
    health = HealthState(state_path)

    snapshot = health.update("sync", "healthy", "mirror restored")

    data = json.loads(state_path.read_text())
    assert snapshot.healthiness == "unhealthy"
    assert data["healthiness"] == "unhealthy"
    assert data["message"] == "copaw unhealthy: not checked yet"
    assert data["components"]["sync"]["healthiness"] == "healthy"
    assert data["components"]["sync"]["message"] == "mirror restored"
    assert data["components"]["copaw"]["healthiness"] == "unhealthy"
    assert data["components"]["copaw"]["message"] == "not checked yet"


def test_snapshot_becomes_healthy_when_all_components_are_healthy(tmp_path):
    state_path = tmp_path / ".copaw" / "health.json"
    health = HealthState(state_path)

    for component in COMPONENTS:
        snapshot = health.update(component, "healthy", f"{component} ok")

    data = json.loads(state_path.read_text())
    assert snapshot.healthiness == "healthy"
    assert snapshot.message == "all components healthy"
    assert data["healthiness"] == "healthy"
    assert data["message"] == "all components healthy"


def test_health_state_rejects_invalid_component_or_healthiness(tmp_path):
    health = HealthState(tmp_path / "health.json")

    with pytest.raises(ValueError, match="unknown health component"):
        health.update("storage", "healthy", "ok")

    with pytest.raises(ValueError, match="invalid healthiness"):
        health.update("sync", "degraded", "not supported")


def test_check_model_service_uses_minimal_chat_completion_preflight(monkeypatch):
    opened = {}

    class FakeResponse:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

    def fake_urlopen(req, timeout):
        opened["url"] = req.full_url
        opened["method"] = req.get_method()
        opened["authorization"] = req.headers.get("Authorization")
        opened["content_type"] = req.headers.get("Content-type")
        opened["body"] = json.loads(req.data.decode("utf-8"))
        opened["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    result = check_model_service(
        {
            "models": {
                "providers": {
                    "openai": {
                        "baseUrl": "https://llm.example.com/v1",
                        "apiKey": "secret",
                        "models": [{"id": "gpt-test"}],
                    }
                }
            },
            "agents": {"defaults": {"model": {"primary": "openai/gpt-test"}}},
        },
        timeout=3,
    )

    assert result.healthiness == "healthy"
    assert result.message == "model chat completion preflight succeeded"
    assert opened == {
        "url": "https://llm.example.com/v1/chat/completions",
        "method": "POST",
        "authorization": "Bearer secret",
        "content_type": "application/json",
        "body": {
            "model": "gpt-test",
            "messages": [{"role": "user", "content": "ping"}],
            "max_tokens": 1,
        },
        "timeout": 3,
    }
    assert result.details["probe"] == "chat_completion"
    assert result.details["token_cost"] == "minimal"


def test_check_model_service_reports_unhealthy_without_active_provider():
    result = check_model_service({"models": {"providers": {}}})

    assert result.healthiness == "unhealthy"
    assert result.message == "no active model provider configured"
    assert result.details["operation"] == "model_preflight"


def test_check_model_service_reports_unhealthy_for_failed_chat_completion_preflight(monkeypatch):
    opened = []

    def fake_urlopen(req, timeout):
        opened.append(
            (
                req.full_url,
                req.get_method(),
                json.loads(req.data.decode("utf-8")),
                req.headers.get("Authorization"),
                timeout,
            )
        )
        raise urllib.error.HTTPError(
            req.full_url,
            404,
            "Not Found",
            hdrs=None,
            fp=None,
        )

    import urllib.error

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    result = check_model_service(
        {
            "models": {
                "providers": {
                    "hiclaw-gateway": {
                        "api": "openai-completions",
                        "baseUrl": "http://aigw-local.agentteams.io:8080/v1",
                        "apiKey": "secret",
                        "models": [{"id": "qwen3.5-plus"}],
                    }
                }
            },
            "agents": {"defaults": {"model": {"primary": "hiclaw-gateway/qwen3.5-plus"}}},
        },
        timeout=3,
    )

    assert result.healthiness == "unhealthy"
    assert result.message == "model chat completion preflight returned HTTP 404"
    assert result.details["endpoint"] == "http://aigw-local.agentteams.io:8080/v1/chat/completions"
    assert result.details["http_status"] == 404
    assert opened == [
        (
            "http://aigw-local.agentteams.io:8080/v1/chat/completions",
            "POST",
            {
                "model": "qwen3.5-plus",
                "messages": [{"role": "user", "content": "ping"}],
                "max_tokens": 1,
                "enable_thinking": False,
            },
            "Bearer secret",
            3,
        ),
    ]


def test_check_model_service_uses_max_completion_tokens_for_gpt5(monkeypatch):
    opened = {}

    class FakeResponse:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

    def fake_urlopen(req, timeout):
        opened["body"] = json.loads(req.data.decode("utf-8"))
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    result = check_model_service(
        {
            "models": {
                "providers": {
                    "hiclaw-gateway": {
                        "baseUrl": "http://aigw-local.agentteams.io:8080/v1",
                        "apiKey": "secret",
                        "models": [{"id": "gpt-5.4"}],
                    }
                }
            },
            "agents": {"defaults": {"model": {"primary": "hiclaw-gateway/gpt-5.4"}}},
        },
    )

    assert result.healthiness == "healthy"
    assert opened["body"]["max_completion_tokens"] == 1
    assert "max_tokens" not in opened["body"]


def test_check_copaw_service_uses_local_health_endpoint(monkeypatch):
    opened = {}

    class FakeResponse:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

    def fake_urlopen(req, timeout):
        opened["url"] = req.full_url
        opened["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    result = check_copaw_service(18799, timeout=2)

    assert result.healthiness == "healthy"
    assert result.message == "copaw health endpoint reachable"
    assert opened == {
        "url": "http://127.0.0.1:18799/health",
        "timeout": 2,
    }


def test_check_copaw_service_requires_http_200(monkeypatch):
    class FakeResponse:
        status = 503

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

    monkeypatch.setattr("urllib.request.urlopen", lambda *_args, **_kwargs: FakeResponse())

    result = check_copaw_service(18799)

    assert result.healthiness == "unhealthy"
    assert result.message == "copaw health endpoint returned HTTP 503"
    assert result.details["operation"] == "copaw_health_probe"


def test_check_matrix_service_uses_versions_endpoint(monkeypatch):
    opened = {}

    class FakeResponse:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

    def fake_urlopen(req, timeout):
        opened["url"] = req.full_url
        opened["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    result = check_matrix_service("http://matrix:6167/", timeout=4)

    assert result.healthiness == "healthy"
    assert result.message == "matrix homeserver reachable"
    assert opened == {
        "url": "http://matrix:6167/_matrix/client/versions",
        "timeout": 4,
    }


def test_check_matrix_service_reports_unhealthy_without_homeserver():
    result = check_matrix_service("")

    assert result.healthiness == "unhealthy"
    assert result.message == "matrix homeserver is not configured"
    assert result.details["operation"] == "matrix_endpoint_probe"
