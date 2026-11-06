"""Production sidecar runtime: core + 0600 socket + bridge, one process.

    python3 -m sidecar --config /path/to/sidecar-config.json

or, for operators, via ``sidecar.sh`` (start/stop/restart/status/run).
The Matrix core is loaded from the checkout ``MATRIX_CORE_DIR`` points
at (fail-closed with an actionable message when absent). Encryption is
OFF — the same choice as the e2e harness: the AgentTeams homeserver
runs in clear. SIGTERM/SIGINT shut down cleanly (core stopped, socket
file removed); a second signal force-exits.
"""

from __future__ import annotations

import argparse
import asyncio
import dataclasses
import logging
import os
import signal
import sys
import threading
from typing import Optional

from sidecar import _core
from sidecar import media as media_mod
from sidecar.bridge import AgentBridge, run_outbound_worker
from sidecar.config import ConfigError, load_config

log = logging.getLogger("sidecar")

SHUTDOWN_TIMEOUT = 10.0


_bridge_logger = logging.getLogger("sidecar.bridge")


def bridge_log(message: str) -> None:
    """Line sink shared by the bridge and the media enricher."""

    _bridge_logger.info(message)


@dataclasses.dataclass
class SidecarRuntime:
    """Everything build() wires together; ownership stays in one place.

    Mutable on purpose: build() fills the slots as the components come
    up and the worker thread reads the finished object through the
    reference it was handed.
    """

    config: object
    state_dir: str
    store: object
    server: object
    bridge: AgentBridge
    core: object
    transport: object
    loop: asyncio.AbstractEventLoop
    loop_thread: threading.Thread
    worker: threading.Thread
    core_task: Optional[object] = None
    shutdown_requested: bool = False


def build(config) -> SidecarRuntime:
    """Construct the full runtime (fail-closed, nothing leaked).

    The core and the worker are NOT started here — run() starts them.
    """
    lib = _core.load_core()
    core_mod, transport_mod = lib.core, lib.transport

    state_dir = os.path.dirname(config.socket_path)
    os.makedirs(state_dir, exist_ok=True)
    os.chmod(state_dir, 0o700)

    store = core_mod.FileSessionStore(state_dir)
    server = core_mod.SidecarServer(config.socket_path)
    bridge = None
    core = None
    transport = None
    loop = None
    loop_thread = None
    try:
        server.start()
        media_dir = os.path.join(
            os.path.dirname(os.path.abspath(config.socket_path)), "media", "inbound"
        )

        def _enrich_frame(frame: dict) -> int:
            return media_mod.fetch_frame_media(
                config.homeserver_url,
                config.agent_access_token,
                frame,
                media_dir,
                log=bridge_log,
            )

        bridge = AgentBridge(
            server, config.agent_user, log=bridge_log, media_enricher=_enrich_frame
        )
        transport = transport_mod.NioTransport(
            transport_mod.TransportConfig(
                homeserver=config.homeserver_url,
                user_id=config.agent_user,
                access_token=config.agent_access_token,
                encryption=False,
            ),
            store=store,
            state_dir=state_dir,
            encryption=False,
        )
        core = core_mod.MatrixCore(
            transport,
            store,
            bridge,
            core_mod.CoreConfig(state_dir, config.agent_name, config.socket_path),
        )
        loop = asyncio.new_event_loop()
        loop_thread = threading.Thread(
            target=loop.run_forever, name="sidecar-loop", daemon=True
        )
        loop_thread.start()
    except BaseException:
        # do not leak the socket file (or the loop) on a failed build
        if server is not None:
            try:
                server.stop()
            except Exception:
                pass
        if loop is not None and loop_thread is not None:
            try:
                loop.call_soon_threadsafe(loop.stop)
                loop_thread.join(timeout=5)
            except Exception:
                pass
        raise

    runtime = SidecarRuntime(
        config=config,
        state_dir=state_dir,
        store=store,
        server=server,
        bridge=bridge,
        core=core,
        transport=transport,
        loop=loop,
        loop_thread=loop_thread,
        worker=None,
    )
    runtime.worker = threading.Thread(
        target=_worker_supervised,
        args=(runtime, loop),
        name="sidecar-worker",
        daemon=True,
    )
    return runtime


