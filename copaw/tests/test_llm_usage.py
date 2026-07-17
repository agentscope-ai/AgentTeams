import httpx
import pytest

from copaw_worker.llm_usage import (
    LLMUsageTracker,
    _is_llm_gateway_call,
    configure_llm_usage_from_openclaw,
    gateway_prefixes_from_openclaw,
    get_llm_usage_tracker,
    install_llm_usage_hooks,
)


def test_gateway_prefixes_from_openclaw_collects_provider_base_urls():
    cfg = {
        "models": {
            "providers": {
                "gw": {"baseUrl": "http://higress-gateway/v1/"},
                "other": {"baseUrl": "http://ignored.example/v1"},
            }
        }
    }

    assert gateway_prefixes_from_openclaw(cfg) == [
        "http://higress-gateway/v1",
        "http://ignored.example/v1",
    ]


def test_is_llm_gateway_call_matches_chat_completions_under_prefix():
    prefixes = ("http://higress-gateway/v1",)
    url = "http://higress-gateway/v1/chat/completions"

    assert _is_llm_gateway_call(url, prefixes) is True
    assert _is_llm_gateway_call("http://matrix.example/_matrix/client/versions", prefixes) is False
    assert _is_llm_gateway_call("http://higress-gateway/v1/models", prefixes) is False


def test_tracker_counts_only_gateway_llm_posts():
    tracker = LLMUsageTracker()
    tracker.configure_gateway_prefixes(["http://higress-gateway/v1"])

    tracker.note_request("GET", "http://higress-gateway/v1/chat/completions")
    tracker.note_request("POST", "http://higress-gateway/v1/chat/completions")
    tracker.note_request("POST", "http://other-gateway/v1/chat/completions")
    tracker.note_request("POST", "http://higress-gateway/v1/embeddings")

    assert tracker.take_for_report() == 2


def test_tracker_restore_after_failed_report():
    tracker = LLMUsageTracker()
    tracker.configure_gateway_prefixes(["http://higress-gateway/v1"])
    tracker.note_request("POST", "http://higress-gateway/v1/chat/completions")

    pending = tracker.take_for_report()
    assert pending == 1
    assert tracker.take_for_report() == 0

    tracker.restore_after_failed_report(pending)
    assert tracker.take_for_report() == 1


def test_configure_llm_usage_from_openclaw_updates_shared_tracker():
    tracker = get_llm_usage_tracker()
    configure_llm_usage_from_openclaw(
        {"models": {"providers": {"gw": {"baseUrl": "http://gateway/v1"}}}}
    )

    tracker.note_request("POST", "http://gateway/v1/chat/completions")
    assert tracker.take_for_report() == 1


@pytest.mark.asyncio
async def test_install_llm_usage_hooks_counts_sync_httpx_requests():
    import copaw_worker.llm_usage as llm_usage_module

    llm_usage_module._HOOK_INSTALLED = False
    install_llm_usage_hooks()

    tracker = get_llm_usage_tracker()
    tracker.configure_gateway_prefixes(["http://gateway/v1"])

    transport = httpx.MockTransport(lambda request: httpx.Response(200, json={"ok": True}))
    with httpx.Client(transport=transport) as client:
        client.post("http://gateway/v1/chat/completions", json={"model": "x"})
        client.get("http://gateway/v1/chat/completions")

    assert tracker.take_for_report() == 1
