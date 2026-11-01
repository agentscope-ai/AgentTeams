# -*- coding: utf-8 -*-
"""P3 — expose the Qwen Code web surface through QwenPaw.

``qwen serve`` (HTTP + Web UI + token) runs locally; this module gives the
plugin a small reverse proxy plus a reachability probe, so the Qwen Code
web features (sessions, streaming, tool calls) can be opened from the
QwenPaw side without embedding a custom frontend.

Env:
  QWEN_CODE_BRIDGE_SERVE_URL    base URL (default ``http://127.0.0.1:8931``)
  QWEN_CODE_BRIDGE_SERVE_TOKEN  bearer token forwarded to ``qwen serve``

All failures are reported as data, never raised into the caller's turn.
"""

from __future__ import annotations

import asyncio
import json
import logging
import mimetypes
import os
import shutil
import re
import secrets
import socket
import time
from pathlib import Path

import httpx
from fastapi import APIRouter, Request, WebSocket
from fastapi.responses import JSONResponse, Response

logger = logging.getLogger(__name__)

SERVE_URL_ENV = "QWEN_CODE_BRIDGE_SERVE_URL"
SERVE_AUTOSTART_ENV = "QWEN_CODE_BRIDGE_SERVE_AUTOSTART"
SERVE_WORKSPACE_ENV = "QWEN_CODE_BRIDGE_SERVE_WORKSPACE"
WEB_DIST_ENV = "QWEN_CODE_BRIDGE_WEB_DIST"
SERVE_TOKEN_ENV = "QWEN_CODE_BRIDGE_SERVE_TOKEN"
#: Fail-closed switch: when the SDK auth module cannot be imported, refuse
#: every bridge WebSocket (4401) instead of keeping the open behaviour.
REQUIRE_WS_AUTH_ENV = "QWEN_CODE_BRIDGE_REQUIRE_WS_AUTH"
#: Denial close code, mirroring ``RuntimeBoundaryMiddleware``.
WS_AUTH_DENY_CODE = 4401
DEFAULT_SERVE_URL = ""  # resolved from env/serve.json; auto-start allocates a free port

#: Where this proxy is mounted; the embedded console treats it as its root.
WEB_PREFIX = "/api/qwen-code-bridge/web"

#: WebSocket destination scoping shared by both injected shims (P1411 round
#: 2): the console's login token (``localStorage["qwenpaw_auth_token"]``) is
#: attached ONLY to the bridge destination — a same-origin URL whose path is
#: the mount prefix or below it — and never to a cross-origin URL or an
#: unrelated same-origin path. The qwen serve web-shell's own WebSocket
#: routes (``/workspaces``, ``/sessions``, ``/session``, ``/session-groups``,
#: ``/git``, ``/terminals``) are mapped onto the mount prefix first, so the
#: shell keeps working when it is served at the origin root; every other URL
#: is passed through untouched. Plain ES5 (no arrow functions) so it runs in
#: the console browser as-is and executes directly in the node tests.
_WS_TARGET_JS = r"""
function bridgePath(p){return p===B||p.indexOf(B+"/")===0;}
function shellPath(p){return /^\/(?:workspaces|sessions|session|session-groups|git|terminals)(?:\/|$)/.test(p);}
function bridgeTarget(u){try{
  if(typeof u!=="string"||!u)return u;
  var pageHost=location.host||"";
  if(!pageHost){var om=/^[a-zA-Z][a-zA-Z0-9+.\-]*:\/\/([^\/]+)/.exec(location.origin||"");if(om)pageHost=om[1];}
  var m=/^([a-zA-Z][a-zA-Z0-9+.\-]*:)\/\/([^\/?#]*)([^?#]*)(\?[^#]*)?(#.*)?$/.exec(u);
  var p,search,hash,abs=false;
  if(m){
    var scheme=m[1].toLowerCase();
    if(scheme!=="ws:"&&scheme!=="wss:")return u;
    if(!pageHost||m[2]!==pageHost)return u;
    p=m[3]||"/";search=m[4]||"";hash=m[5]||"";abs=true;
  }else{
    var base=location.href||((location.origin||"")+(location.pathname||"/"));
    var a;try{a=new URL(u,base);}catch(e){return u;}
    p=a.pathname||"/";search=a.search||"";hash=a.hash||"";
  }
  var out=u;
  if(!bridgePath(p)){
    if(!shellPath(p))return u;
    var proto=abs?m[1].toLowerCase():((location.protocol==="https:")?"wss:":"ws:");
    out=proto+"//"+pageHost+B+p+search+hash;
  }
  if(/[?&]token=/.test(out))return out;
  var t=null;try{t=window.localStorage.getItem("qwenpaw_auth_token");}catch(e){}
  if(!t)return out;
  return out+(out.indexOf("?")>=0?"&":"?")+"token="+encodeURIComponent(t);
}catch(e){return u;}}
"""

