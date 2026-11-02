# W2 review gate and protocol guards (issue #1451 W2): the review_task
# record, the accept_task_result acceptance gates, and the structured
# rejection type shared by the taskflow / projectflow dispatch layers.
#
# Round 6 (issue #1467) — trusted review identity: the account part of
# a transition actor is the trusted runtime member identity (the
# operator-set AGENTTEAMS_WORKER_NAME env / runtime config member block:
# member.name -> member.runtimeName -> matrixUserId localpart), never a
# tool payload field. The payload's account_id is provenance only: it
# is used when no trusted identity exists (local / test runs) and can
# never override one — two different members must never collapse into
# one identity. The self-review comparison therefore normalizes the
# accounts and treats "default" / "unknown" / empty as UNKNOWN: an
# unknown (legacy) submitter account is not a self-review match, while
# a concrete trusted identity on both sides that matches — or a full
# reviewer == submitted_by — is still self_review_forbidden.
#
# Round 7 (issue #1467) — identity provenance: round 6 encoded
# "unknown" as the VALUE "default", so a real trusted member literally
# named "default" was indistinguishable from the legacy sentinel. The
# record now carries where its account came from — submitted_by_source
# on the task meta, reviewer_source on the review object: "runtime" for
# the trusted runtime member identity, "payload" for the account_id
# fallback. The self-review rule compares identity + provenance: when
# BOTH sides are runtime-sourced the normalized accounts compare
# strictly, "default" included (a trusted name is an identity, never
# the sentinel); a legacy record without the marker — or a
# payload-sourced side — keeps the unknown-sentinel rule for
# compatibility ("default" / "unknown" / empty never match, concrete
# accounts still compare).
#
# Round 8 (issue #1467) — the trusted decider: the r1 approval gate
# reads the decider from the server-written resolvedActor marker ONLY.
# resolvedBy is display provenance written from the caller's payload
# and must never influence the decision: a record without the
# server-written marker is not an approval (fail closed), whatever its
# display fields claim. The self-approval comparison canonicalizes one
# way on both sides (the shared _canonical_account): the MXID
# localpart and the runtimeName of the same member are the same
# identity, so the display spelling can no longer decide.
from __future__ import annotations

from typing import Any

from mcp.common import refs
from mcp.common.config import _protocol_enforce_enabled, _runtime_team_name
from mcp.common.constants import DECISION_CLASS_IDS, DEFAULT_DECISION_CLASS
from mcp.common.identity import _canonical_account
from mcp.common.io import _utc_timestamp
from mcp.common.payload import _first_text, _safe_id
from mcp.artifact.sync import (
    _TASK_COMMIT_ATTEMPTS,
    _load_authoritative_task,
    _load_authoritative_task_versioned,
    _remote_task_meta_etag,
    _task_meta_commit_landed,
)
from .transitions import (
    _reject_task_transition,
    _require_task_mutable,
    _transition_actor,
    _transition_actor_source,
)

# Call-time lookups (common.refs): these names resolve against the bound
# mcp/server.py namespace, so test monkeypatches of server attributes stay
# visible to this module.
_write_task = refs.proxy("_write_task")
_sync_task = refs.proxy("_sync_task")
# The trusted runtime role (server.py): it reads the operator-set
# environment / runtime config, never the tool payload, so a caller cannot
# spoof it. Call-time proxy so tests can monkeypatch it.
_runtime_role = refs.proxy("_runtime_role")

# Resolve channels that carry a human decision: a reply or a command in
# the IM room, or the dashboard. "tool" and "api" are agent/automation
# channels — they may close an attention loop, but they can never
# authorize an approval (#1467 round 3: verifiable decision authority).
_HUMAN_RESOLVE_CHANNELS = {"im_reply", "im_command", "dashboard"}


class ProtocolGuardError(Exception):
    """Structured W2 protocol rejection: a stable error_code plus the
    stable message. Deliberately NOT a ValueError subclass so the
    dispatch layers return the {ok, error_code, message} surface instead
    of a bare error string."""

    def __init__(self, error_code: str, message: str) -> None:
        super().__init__(message)
        self.error_code = error_code
        self.message = message


def _decision_class_of(task: dict[str, Any]) -> str:
    """Read the task's decision class; an absent field reads as the r4
    default (compat rule: pre-W2 tasks have no decision class)."""
    return _first_text(task.get("decision_class"), task.get("decisionClass")) or DEFAULT_DECISION_CLASS


def _decider_identity(decider: str) -> tuple[str, str]:
    """Parse a recorded decider into (role, account): the leading role
    prefix up to the first colon, plus the account behind it (colons
    preserved — Matrix identities @user:server). A decider without a
    colon is a bare claim with no identity and parses to (text, "")."""
    text = str(decider or "").strip()
    role, sep, account = text.partition(":")
    if not sep or not account:
        return (text, "")
    return (role, account)


