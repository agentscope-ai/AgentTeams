"""AgentTeams upgrade plugin: the /api/upgrade/{check,run,status} endpoints.

Backend-only (no console frontend). The endpoints are thin HTTP adapters
over ``qwenpaw_worker.upgrade``:

* ``GET  /api/upgrade/check``  -> ``checker.run_check``: live preflight
  gates (C1-C4) over the version index; always 200, network problems land
  in ``errors[]`` (design doc §4.4).
* ``POST /api/upgrade/run``    -> ``runner.start_upgrade``: validates,
  pre-downloads the wheel (mandatory C3 input), detaches the two-phase
  orchestrator (``--phase pre-restart``), returns the job handle (202) —
  design doc §4.5/§4.8.
* ``GET  /api/upgrade/status`` -> ``runner.job_status``: re-reads the
  on-disk job state (single source of truth across container restarts) —
  design doc §4.6.

Error bodies stay flat (``{"error": ...}``) on 400/404/409/501.
"""
from typing import Optional

try:  # the plugin host (qwenpaw console web stack) always has fastapi
    from fastapi import APIRouter, Query
    from fastapi.responses import JSONResponse
except ImportError:  # pragma: no cover
    APIRouter = None  # type: ignore[assignment]

# Module-level import of the Pydantic body model. With `from __future__
# import annotations` the /run annotation is a deferred string, so FastAPI
# resolves it in the module globals — the name must be a real module-level
# symbol (a local inside register() would not be found).
try:
    from qwenpaw_worker.upgrade.runner import UpgradeRunBody as _UpgradeRunBody
except ImportError:  # pragma: no cover
    _UpgradeRunBody = None  # type: ignore


class AgentTeamsUpgradePlugin:
    """Plugin entry object (qwenpaw plugin API: ``register(api)``)."""

    def register(self, api) -> None:
        if APIRouter is None:
            return
        from qwenpaw_worker.upgrade import checker, runner
        from qwenpaw_worker.upgrade.runner import (
            UpgradeInProgress,
            UpgradeNotFound,
            UpgradeRejected,
            UpgradeUnsupported,
        )

        router = APIRouter()

        @router.get("/check")
        async def check_upgrade(
            verify: bool = Query(False),
            include_prerelease: bool = Query(False),
            index_url: Optional[str] = Query(None),
        ):
            # run_check never raises: index/patch problems are reported in
            # the payload (errors[] / per-candidate gates), status stays 200.
            payload = checker.run_check(
                verify=verify,
                include_prerelease=include_prerelease,
                index_url=index_url,
            )
            return JSONResponse(payload, status_code=200)

        @router.post("/run")
        async def run_upgrade(body: _UpgradeRunBody):
            try:
                result = runner.start_upgrade(body)
            except UpgradeUnsupported as exc:
                return JSONResponse({"error": str(exc)}, status_code=501)
            except UpgradeInProgress as exc:
                return JSONResponse(
                    {"error": "upgrade_in_progress", "job_id": exc.job_id},
                    status_code=409,
                )
            except UpgradeRejected as exc:
                return JSONResponse({"error": str(exc)}, status_code=400)
            return JSONResponse(result, status_code=202)

        @router.get("/status")
        async def upgrade_status(job_id: Optional[str] = Query(None)):
            try:
                payload = runner.job_status(job_id)
            except UpgradeNotFound as exc:
                return JSONResponse(
                    {"error": "job_not_found", "job_id": exc.job_id},
                    status_code=404,
                )
            return JSONResponse(payload, status_code=200)

        api.register_http_router(router, prefix="/upgrade", tags=["agentteams-upgrade"])


plugin = AgentTeamsUpgradePlugin()