#: Injected into the console HTML: prefix every root-absolute request the
#: shell makes (fetch / XHR / EventSource / Worker) so a prefixed mount
#: behaves like the original origin root; WebSocket URLs go through
#: ``bridgeTarget`` (see above) instead. ``__BRIDGE_PREFIX__`` and
#: ``__WS_TARGET_JS__`` are rendered once at import time — the injector and
#: the tests consume the finished string. Plain JavaScript (no HTML wrapper)
#: so tests can execute it directly; the injection sites wrap it in
#: ``<script>``.
_PREFIX_SHIM = """(function(){
var B=__BRIDGE_PREFIX__;
var P=location.pathname.replace(/\\/?[^/]*\\/?$/,"")||"";
function fix(u){try{
  if(typeof u!=="string") return u;
  if(u.indexOf(P+"/")===0) return u;
  if(u.charAt(0)==="/") return P+u;
  var o=location.origin;
  if(o && u.indexOf(o+"/")===0) return o+P+u.slice(o.length);
}catch(e){}return u;}
var f=window.fetch;if(f){window.fetch=function(u,o){
  try{if(typeof u==="object"&&u&&typeof u.url==="string"&&window.Request&&u instanceof Request){
    var nu=fix(u.url); if(nu!==u.url){u=new Request(nu,u);}
  }}catch(e){}
  return f.call(this,fix(u),o)};}
  try{if(typeof u==="object"&&u&&typeof u.url==="string"&&window.Request&&u instanceof Request){
    var nu=fix(u.url); if(nu!==u.url){u=new Request(nu,u);}
  }}catch(e){}
var x=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){var a=[].slice.call(arguments);a[1]=fix(u);return x.apply(this,a)};
__WS_TARGET_JS__
var W=window.WebSocket;if(W){window.WebSocket=function(u,p){return new W(bridgeTarget(u),p)};window.WebSocket.prototype=W.prototype;}
var E=window.EventSource;if(E){window.EventSource=function(u,o){return new E(fix(u),o)};window.EventSource.prototype=E.prototype;}
var WK=window.Worker;
if(WK){
  var bootCommon="var P="+JSON.stringify(P)+";function fix(u){try{if(typeof u!=='string')return u;if(u.indexOf(P+'/')===0)return u;if(u.charAt(0)==='/')return P+u;var o=location.origin;if(o&&u.indexOf(o+'/')===0)return o+P+u.slice(o.length);}catch(e){}return u;}var _f=self.fetch;self.fetch=function(u,o){return _f(fix(u),o)};if(self.XMLHttpRequest){var _x=self.XMLHttpRequest.prototype.open;self.XMLHttpRequest.prototype.open=function(m,u){var a=[].slice.call(arguments);a[1]=fix(u);return _x.apply(this,a)}};";
  window.Worker=function(u,o){try{
    if(String(u).indexOf("blob:")===0&&o&&o.type==="module"){
      var b2=bootCommon+"await import("+JSON.stringify(String(u))+");";
      var w2=URL.createObjectURL(new Blob([b2],{type:"text/javascript"}));
      return new WK(w2,{type:"module"});
    }
    if(String(u).indexOf("blob:")===0){
      var b1=bootCommon+"importScripts("+JSON.stringify(String(u))+");";
      var w1=URL.createObjectURL(new Blob([b1],{type:"text/javascript"}));
      return new WK(w1,o);
    }
  }catch(e){}return new WK(u,o)};
}
})();""".replace("__WS_TARGET_JS__", _WS_TARGET_JS).replace(
    "__BRIDGE_PREFIX__", json.dumps(WEB_PREFIX)
)


