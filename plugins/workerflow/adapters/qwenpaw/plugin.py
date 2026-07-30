"""WorkerFlow integration implemented with QwenPaw 2 public plugin APIs."""

from __future__ import annotations

from pathlib import Path
from typing import Any


PLUGIN_DIR = Path(__file__).resolve().parent
ASSET_DIR = PLUGIN_DIR / "workerflow"
if not (ASSET_DIR / "plugin.yaml").exists():
    ASSET_DIR = PLUGIN_DIR.parent.parent


class WorkerFlowPlugin:
    def register(self, api: Any) -> None:
        api.register_skill_provider(
            ASSET_DIR / "skills" / "agent",
            enabled_by_default=True,
            channels=["all"],
        )
        self._register_http(api)

    def _register_http(self, api: Any) -> None:
        try:
            from fastapi import APIRouter
        except ImportError:
            return
        router = APIRouter()

        @router.get("/health")
        def health() -> dict[str, Any]:
            return {"ok": True, "plugin": "workerflow", "adapter": "qwenpaw-2"}

        @router.post("/sync")
        def sync_endpoint() -> dict[str, Any]:
            return {
                "ok": True,
                "plugin": "workerflow",
                "managedBy": "qwenpaw-plugin-api",
            }

        api.register_http_router(
            router,
            prefix="/workerflow",
            tags=["workerflow"],
        )


plugin = WorkerFlowPlugin()