def _has_approval_record(task: dict[str, Any], submitter_account: str = "") -> bool:
    """An approval record is a RESOLVED attention record of kind
    'approval' that carries the trusted-authority marker: the
    server-written resolvedRole must be "human" (only the authorized
    close path writes it — a payload-supplied resolvedRole is never
    copied), the close went through a human channel (im_reply /
    im_command / dashboard — never tool or api), and the server-written
    decider (resolvedActor) carries an account that, when the
    submitter's account is known, differs from it: self-approval never
    counts. The role is read from the marker only — never inferred from
    the decider strings (#1467 round 4: the marker is the authority;
    record presence alone is not an approval).

    The decider is the server-written marker ONLY (#1467 round 8):
    resolvedBy is display provenance written from the caller's payload
    and must never influence the decision. A record without the
    server-written resolvedActor marker is not an approval — fail
    closed, whatever its display fields claim. The identity comparison
    canonicalizes one way on both sides (the shared _canonical_account):
    the MXID localpart and the runtimeName of the same member are the
    same identity, so the display spelling can no longer decide whether
    the decider is the submitter."""
    attention = task.get("attention")
    if not isinstance(attention, list):
        return False
    for record in attention:
        if not isinstance(record, dict):
            continue
        if str(record.get("kind") or "") != "approval":
            continue
        if not record.get("resolved"):
            continue
        if record.get("resolvedRole") != "human":
            continue
        if str(record.get("resolvedVia") or "") not in _HUMAN_RESOLVE_CHANNELS:
            continue
        # The decider is the server-written marker only (#1467 round
        # 8): resolvedBy is display provenance and never decides — a
        # record without the server marker is not an approval (fail
        # closed).
        _decider_role, account = _decider_identity(
            str(record.get("resolvedActor") or "")
        )
        if not account:
            continue
        if submitter_account and _canonical_account(
            account
        ) == _canonical_account(submitter_account):
            continue
        return True
    return False


def _approval_authorized(
    actor: str,
    resolve_via: str,
    resolved_by: str = "",
    resolve_evidence: Any = None,
) -> bool:
    """Only the trusted runtime role can authorize an approval close.

    Every field a tool caller controls (role, account, channel, decider,
    evidence) is provenance, never authority: with no trusted role there
    is no verified human identity, so the close fails closed. The
    parameters are accepted for call-site compatibility and are
    deliberately ignored by the decision. A human integration
    (dashboard / API bridge) must run with the runtime role 'human'
    (AGENTTEAMS_AGENT_ROLE=human or member.role in the runtime config);
    that value comes from the operator, not the payload (#1467 round 4:
    trusted authority only)."""
    return _runtime_role() == "human"


def _guard_delegate_decision_class(payload: dict[str, Any]) -> str:
    """W2: with the protocol switch on, delegate_task requires a known
    decision class (r1..r6, contracts/decision-classes.json). Returns the
    class to persist on the task meta; with the switch off the field is
    ignored entirely (pre-W2 behaviour, byte-for-byte)."""
    if not _protocol_enforce_enabled():
        return ""
    decision_class = _first_text(payload.get("decision_class"), payload.get("decisionClass"))
    if not decision_class:
        raise ProtocolGuardError(
            "missing_decision_class",
            "decision_class is required (r1..r6, see contracts/decision-classes.json)",
        )
    if decision_class not in DECISION_CLASS_IDS:
        raise ProtocolGuardError(
            "unknown_decision_class",
            "decision_class must be one of r1, r2, r3, r4, r5, r6 (see contracts/decision-classes.json)",
        )
    return decision_class


def _protocol_guard_for_accept(task: dict[str, Any], accepted: bool) -> None:
    """W2 acceptance gates for projectflow accept_task_result (skipped
    entirely with the protocol switch off). The gate applies to a
    POSITIVE acceptance only: a rejection / return-for-revision
    (accepted=False) must always be possible, so it returns immediately
    for those. With accepted=True:
    - a missing or malformed review record blocks the acceptance
      (review_missing);
    - a review record with a non-passing verdict blocks the acceptance
      (review_not_passed);
    - an r1 task result (task_result_acceptance class; an absent
      decision_class reads as r4) cannot be decided without a resolved
      approval record carrying the trusted-authority marker
      (approval_required): the record must have been closed by the
      authorized path (server-written resolvedRole "human") through a
      human channel by a decider — the server-written resolvedActor
      only; the display field resolvedBy never decides (#1467 round 8)
      — whose account differs from the task's assignee (self-approval
      never counts; the role is never inferred from the decider
      strings, and record presence alone is not an approval).
    """
    if not _protocol_enforce_enabled():
        return
    if not accepted:
        return
    review = task.get("review")
    verdict = ""
    if isinstance(review, dict):
        verdict = str(review.get("verdict") or "").strip().lower()
    if verdict not in {"pass", "fail"}:
        raise ProtocolGuardError(
            "review_missing",
            "accept_task_result requires a review record before accepting a result "
            "(missing or malformed)",
        )
    if verdict != "pass":
        raise ProtocolGuardError(
            "review_not_passed",
            f"accept_task_result requires a passing review verdict "
            f"(current verdict: '{review.get('verdict')}')",
        )
    if _decision_class_of(task) == "r1" and not _has_approval_record(
        task, submitter_account=_account_of(task.get("assigned_to") or "")
    ):
        raise ProtocolGuardError(
            "approval_required",
            "r1 task result requires a resolved approval record before it can be decided",
        )


