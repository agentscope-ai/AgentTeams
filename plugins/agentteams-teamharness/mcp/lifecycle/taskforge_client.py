"""TaskForge HTTP client (1.2.7 reservation, issue #1510).

Generic read/write client for the TaskForge service (TrackForge, #1508):
a blocking ``get``/``post`` pair plus endpoint path constants — no
business methods. This reservation wires the dependency slot without
touching the existing file-backed taskflow, so the module keeps the
defensive line: it never reads or writes local files, there is no
meta.json fallback, no automatic retry, and a 503 is never swallowed
into a success.

Error contract:

* HTTP 503                     -> TaskForgeUnavailable
* HTTP 409                     -> TaskForgeConflict (carries the server
                                  ``code`` when the JSON body has one)
* any other HTTP status >= 400 -> TaskForgeError (carries the status and
                                  the first 512 bytes of the body)
* connection refused / timeout -> TaskForgeUnavailable

Pure stdlib (urllib). The endpoint constants and request/response shapes
are placeholders until TaskForge 0.1.0 publishes its schemas in
``core/schemas`` (see ``contracts/taskforge/README.md``); the constants
track this client's intended path layout.
"""
from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any

__all__ = [
    "EVENTS",
    "LOOPS",
    "MAX_ERROR_BODY_BYTES",
    "PROJECTS",
    "TASKS",
    "TaskForgeClient",
    "TaskForgeConflict",
    "TaskForgeError",
    "TaskForgeUnavailable",
    "task_action",
    "task_sub",
]

# Endpoint path constants for the TaskForge REST surface. Provisional
# until the 0.1.0 core/schemas mirror lands (contracts/taskforge/).
PROJECTS = "/api/v1/projects"
TASKS = "/api/v1/tasks"
LOOPS = "/api/v1/loops"
EVENTS = "/api/v1/events"

# Diagnostic cap for response bodies carried on TaskForgeError.
MAX_ERROR_BODY_BYTES = 512


class TaskForgeError(Exception):
    """Base error for TaskForge client failures.

    ``status`` is the HTTP status code, or None for transport-level
    failures (connection refused, timeout). ``body`` carries at most the
    first ``MAX_ERROR_BODY_BYTES`` bytes of the response body.
    """

    def __init__(
        self,
        message: str,
        *,
        status: int | None = None,
        body: bytes = b"",
    ) -> None:
        super().__init__(message)
        self.status = status
        self.body = body


class TaskForgeUnavailable(TaskForgeError):
    """TaskForge is unreachable or not ready: HTTP 503, a refused
    connection, or a timed-out request. Callers must surface this; it is
    never a success."""


class TaskForgeConflict(TaskForgeError):
    """Conditional-write conflict (HTTP 409). ``code`` carries the
    server's conflict code from the JSON body when present, else None."""

    def __init__(
        self,
        message: str,
        *,
        status: int | None = None,
        body: bytes = b"",
        code: str | None = None,
    ) -> None:
        super().__init__(message, status=status, body=body)
        self.code = code


def task_action(task_id: str, action: str) -> str:
    """Path for a task action endpoint: /api/v1/tasks/<id>/<action>."""
    return f"{TASKS}/{task_id}/{action}"


def task_sub(task_id: str, sub: str) -> str:
    """Path for a task sub-resource endpoint: /api/v1/tasks/<id>/<sub>."""
    return f"{TASKS}/{task_id}/{sub}"


def _truncate(body: bytes) -> bytes:
    return body[:MAX_ERROR_BODY_BYTES]


def _conflict_code(body: bytes) -> str | None:
    """Extract the string conflict code from a (possibly truncated) JSON
    409 body: the ``error.code`` envelope field first, then a top-level
    ``code``; None when the body is not a JSON object carrying either."""
    try:
        data = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return None
    if not isinstance(data, dict):
        return None
    error = data.get("error")
    if isinstance(error, dict):
        code = error.get("code")
        if isinstance(code, str) and code:
            return code
    code = data.get("code")
    if isinstance(code, str) and code:
        return code
    return None


def _read_error_body(exc: urllib.error.HTTPError) -> bytes:
    try:
        return _truncate(exc.read())
    except (OSError, ValueError):
        return b""