def _rewrite_console_html(html: bytes) -> bytes:
    """Prefix root-absolute shell assets and inject the request shim."""
    text = html.decode("utf-8", "ignore")
    for path in ("/assets/", "/manifest.webmanifest", "/favicon"):
        text = text.replace('="' + path, '="' + WEB_PREFIX + path)
    if _PREFIX_SHIM not in text:
        text = text.replace("<head>", "<head><script>" + _PREFIX_SHIM + "</script>", 1)
    return text.encode("utf-8")

router = APIRouter(tags=["qwen-code-bridge"])


_WS_SUFFIXES = ("sessions/live-state", "session-groups", "sessions", "session", "git")


def _fix_workspace_path(path: str) -> str:
    """Encode an absolute workspace selector (``/workspaces//tmp/...``).

    The shell emits ``/workspaces/<cwd>/<tail>`` unencoded; the daemon expects
    one path segment, so ``/tmp`` must become ``%2Ftmp``.
    """
    match = re.match(r"^(/?workspaces?)/(.*)$", path)
    if not match:
        return path
    base, rest = match.group(1), match.group(2)
    if not rest.startswith("/"):
        return path
    for suffix in _WS_SUFFIXES:
        idx = rest.rfind("/" + suffix)
        if idx > 0:
            from urllib.parse import quote

            workspace = rest[:idx]
            tail = rest[idx + 1:]
            return f"{base}/{quote(workspace, safe='')}/{tail}"
    return path


def web_dist() -> Path | None:
    """Optional rebuilt web-shell (base-prefixed). Served statically when present."""
    try:
        from .state import state_dir

        raw = os.environ.get(WEB_DIST_ENV, "").strip()
        candidates = []
        if raw:
            candidates.append(Path(raw))
        candidates.append(state_dir() / "web-shell" / "dist")
        # a "-full" plugin ZIP may ship the dist inside the plugin directory
        candidates.append(
            Path(__file__).resolve().parent.parent / "web-shell" / "dist",
        )
        for path in candidates:
            if (path / "index.html").is_file():
                return path
        return None
    except Exception:
        return None


#: Injected by ``_inject_daemon_base`` (rebuilt-shell path): the same
#: ``bridgeTarget`` destination scoping, with the mount prefix taken from
#: ``window.__QWEN_DAEMON_BASE__``. A URL that already carries a ``token=``
#: query parameter is not re-tokened. Plain JavaScript (no HTML wrapper) so
#: tests can execute it directly; ``_inject_daemon_base`` wraps it in
#: ``<script>``.
_DAEMON_WS_SHIM = (
    "(function(){var B=window.__QWEN_DAEMON_BASE__||'';"
    "var W=window.WebSocket;if(!W||!B)return;"
    + _WS_TARGET_JS
    + "window.WebSocket=function(u,p){return new W(bridgeTarget(u),p)};"
    "window.WebSocket.prototype=W.prototype;})()"
)