# The role prefix a transition actor can carry (see _transition_actor:
# the canonical _normalize_role output, plus its "unknown" fallback).
# The vocabulary is the union of this tree's role checks — taskflow
# (worker / remote-member / leader), review_task (critic / leader), the
# trusted runtime role (human — a human integration's runtime role
# surfaces in the actor, #1467 round 4) and constants (worker /
# remote-member). Accounts may themselves contain a colon (Matrix
# identities @user:server), so only a leading known role prefix may be
# stripped — never the last colon segment.
_ACTOR_ROLE_PREFIXES = {
    "leader",
    "worker",
    "critic",
    "remote-member",
    "human",
    "unknown",
}


def _account_of(actor: str) -> str:
    """The account identity behind a ``role:account`` actor string.

    Strips only a leading known role prefix; everything after the first
    colon is the account, including any further colons. An actor whose
    prefix is not a known role is returned unchanged."""
    text = str(actor)
    prefix, sep, rest = text.partition(":")
    if sep and rest and prefix in _ACTOR_ROLE_PREFIXES:
        return rest
    return text


# Accounts that mean "the caller did not (or could not) identify
# itself": the shared 'default' fallback, an explicit 'unknown', or
# empty. Records written by older builds (pre trusted-identity, every
# member collapsed to 'default') carry such accounts; they are unknown,
# not an identity (#1467 round 6: unknown is unknown — no false
# positive — and a concrete identity can never collide with one).
#
# UNKNOWN-SENTINEL VALUES ONLY (#1467 round 7): this set is the
# vocabulary of records WITHOUT runtime provenance. A runtime-sourced
# account is an identity and is never mapped through it — a trusted
# member literally named 'default' is compared as 'default'.
_UNKNOWN_ACCOUNTS = {"", "default", "unknown"}


def _known_account_of(actor: str) -> str:
    """The UNKNOWN-SENTINEL rule (not the identity rule, #1467
    round 7): the normalized account behind a ``role:account`` actor
    string, or '' when the account is the unknown sentinel (empty /
    'default' / 'unknown').

    It applies only to the side of a comparison that has no runtime
    provenance — a legacy record without the submitted_by_source /
    reviewer_source marker, or a payload-sourced side. A
    runtime-sourced account is compared strictly by the self-review
    rule and never passes through here, so a trusted member literally
    named 'default' is never discarded as a sentinel.

    Normalized (strip, lowercase) so only real identity matches count;
    an unknown account returns '' and can never match — a legacy
    'worker:default' submission must not block review by a distinct
    trusted member, and two unknown accounts must not read as the same
    identity."""
    account = _account_of(actor).strip().lower()
    return "" if account in _UNKNOWN_ACCOUNTS else account


class _TaskCommitConflict(RuntimeError):
    """The task base moved between the authoritative read and the commit."""


def _review_task_action(
    arguments: dict[str, Any],
    payload: dict[str, Any],
    role: str,
) -> dict[str, Any]:
    """review_task with a bounded version-guarded commit (#1467 round 17).

    A peer commit (a freeze with its approval record and
    decision_status_history) can land after the authoritative pull and
    before the whole-directory mirror. Each attempt reloads the fresh base
    and re-applies the review, so the peer's record survives; an exhausted
    retry fails closed instead of overwriting.
    """
    last_error: Exception | None = None
    for _attempt in range(1, _TASK_COMMIT_ATTEMPTS + 1):
        try:
            return _review_task_attempt(arguments, payload, role)
        except _TaskCommitConflict as exc:
            last_error = exc
    raise ValueError(f"review_task: {last_error}; reload and retry")