def _decode_object(body: bytes, url: str) -> dict[str, Any]:
    """Decode a 2xx response body into the object this client promises.

    An empty body (e.g. 204) yields {}; a non-object JSON body is a
    contract violation and raises TaskForgeError.
    """
    if not body:
        return {}
    try:
        data = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise TaskForgeError(
            f"TaskForge returned a non-JSON body for {url}"
        ) from exc
    if not isinstance(data, dict):
        raise TaskForgeError(
            f"TaskForge returned a non-object JSON body for {url}"
        )
    return data


class TaskForgeClient:
    """Minimal blocking client for the TaskForge service.

    Only the generic ``get``/``post`` are provided (the endpoint path
    constants live at module level); business-level helpers deliberately
    do not. Headers are pass-through: ``If-Match`` and
    ``Idempotency-Key`` are sent only when the caller provides them, and
    ``X-Request-Id`` is caller-supplied when given, else a fresh uuid4.
    Each call is a single attempt — there is no retry and no local
    fallback.
    """

    def __init__(self, base_url: str, token: str, timeout: float = 10.0) -> None:
        self._base = base_url.rstrip("/")
        self._token = token
        self._timeout = timeout

    def get(
        self,
        path: str,
        params: dict[str, Any] | None = None,
        *,
        request_id: str | None = None,
    ) -> dict[str, Any]:
        """GET <base><path> (query string from ``params``) and return the
        JSON object response body ({} for an empty body)."""
        url = self._url(path, params)
        with self._open("GET", url, data=None, request_id=request_id) as resp:
            return _decode_object(resp.read(), url)

    def post(
        self,
        path: str,
        payload: dict[str, Any] | None = None,
        *,
        if_match: str | None = None,
        idempotency_key: str | None = None,
        request_id: str | None = None,
    ) -> dict[str, Any]:
        """POST <base><path> with the JSON object body ``payload`` (no
        body when None) and return the JSON object response body.

        ``if_match`` / ``idempotency_key`` are passed through to the
        If-Match / Idempotency-Key headers when provided; this client
        never generates them itself.
        """
        url = self._url(path)
        data = None if payload is None else json.dumps(payload).encode("utf-8")
        with self._open(
            "POST",
            url,
            data=data,
            if_match=if_match,
            idempotency_key=idempotency_key,
            request_id=request_id,
        ) as resp:
            return _decode_object(resp.read(), url)

    # -- internals -------------------------------------------------------

    def _url(self, path: str, params: dict[str, Any] | None = None) -> str:
        if not path.startswith("/"):
            path = "/" + path
        url = self._base + path
        if params:
            url = f"{url}?{urllib.parse.urlencode(params, doseq=True)}"
        return url

    def _open(
        self,
        method: str,
        url: str,
        *,
        data: bytes | None = None,
        if_match: str | None = None,
        idempotency_key: str | None = None,
        request_id: str | None = None,
    ) -> Any:
        headers = {
            "Authorization": f"Bearer {self._token}",
            "Content-Type": "application/json",
            "X-Request-Id": request_id or str(uuid.uuid4()),
        }
        if if_match is not None:
            headers["If-Match"] = if_match
        if idempotency_key is not None:
            headers["Idempotency-Key"] = idempotency_key
        request = urllib.request.Request(
            url, data=data, headers=headers, method=method
        )
        try:
            return urllib.request.urlopen(request, timeout=self._timeout)
        except urllib.error.HTTPError as exc:
            # HTTPError subclasses URLError, so it must be mapped first.
            body = _read_error_body(exc)
            status = exc.code
            if status == 503:
                raise TaskForgeUnavailable(
                    f"TaskForge unavailable (503) for {method} {url}",
                    status=status,
                    body=body,
                ) from exc
            if status == 409:
                raise TaskForgeConflict(
                    f"TaskForge conflict (409) for {method} {url}",
                    status=status,
                    body=body,
                    code=_conflict_code(body),
                ) from exc
            raise TaskForgeError(
                f"TaskForge returned HTTP {status} for {method} {url}",
                status=status,
                body=body,
            ) from exc
        except urllib.error.URLError as exc:
            raise TaskForgeUnavailable(
                f"TaskForge unreachable for {method} {url}: {exc.reason}"
            ) from exc
        except TimeoutError as exc:
            raise TaskForgeUnavailable(
                f"TaskForge request timed out for {method} {url}: {exc}"
            ) from exc