def _worker_supervised(runtime: SidecarRuntime, loop) -> None:
    """Re-run the outbound worker while the sidecar is alive.

    run_outbound_worker returns when the client connection ends (or a
    submit fails). The core's SidecarServer keeps accepting
    reconnections meanwhile, so the worker must come back and wait for
    the next one — otherwise the sidecar is half-dead after the first
    OpenClaw gateway restart and nobody notices.
    """
    while not runtime.shutdown_requested:
        try:
            run_outbound_worker(
                runtime.server,
                runtime.core,
                loop,
                should_stop=lambda: runtime.shutdown_requested,
            )
        except Exception:
            log.exception("sidecar worker crashed; restarting it")
        if runtime.shutdown_requested:
            return
        log.info(
            "sidecar worker idle: waiting for the next connection "
            "(socket %s)",
            runtime.config.socket_path,
        )


def shutdown(
    runtime: SidecarRuntime,
    stop_event: threading.Event,
    timeout: float = SHUTDOWN_TIMEOUT,
) -> None:
    """Tear down in an order that keeps the protocol honest."""
    stop_event.set()
    runtime.shutdown_requested = True
    if runtime.core_task is not None:
        try:
            asyncio.run_coroutine_threadsafe(
                runtime.core.stop(), runtime.loop
            ).result(timeout=timeout)
        except Exception as exc:
            log.warning("core.stop() did not complete cleanly: %r", exc)
        try:
            runtime.core_task.result(timeout=timeout)
        except Exception as exc:
            log.warning("core did not stop cleanly: %r", exc)
    # server.stop() closes the connection and removes the socket file.
    try:
        runtime.server.stop()
    except Exception as exc:
        log.warning("sidecar server stop failed: %r", exc)
    # build() creates the worker thread but does not start it (run()
    # does), so join() must only be attempted when the thread is
    # actually alive — joining a never-started thread raises.
    if runtime.worker is not None and runtime.worker.is_alive():
        runtime.worker.join(timeout=5)
    try:
        runtime.loop.call_soon_threadsafe(runtime.loop.stop)
        runtime.loop_thread.join(timeout=5)
    except Exception:
        pass
    log.info("sidecar stopped (socket removed: %s)", runtime.config.socket_path)


def run(config) -> None:
    """Build the runtime and block until SIGTERM/SIGINT; then shut down."""
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s [sidecar] %(message)s",
        stream=sys.stderr,
    )
    stop_event = threading.Event()
    is_main = threading.current_thread() is threading.main_thread()
    previous_handlers = {}

    def _handle(signum, _frame):
        if stop_event.is_set():
            log.warning("second signal received; forcing exit")
            os._exit(1)
        log.info("signal %s received; shutting down", signum)
        stop_event.set()

    if is_main:
        for signum in (signal.SIGTERM, signal.SIGINT):
            previous_handlers[signum] = signal.signal(signum, _handle)

    try:
        runtime = build(config)
    except Exception as exc:
        log.error("startup failed: %s", exc)
        raise SystemExit(1)

    try:
        runtime.worker.start()
        runtime.core_task = asyncio.run_coroutine_threadsafe(
            runtime.core.run(), runtime.loop
        )
        log.info(
            "sidecar ready: agent=%s socket=%s core=%s",
            config.agent_name,
            config.socket_path,
            _core.core_dir(),
        )
        while not stop_event.wait(0.5):
            if runtime.core_task.done():
                exc = runtime.core_task.exception()
                log.error("core stopped at startup: %r", exc)
                raise SystemExit(1)
    finally:
        if is_main:
            for signum, handler in previous_handlers.items():
                signal.signal(signum, handler)
        shutdown(runtime, stop_event)


def main(argv: Optional[list] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="sidecar",
        description="AgentTeams Matrix sidecar — production entry",
    )
    parser.add_argument(
        "--config",
        default=None,
        help="path to sidecar-config.json (default: "
        "$AGENTTEAMS_MATRIX_SIDECAR_CONFIG or "
        "<state dir>/sidecar-config.json)",
    )
    args = parser.parse_args(argv)
    try:
        config = load_config(args.config)
    except ConfigError as exc:
        print(f"sidecar: configuration error: {exc}", file=sys.stderr)
        return 1
    try:
        run(config)
    except SystemExit as exc:
        return exc.code if isinstance(exc.code, int) else 1
    return 0