def _review_task_attempt(
    arguments: dict[str, Any],
    payload: dict[str, Any],
    role: str,
) -> dict[str, Any]:
    """W2: review_task — the critic/leader review of a submitted task
    result. Only 'submitted' tasks are reviewable; the reviewer must
    differ from the submitting identity (self_review_forbidden). The
    record shape follows the design doc §3.3 review object.

    The self-review rule compares identity + provenance (#1467
    round 7): the reviewer's account is the transition actor's — the
    trusted runtime member identity when one exists, the payload's
    account_id otherwise — and its source is recorded on the review
    object (reviewer_source: "runtime" / "payload"); the submitter's
    source is the record's submitted_by_source. The exact
    reviewer == submitted_by match always holds. When both sides are
    runtime-sourced the normalized accounts compare strictly,
    "default" included — a trusted name is never treated as unknown
    just because it equals a sentinel. A legacy record without
    provenance (or a payload-sourced side) is treated as unknown for
    compatibility: "default" / "unknown" / empty never match, concrete
    accounts still compare."""
    if role not in {"critic", "leader"}:
        raise ValueError("review_task requires critic or leader role")
    task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
    # W2 round 9 (#1467): review is a cross-actor write and _sync_task
    # pushes the WHOLE task directory — load the authoritative task
    # (pull first, fail closed on failure) so a concurrently persisted
    # decision_status=frozen, its approval records and the
    # decision_status_history survive the review write.
    task, base_etag = _load_authoritative_task_versioned(arguments, task_id)
    _require_task_mutable(arguments, task, task_id, "review_task")
    status = str(task.get("status") or "")
    if status != "submitted":
        _reject_task_transition(
            status,
            status,
            "review_task",
            (
                f"illegal_transition: review_task: task is '{status}'; "
                "wait for submit_task"
            ),
            arguments=arguments,
            task_id=task_id,
        )
    verdict = str(payload.get("verdict") or "").strip().lower()
    if verdict not in {"pass", "fail"}:
        raise ValueError("verdict must be one of pass, fail")
    note = str(payload.get("note") or "").strip()
    scorecard = payload.get("scorecard")
    if scorecard is not None and not isinstance(scorecard, dict):
        raise ValueError("scorecard must be an object")
    reviewer = _transition_actor(arguments)
    submitted_by = _first_text(task.get("submitted_by"), task.get("submittedBy"))
    # Self-review compares identity + provenance (#1467 round 7). The
    # exact actor-string match always holds. When BOTH sides are
    # runtime-sourced — the record carries submitted_by_source
    # "runtime" and the reviewer has a trusted runtime identity — the
    # normalized accounts compare strictly, "default" included: a
    # trusted member literally named "default" is an identity, never
    # the legacy sentinel. Otherwise — a legacy record without the
    # provenance marker, or a payload-sourced side — the
    # unknown-sentinel rule applies for compatibility: "default" /
    # "unknown" / empty never match, concrete accounts still compare
    # (#1467 round 6).
    reviewer_account = _account_of(reviewer).strip().lower()
    submitter_account = _account_of(submitted_by).strip().lower()
    submitter_source = str(task.get("submitted_by_source") or "")
    reviewer_source = _transition_actor_source(arguments)
    both_runtime = submitter_source == "runtime" and reviewer_source == "runtime"
    if submitted_by and (
        reviewer == submitted_by
        or (both_runtime and reviewer_account == submitter_account)
        or (
            not both_runtime
            and _known_account_of(reviewer)
            and _known_account_of(reviewer) == _known_account_of(submitted_by)
        )
    ):
        raise ProtocolGuardError(
            "self_review_forbidden",
            "reviewer must differ from submitted_by",
        )
    review: dict[str, Any] = {
        "verdict": verdict,
        "reviewer": reviewer,
        "reviewer_source": reviewer_source,
        "reviewer_team": _runtime_team_name(),
        "reviewed_at": _utc_timestamp(),
        "note": note,
    }
    if scorecard is not None:
        review["scorecard"] = scorecard
    task["review"] = review
    # Guard the commit against a base change during the window: mirroring a
    # stale directory would erase a peer's freeze, approval and history.
    current_etag, version_error = _remote_task_meta_etag(arguments, task_id)
    if version_error:
        raise ValueError(
            "review_task: cannot verify the authoritative task version before "
            "the commit; refusing to write (retry)"
        )
    if base_etag is not None and current_etag != base_etag:
        raise _TaskCommitConflict(
            "a peer committed a newer task version during the commit window"
        )
    _write_task(arguments, task)
    synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
    if not synced:
        raise ValueError("review_task: task sync failed (retry)")
    if not _task_meta_commit_landed(arguments, task_id, task):
        raise _TaskCommitConflict(
            "a peer overwrote the task during the commit window"
        )
    return {
        "ok": True,
        "tool": "taskflow",
        "action": "review_task",
        "task": task,
        "review": review,
        "synced": synced,
    }
