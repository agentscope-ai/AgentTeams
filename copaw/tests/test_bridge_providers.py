"""Focused tests for the OpenClaw-to-CoPaw provider bridge."""

import json

from copaw_worker.bridge import _write_providers_json


def test_provider_model_input_modalities_are_preserved(tmp_path):
    """Provider model inputs become CoPaw model capability annotations."""
    cfg = {
        "models": {
            "providers": {
                "gw": {
                    "baseUrl": "http://aigw:8080/v1",
                    "apiKey": "key123",
                    "models": [
                        {
                            "id": "vision-model",
                            "name": "Vision Model",
                            "input": ["text", "image"],
                        },
                        {"id": "video-model", "input": ["text", "video"]},
                        {"id": "text-model", "input": ["text"]},
                        {"id": "unknown-model"},
                    ],
                },
            },
        },
        "agents": {
            "defaults": {"model": {"primary": "gw/vision-model"}},
        },
    }

    _write_providers_json(cfg, tmp_path, in_container=False)

    providers = json.loads((tmp_path / "providers.json").read_text())
    models = providers["custom_providers"]["gw"]["models"]
    assert models == [
        {
            "id": "vision-model",
            "name": "Vision Model",
            "supports_image": True,
            "supports_video": False,
            "supports_multimodal": True,
        },
        {
            "id": "video-model",
            "name": "video-model",
            "supports_image": False,
            "supports_video": True,
            "supports_multimodal": True,
        },
        {
            "id": "text-model",
            "name": "text-model",
            "supports_image": False,
            "supports_video": False,
            "supports_multimodal": False,
        },
        {"id": "unknown-model", "name": "unknown-model"},
    ]