def _inject_daemon_base(data: bytes) -> bytes:
    html = data.decode("utf-8", "ignore")
    inject = (
        "<script>window.__QWEN_DAEMON_BASE__="
        + json.dumps(WEB_PREFIX)
        + ";</script>"
    )
    if "__QWEN_DAEMON_BASE__" not in html:
        wrapped = "<script>" + _DAEMON_WS_SHIM + "</script>"
        html = html.replace("<head>", "<head>" + inject + wrapped, 1)
    return html.encode("utf-8")


def _spa_index(dist: Path) -> bytes:
    return _inject_daemon_base((dist / "index.html").read_bytes())


def _config() -> dict:
    """Optional ``<state_dir>/serve.json`` = {url, token} (env wins)."""
    try:
        from .state import state_dir

        path: Path = state_dir() / "serve.json"
        data = json.loads(path.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except Exception:
        return {}


def serve_url() -> str:
    explicit = (
        os.environ.get(SERVE_URL_ENV, "").strip()
        or str(_config().get("url") or "").strip()
    )
    if explicit:
        return explicit.rstrip("/")
    return ""


def serve_token() -> str:
    return (
        os.environ.get(SERVE_TOKEN_ENV, "").strip()
        or str(_config().get("token") or "").strip()
    )


def _headers(request: Request | None = None) -> dict[str, str]:
    token = serve_token()
    headers = {"Accept": "application/json, text/plain, */*"}
    if request is not None:
        content_type = request.headers.get("content-type")
        if content_type:
            headers["Content-Type"] = content_type
        accept = request.headers.get("accept")
        if accept:
            headers["Accept"] = accept
    if token:
        headers["Authorization"] = f"Bearer {token}"
    return headers


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _write_config(url: str, token: str) -> None:
    try:
        from .state import state_dir

        path = state_dir() / "serve.json"
        path.write_text(
            json.dumps({"url": url, "token": token}, ensure_ascii=False),
            encoding="utf-8",
        )
    except Exception:
        pass


async def _probe(url: str) -> bool:
    try:
        async with httpx.AsyncClient(timeout=2.0) as client:
            resp = await client.get(url + "/health", headers=_headers())
        return resp.status_code < 500
    except Exception:
        return False


def _drop_nested_workspaces(workspaces: list[str]) -> list[str]:
    """Remove workspaces nested inside another (qwen serve rejects nesting).

    Keeps the parent, preserves the caller's order for the survivors, and
    dedupes by realpath. Primary-workspace ordering is preserved among the
    survivors; a nested child is dropped because its parent covers it.
    """
    ordered = list(dict.fromkeys(workspaces))
    real = {path: os.path.realpath(path) for path in ordered}
    kept: list[str] = []
    for path in sorted(ordered, key=lambda item: len(real[item])):
        resolved = real[path]
        if any(
            resolved == parent or resolved.startswith(parent + os.sep)
            for parent in kept
        ):
            continue
        kept.append(resolved)
    kept_set = set(kept)
    out: list[str] = []
    for path in ordered:
        resolved = real[path]
        if resolved in kept_set and resolved not in out:
            out.append(resolved)
    return out


_ensure_lock: asyncio.Lock | None = None


async def ensure_serve() -> str:
    """Return a reachable serve URL, starting one on a free port if needed.

    Single-flight: the web shell fires many requests on first load. Without
    this lock every concurrent caller spawned its own daemon; six daemons then
    raced for the Conversations runtime, and every one of them served 503 for
    ``/standalone`` because the ownership state could not be verified.
    """
    global _ensure_lock
    if _ensure_lock is None:
        _ensure_lock = asyncio.Lock()
    async with _ensure_lock:
        return await _ensure_serve_locked()


async def _ensure_serve_locked() -> str:
    previous = serve_url()
    url = previous
    if url and await _probe(url):
        return url
    if os.environ.get(SERVE_AUTOSTART_ENV, "1") == "0":
        return url
    port = _free_port()
    token = serve_token() or secrets.token_urlsafe(16)
    url = f"http://127.0.0.1:{port}"
    binary = os.environ.get("QWEN_CODE_BRIDGE_CLI", "").strip()
    if not binary:
        try:
            from .state import qwen_cli

            binary = qwen_cli()
        except Exception:
            binary = ""
    binary = binary or "qwen"
    if not (os.path.isabs(binary) and os.path.exists(binary)) and shutil.which(binary) is None:
        return ""
    try:
        from .state import state_dir

        workspaces: list[str] = []
        env_ws = os.environ.get(SERVE_WORKSPACE_ENV, "").strip()
        for chunk in env_ws.replace(":", ",").split(","):
            chunk = chunk.strip()
            if chunk and os.path.isdir(chunk) and chunk not in workspaces:
                workspaces.append(chunk)
        try:  # every run's cwd becomes a serve workspace, in recency order
            import json as _json
            import time as _time

            tasks = []
            for path in (state_dir() / "dispatch").glob("*.json"):
                try:
                    task = _json.loads(path.read_text(encoding="utf-8"))
                except Exception:
                    continue
                cwd = str(task.get("cwd") or "").strip()
                if cwd and os.path.isdir(cwd):
                    tasks.append((float(task.get("started_epoch") or 0), cwd))
            for _, cwd in sorted(tasks, reverse=True):
                if cwd not in workspaces:
                    workspaces.append(cwd)
        except Exception:
            pass
        # primary workspace first: the first entry drives the console's default
        # project view, so task cwds must precede the plugin's state dir.
        state = str(state_dir())
        if state in workspaces:
            workspaces.remove(state)
        workspaces.append(state)
        workspaces = _drop_nested_workspaces(workspaces)
        ws_args: list[str] = []
        for workspace in workspaces[:8]:
            ws_args += ["--workspace", workspace]
        env = dict(os.environ)
        try:
            from .state import cli_home, prepare_cli_home

            home = cli_home()
            if home:
                env["HOME"] = home
                prepared = prepare_cli_home(home)
                if prepared:
                    env["QWEN_HOME"] = prepared
                gitconfig = os.path.expanduser("~/.gitconfig")
                if os.path.exists(gitconfig):
                    env.setdefault("GIT_CONFIG_GLOBAL", gitconfig)
        except Exception:
            pass
        # NOTE: the daemon stays at the app's uid (root in this deployment).
        # The Conversations runtime's ownership check requires <home>/.qwen to
        # be owned by the same uid, and root can also read the session files
        # written by root-run dispatches; dropping to the home owner made
        # root-owned settings.json unreadable (EACCES).
        spawn_user = None
        spawn_group = None
        errlog = (state_dir() / "web-serve.log").open("ab")
        errlog.write(
            (
                f"--- spawn {binary} serve port={port} "
                f"home={env.get('HOME')} qwen_home={env.get('QWEN_HOME')} "
                f"cwd={os.getcwd()}\n"
            ).encode()
        )
        errlog.flush()
        proc = await asyncio.create_subprocess_exec(
            binary,
            "serve",
            "--hostname",
            "127.0.0.1",
            "--port",
            str(port),
            "--token",
            token,
            *ws_args,
            env=env,
            user=spawn_user,
            group=spawn_group,
            stdout=errlog,
            stderr=errlog,
        )
    except Exception as exc:
        try:
            from .state import state_dir

            with (state_dir() / "web-serve.log").open("a", encoding="utf-8") as fh:
                fh.write(
                    f"spawn failed: {type(exc).__name__}: {exc}; "
                    f"binary={binary} port={port} home={env.get('HOME')}\n"
                )
        except Exception:
            pass
        return previous
    try:
        from .state import state_dir

        with (state_dir() / "web-serve.log").open("a", encoding="utf-8") as fh:
            fh.write(
                f"spawned pid={proc.pid} port={port} binary={binary} "
                f"home={env.get('HOME')} qwen_home={env.get('QWEN_HOME')} "
                f"uid={spawn_user}\n"
            )
    except Exception:
        pass
    for _ in range(20):
        await asyncio.sleep(0.5)
        if await _probe(url):
            _write_config(url, token)
            os.environ[SERVE_URL_ENV] = url
            os.environ[SERVE_TOKEN_ENV] = token
            return url
        if proc.returncode is not None:
            break
    # Never poison serve.json with an unreachable port: stop the half-booted
    # daemon and keep the previous URL (possibly empty).
    try:
        proc.terminate()
    except Exception:
        pass
    return previous


@router.get("/web/status")
async def web_status() -> dict:
    """Probe ``qwen serve`` reachability (never raises)."""
    binary = os.environ.get("QWEN_CODE_BRIDGE_CLI", "").strip()
    if not binary:
        try:
            from .state import qwen_cli

            binary = qwen_cli()
        except Exception:
            binary = ""
    binary = binary or "qwen"
    if not (os.path.isabs(binary) and os.path.exists(binary)) and shutil.which(binary) is None:
        return {
            "reachable": False,
            "url": "",
            "error": "qwen-cli-missing",
            "hint": (
                "qwen CLI is not on PATH (container rebuild drops it). "
                "Run tools/qwen-setup/reinstall.sh or use the prebuilt "
                "qwenpaw:node image."
            ),
        }
    url = await ensure_serve()
    if not url:
        return {"reachable": False, "url": "", "error": "serve-url-unconfigured"}
    try:
        async with httpx.AsyncClient(timeout=3.0) as client:
            resp = await client.get(url + "/health", headers=_headers())
        return {
            "reachable": resp.status_code < 500,
            "url": url,
            "status_code": resp.status_code,
        }
    except Exception as exc:  # unreachable / timeout
        return {
            "reachable": False,
            "url": url,
            "error": f"{type(exc).__name__}: {str(exc)[:160]}",
        }


@router.api_route(
    "/web/{path:path}",
    methods=["GET", "POST", "PUT", "DELETE", "PATCH"],
)
async def web_proxy(path: str, request: Request) -> Response:
    """Reverse-proxy a request to the local ``qwen serve`` instance."""
    dist = web_dist()
    if dist is not None and request.method == "GET":
        rel = (path or "").strip("/")
        candidate = (dist / rel) if rel else (dist / "index.html")
        try:
            candidate.resolve().relative_to(dist.resolve())
        except Exception:
            candidate = None
        if candidate is not None and candidate.is_file():
            ctype = mimetypes.guess_type(str(candidate))[0] or "application/octet-stream"
            data = candidate.read_bytes()
            if ctype.startswith("text/html"):
                data = _inject_daemon_base(data)
            return Response(content=data, status_code=200, media_type=ctype)
    base = await ensure_serve()
    if not base:
        return JSONResponse(
            status_code=502,
            content={"error": "serve-url-unconfigured"},
        )
    path = _fix_workspace_path(path)
    url = f"{base}/{path}"
    body = await request.body()
    try:
        async with httpx.AsyncClient(timeout=60.0) as client:
            upstream = await client.request(
                request.method,
                url,
                content=body or None,
                headers=_headers(request),
                params=dict(request.query_params),
            )
    except Exception as exc:  # report as data, don't break the HTTP layer
        if dist is not None and "text/html" in request.headers.get("accept", "").lower():
            # Browser navigation with the daemon down: boot the SPA anyway.
            return Response(
                content=_spa_index(dist),
                status_code=200,
                media_type="text/html; charset=utf-8",
            )
        return JSONResponse(
            status_code=502,
            content={
                "error": "serve-unreachable",
                "url": url,
                "detail": f"{type(exc).__name__}: {str(exc)[:160]}",
            },
        )
    try:  # diagnostics: last 200 proxied calls
        from .state import state_dir

        log = state_dir() / "web-proxy.log"
        with log.open("a", encoding="utf-8") as fh:
            fh.write(f"{request.method} /{path} -> {upstream.status_code}\n")
        if log.stat().st_size > 200_000:
            lines = log.read_text(encoding="utf-8", errors="ignore").splitlines()[-500:]
            log.write_text("\n".join(lines) + "\n", encoding="utf-8")
    except Exception:
        pass
    media_type = upstream.headers.get("content-type", "application/octet-stream")
    content = upstream.content
    wants_html = "text/html" in request.headers.get("accept", "").lower()
    if wants_html and upstream.status_code >= 400:
        # Browser navigation to a client-side route (/session/<id>, /threads, …):
        # the daemon answers 404 JSON, the SPA must boot instead. Works with or
        # without a rebuilt shell (without it, the daemon's own index is reused).
        if dist is not None:
            return Response(
                content=_spa_index(dist),
                status_code=200,
                media_type="text/html; charset=utf-8",
                headers={"Cache-Control": "no-store"},
            )
        try:
            async with httpx.AsyncClient(timeout=10.0) as client:
                index = await client.get(base + "/", headers=_headers(request))
            if index.status_code < 400 and index.content:
                return Response(
                    content=_rewrite_console_html(index.content),
                    status_code=200,
                    media_type="text/html; charset=utf-8",
                    headers={"Cache-Control": "no-store"},
                )
        except Exception:
            pass
    if "text/html" in media_type.lower():
        if dist is not None:
            content = _spa_index(dist)
            media_type = "text/html; charset=utf-8"
        else:
            content = _rewrite_console_html(content)
    return Response(
        content=content,
        status_code=upstream.status_code,
        media_type=media_type,
    )



_WS_ROUTE_REGISTERED = False
_ws_auth_missing_warned = False


def ensure_ws_route(app) -> None:
    """Append the WebSocket bridge route to the root app once (lazy)."""
    global _WS_ROUTE_REGISTERED
    if _WS_ROUTE_REGISTERED or app is None:
        return
    from starlette.routing import WebSocketRoute

    router_obj = getattr(app, "router", None)
    if router_obj is None:
        return
    router_obj.routes.append(
        WebSocketRoute("/api/qwen-code-bridge/web/{path:path}", web_ws)
    )
    _WS_ROUTE_REGISTERED = True


def _ws_caller_token(websocket: WebSocket) -> str:
    """Resolve the caller's token: query ``token`` first, else Bearer.

    Mirrors ``qwenpaw.app.auth.AuthMiddleware._extract_token``: the
    supported WebSocket credential channel is the query token (the console
    stores its login token in ``localStorage["qwenpaw_auth_token"]`` and the
    injected shell shim appends it); the ``Authorization: Bearer …`` header
    covers non-browser clients.
    """
    token = (websocket.query_params.get("token") or "").strip()
    if not token:
        auth_header = websocket.headers.get("authorization", "")
        if auth_header[:7].lower() == "bearer ":
            token = auth_header[7:].strip()
    return token


def _load_sdk_auth():
    """The SDK auth module (``qwenpaw.app.auth``), or None when unavailable."""
    try:
        from qwenpaw.app import auth as sdk_auth

        return sdk_auth
    except Exception:
        return None


def _ws_caller_authenticated(websocket: WebSocket) -> bool:
    """Authenticate the WS caller against the QwenPaw login (before accept).

    Decision table:

    - SDK auth module available + login active (``is_auth_enabled()`` and
      ``has_registered_users()``): the caller token (query ``token`` or
      ``Authorization: Bearer …``) must pass ``verify_token``; a missing or
      invalid token is a denial — close code 4401 sent **before** the
      handshake is accepted, the exact shape ``RuntimeBoundaryMiddleware``
      uses, so the upstream ``qwen serve`` is never contacted.
    - SDK auth module available + login off / no registered user: accept
      (unchanged behaviour).
    - SDK auth module unavailable: one logged warning; accept (documented
      compatibility) unless ``QWEN_CODE_BRIDGE_REQUIRE_WS_AUTH=1`` forces a
      fail-closed denial (4401).

    ``allow_no_auth_hosts`` is intentionally NOT replicated for WebSocket:
    a login-protected runtime requires a token on the WS path.
    """
    global _ws_auth_missing_warned
    sdk_auth = _load_sdk_auth()
    if sdk_auth is None:
        if not _ws_auth_missing_warned:
            logger.warning(
                "qwenpaw.app.auth unavailable; bridge WebSocket login auth "
                "is not enforced unless %s=1",
                REQUIRE_WS_AUTH_ENV,
            )
            _ws_auth_missing_warned = True
        if os.environ.get(REQUIRE_WS_AUTH_ENV, "").strip().lower() in (
            "1",
            "true",
            "yes",
        ):
            return False
        return True
    if not (sdk_auth.is_auth_enabled() and sdk_auth.has_registered_users()):
        return True
    token = _ws_caller_token(websocket)
    if not token:
        return False
    return sdk_auth.verify_token(token) is not None


@router.websocket("/web/{path:path}")
async def web_ws(websocket: WebSocket, path: str) -> None:
    """Bridge the shell's WebSocket streams (live-state, terminals, …).

    Caller authentication happens BEFORE ``accept()``: when the QwenPaw
    login is active, the caller token (``?token=`` — the console's stored
    ``qwenpaw_auth_token`` — or ``Authorization: Bearer …``) must verify via
    the SDK's ``verify_token``; a missing or invalid token closes the socket
    with code 4401 (the shape ``RuntimeBoundaryMiddleware`` uses) and the
    upstream is never contacted. The backend ``serve_token()`` stays
    server-side: it only ever appears in the upstream ``Authorization``
    header, never in a response, URL or log. ``allow_no_auth_hosts`` is not
    replicated for WS: a login-protected runtime requires a token here.
    Without the SDK auth module the behaviour is unchanged unless
    ``QWEN_CODE_BRIDGE_REQUIRE_WS_AUTH=1`` (fail-closed 4401).
    """
    import asyncio

    import websockets

    if not _ws_caller_authenticated(websocket):
        try:
            await websocket.close(code=WS_AUTH_DENY_CODE)
        except Exception:
            pass
        return
    await websocket.accept()
    base = serve_url()
    if not base:
        await websocket.close(code=1011)
        return
    fixed = _fix_workspace_path(path or "")
    url = base.replace("http://", "ws://").replace("https://", "wss://")
    target = f"{url.rstrip('/')}/{fixed}"
    token = serve_token()
    headers = {"Authorization": f"Bearer {token}"} if token else {}
    try:
        async with websockets.connect(
            target,
            additional_headers=headers,
            max_size=None,
            open_timeout=10,
            close_timeout=2,
        ) as upstream:
            async def client_to_upstream() -> None:
                while True:
                    message = await websocket.receive()
                    if message.get("type") == "websocket.disconnect":
                        return
                    if message.get("text") is not None:
                        await upstream.send(message["text"])
                    elif message.get("bytes") is not None:
                        await upstream.send(message["bytes"])

            async def upstream_to_client() -> None:
                async for message in upstream:
                    if isinstance(message, bytes):
                        await websocket.send_bytes(message)
                    else:
                        await websocket.send_text(message)

            tasks = [
                asyncio.create_task(client_to_upstream()),
                asyncio.create_task(upstream_to_client()),
            ]
            done, pending = await asyncio.wait(
                tasks,
                return_when=asyncio.FIRST_COMPLETED,
            )
            for task in pending:
                task.cancel()
    except Exception:
        try:
            await websocket.close(code=1011)
        except Exception:
            pass
        return
    try:
        await websocket.close()
    except Exception:
        pass


__all__ = ["router", "serve_url"]
