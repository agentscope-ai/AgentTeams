"""Test harness for loading the QwenPaw Matrix channel overlay in isolation."""

from __future__ import annotations

import importlib.util
import sys
import types
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[1]
OVERLAY = ROOT / "src" / "matrix" / "channel.py"


def _install_module(name: str, **attrs):
    module = types.ModuleType(name)
    module.__dict__.update(attrs)
    sys.modules[name] = module
    return module


def load_overlay_module():
    root = "_qwenpaw_overlay_test"

    class _Dummy:
        pass

    class _BaseChannel:
        def __init__(self, *args, **kwargs):
            self.streaming_enabled = kwargs.get("streaming_enabled", True)

        async def _on_consume_error(self, _request, _to_handle, _err_text):
            return None

    class _ContentType:
        TEXT = "text"
        REFUSAL = "refusal"
        DATA = "data"
        IMAGE = "image"
        VIDEO = "video"
        AUDIO = "audio"
        FILE = "file"

    class _MessageType:
        MESSAGE = "MESSAGE"
        REASONING = "REASONING"

    class _RunStatus:
        Completed = "COMPLETED"
        InProgress = "INPROGRESS"

    def _content_init(self, **kwargs):
        self.__dict__.update(kwargs)

    content_class = type("_Content", (), {"__init__": _content_init})

    _install_module(root)
    _install_module(f"{root}.app")
    _install_module(f"{root}.app.channels")
    _install_module(f"{root}.app.channels.matrix")
    _install_module(f"{root}.app.channels.base", BaseChannel=_BaseChannel)
    _install_module(
        f"{root}.app.channels.utils",
        file_url_to_local_path=lambda value: value,
    )
    _install_module(f"{root}.constant", WORKING_DIR="/tmp")

    _install_module(
        "nio",
        **{
            name: _Dummy
            for name in (
                "AsyncClient",
                "AsyncClientConfig",
                "KeysUploadResponse",
                "LoginResponse",
                "KeyVerificationCancel",
                "KeyVerificationEvent",
                "KeyVerificationKey",
                "KeyVerificationMac",
                "KeyVerificationStart",
                "LocalProtocolError",
                "MatrixRoom",
                "MegolmEvent",
                "RoomEncryptedAudio",
                "RoomEncryptedFile",
                "RoomEncryptedImage",
                "RoomEncryptedVideo",
                "RoomMessageAudio",
                "RoomMessageFile",
                "RoomMessageImage",
                "RoomMessageText",
                "RoomMessageVideo",
                "SyncResponse",
                "ToDeviceEvent",
                "ToDeviceError",
                "UploadResponse",
            )
        },
    )
    _install_module("nio.event_builders")
    _install_module("nio.event_builders.direct_messages", ToDeviceMessage=_Dummy)
    _install_module("nio.events")
    _install_module(
        "nio.events.to_device",
        RoomKeyRequest=_Dummy,
        RoomKeyRequestCancellation=_Dummy,
    )
    _install_module(
        "nio.responses",
        JoinedMembersResponse=_Dummy,
        RoomGetStateEventResponse=_Dummy,
        RoomSendError=_Dummy,
        SyncError=_Dummy,
        WhoamiResponse=_Dummy,
    )
    _install_module("agentscope_runtime")
    _install_module("agentscope_runtime.engine")
    _install_module("agentscope_runtime.engine.schemas")
    _install_module(
        "agentscope_runtime.engine.schemas.agent_schemas",
        AudioContent=content_class,
        ContentType=_ContentType,
        FileContent=content_class,
        ImageContent=content_class,
        MessageType=_MessageType,
        RunStatus=_RunStatus,
        TextContent=content_class,
        VideoContent=content_class,
    )

    module_name = f"{root}.app.channels.matrix.channel"
    spec = importlib.util.spec_from_file_location(module_name, OVERLAY)
    module = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = module
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module
