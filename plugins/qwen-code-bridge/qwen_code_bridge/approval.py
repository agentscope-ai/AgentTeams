# -*- coding: utf-8 -*-
"""Worker-side approval mapping for headless coding-CLI runs.

The CLI (``qwen --input-format stream-json``) asks the host to decide
every would-ask tool call through the control protocol::

    {"type":"control_request","request_id":"...",
     "request":{"subtype":"can_use_tool","tool_name":"...",
                "tool_use_id":"...","input":{...},
                "permission_suggestions":null,"blocked_path":null}}

and waits for::

    {"type":"control_response",
     "response":{"subtype":"success","request_id":"...",
                 "response":{"behavior":"allow","updatedInput":{...}}}}
    # or {"behavior":"deny","message":"...","interrupt":false}

This module is the **deterministic policy** the worker uses to answer on
the human's behalf ("worker 代审批").  No LLM in the loop: the same
tool call always maps to the same decision, every decision is written to
a receipt, and anything unrecognised fails closed (deny).

Shell policy (fail closed, #1411):

1. Lexical scan — any shell metacharacter **outside** quotes/escapes
   (``; & | < >`` backtick ``$( `` ``${ `` newline ``(){} ``) denies the
   command outright (``exec-compound``).  Inside single quotes such
   characters are data, but double quotes still perform command
   substitution — backtick and ``$( ``/``${ `` are denied inside double
   quotes too (``git commit -m "fix: a;b"`` is fine).
2. ``shlex.split`` must succeed (``exec-parse`` otherwise) and
   ``argv[0]`` must be a whitelisted program.
3. ``cwd`` (when provided) must exist and resolve inside the run roots
   (``outside-root-cwd``).
4. Path-like argv (absolute, ``./``, ``../``, ``~/``, contains ``/``)
   must ``realpath`` into the roots — symlinks included
   (``outside-root``).  A bare name (no path separator) is checked the
   same way when it exists in the run dir — the shell resolves bare
   names against the cwd — while a missing bare name is a pattern or
   argument and is left to the command itself to fail.  Bare flag
   tokens (leading ``-`` without ``=``) are never path operands, but
   the value part of an equals-form flag (``--files0-from=<file>``) is
   data the program actually opens and is guarded like a bare
   operand.  The end-of-options delimiter (``--``) ends flag parsing:
   every token after it is a literal operand — a leading ``-`` or an
   embedded ``=`` is part of the file name, never flag syntax — and
   is checked the same way.  Secret-ish operands are
   refused everywhere.  Programs whose arguments are a program (``sed``)
   are denied outright: their scripts can execute commands
   (``e``/``s///e``) or write files (``w``/``W``/``s///w``, ``-f``),
   which no flag check can rule out.
5. Unconstrained interpreters (``sh``/``bash``/``python*``/``node`` …)
   and destructive utilities are hard-denied (``exec-denied``);
   anything else escalates (deny by default, overridable via
   ``escalate_behavior``).  Hard denies are never overridable.
"""

from __future__ import annotations

import json
import os
import re
import shlex
from pathlib import Path
from typing import Any, Iterable

#: Risk classes.  ``allow`` = auto-approved, ``deny`` = auto-denied,
#: ``escalate`` = needs a human (the host denies by default and records
#: the escalation; a caller may override with ``escalate_behavior``).
RISK_ALLOW = "allow"
RISK_DENY = "deny"
RISK_ESCALATE = "escalate"

#: Tools the CLI uses for writes (edit family).
_WRITE_TOOLS = {
    "write_file",
    "edit",
    "multi_edit",
    "apply_patch",
    "notebook_edit",
}
_READ_TOOLS = {
    "read_file",
    "list_directory",
    "glob",
    "grep",
    "search_file_content",
    "read_many_files",
}
_EXEC_TOOLS = {"shell", "run_shell_command", "bash", "exec", "execute"}
_FETCH_TOOLS = {"web_fetch", "fetch", "web_search"}

#: Path-like keys in tool inputs.
_PATH_KEYS = ("file_path", "path", "target_file", "notebook_path", "cwd")

#: Secret-ish path fragments that must never be written or read out.
_SECRET_MARKERS = (
    ".env",
    "id_rsa",
    "id_ed25519",
    ".pem",
    ".key",
    "credentials",
    ".aws",
    ".ssh",
    ".gnupg",
    ".netrc",
    ".npmrc",
    ".pypirc",
)

#: Shell metacharacters that make a command compound.  Checked outside
#: quotes/escapes only — inside quotes they are data.
_COMPOUND_CHARS = frozenset(";&|<>`\n")
_PAREN_CHARS = frozenset("(){}")

#: argv[0] programs that are never auto-approved: unconstrained
#: interpreters, network tools, and destructive file utilities.
#: ``python*`` is matched by prefix (``python``/``python2``/``python3.x``).
_DENY_PROGRAMS = frozenset({
    "zsh", "dash", "ash", "ksh", "fish",
    "perl", "php",
    "env", "xargs", "sudo", "su", "ssh", "scp",
    "curl", "wget", "awk", "gawk", "mawk", "busybox",
    "rm", "rmdir", "chmod", "chown", "dd",
    "shutdown", "reboot", "halt", "poweroff",
})

#: argv[0] programs auto-approved inside the run roots (read-only file
#: inspection and the test runner).  ``git`` is validated separately.
#: Shell builtins the coding loop leans on. ``cd`` is validated like a
#: path operand (it must stay inside the run root); the others cannot touch
#: the filesystem on their own.
_ALLOW_BUILTINS = frozenset({
    "cd", "export", "set", "unset", "readonly", "local", "true", "false",
    "test", "printf", "echo", "umask", "wait", "shift", ":", "sleep",
})

_ALLOW_PROGRAMS = frozenset({
    # read-only inspection
    "cat", "head", "tail", "wc", "ls", "grep", "rg", "find",
    "pwd", "sort", "uniq", "cut", "tr", "diff", "date", "jq", "yq",
    "echo", "printf", "true", "false", "test", "basename", "dirname", "tee",
    # dev toolchain (2026-11-05: the worker runs the repo's own gates)
    "pytest", "python", "python3", "go", "gofmt", "ruby",
    "node", "npm", "npx", "pnpm", "yarn", "make",
    "bash", "sh",
})

#: Programs whose *arguments are a program*: they can execute commands or
#: write files regardless of flags, so no static flag check makes them safe.
#: ``sed`` is here because scripts can run commands (``e``, ``s///e``) and
#: write files (``w``/``W``/``s///w``), and ``-f`` loads arbitrary scripts —
#: a read-only subset is not decidable without a full parser.
_DENY_SCRIPT_PROGRAMS = frozenset({"sed"})

#: git flag tokens forbidden for every subcommand (config/dir overrides).
_GIT_FORBIDDEN_FLAGS = frozenset({"-c", "-C", "--exec", "--output", "-O"})
#: git branch: the only accepted flags (read-only listing).
_GIT_BRANCH_LIST_FLAGS = frozenset({"--list", "-a", "-v"})


def _norm(text: Any) -> str:
    return str(text or "").strip()


def _roots(roots: Iterable[str] | None) -> list[str]:
    out: list[str] = []
    for raw in roots or ():
        try:
            out.append(os.path.realpath(str(raw)))
        except OSError:
            continue
    return out


def _inside(path: str, roots: list[str]) -> bool:
    try:
        real = os.path.realpath(path)
    except OSError:
        return False
    return any(real == root or real.startswith(root + os.sep) for root in roots)


def _looks_secret(path: str) -> bool:
    low = path.lower()
    return any(marker in low for marker in _SECRET_MARKERS)


def _paths_from_input(tool_input: Any) -> list[str]:
    if not isinstance(tool_input, dict):
        return []
    paths: list[str] = []
    for key in _PATH_KEYS:
        value = tool_input.get(key)
        if isinstance(value, str) and value:
            paths.append(value)
    for key in ("files", "file_paths", "paths"):
        value = tool_input.get(key)
        if isinstance(value, list):
            paths.extend(str(v) for v in value if isinstance(v, str) and v)
    return paths


def _command_from_input(tool_input: Any) -> str:
    if not isinstance(tool_input, dict):
        return ""
    for key in ("command", "cmd", "script"):
        value = tool_input.get(key)
        if isinstance(value, str):
            return value
    return ""


def _has_command_substitution(command: str) -> bool:
    """True if the command performs command substitution (backticks or
    ``$(...)``) outside single quotes. ``${VAR}`` / ``$VAR`` are plain
    variable expansion and stay allowed.
    """
    quote: str | None = None
    i, n = 0, len(command)
    while i < n:
        ch = command[i]
        if quote == "'":
            if ch == "'":
                quote = None
            i += 1
        elif quote == '"':
            if ch == "\\" and i + 1 < n and command[i + 1] in "$`\"\\":
                i += 2
            elif ch == '"':
                quote = None
                i += 1
            elif ch == "`":
                return True
            elif ch == "$" and i + 1 < n and command[i + 1] == "(":
                return True
            else:
                i += 1
        else:
            if ch == "\\":
                i += 2
            elif ch == "'":
                quote = "'"
                i += 1
            elif ch == '"':
                quote = '"'
                i += 1
            elif ch == "`":
                return True
            elif ch == "$" and i + 1 < n and command[i + 1] == "(":
                return True
            else:
                i += 1
    return False


#: Redirection operators (optionally fd-prefixed) and their targets.
_REDIRECT_RE = re.compile(r"(?P<fd>\d?)&?>>?\s*(?P<target>[^\s;&|<>]+)|(?P<in><)\s*(?P<in_target>[^\s;&|<>]+)")


def _split_compound(command: str) -> list[list[str]]:
    """Split a shell command into simple commands on ``&&``/``||``/``;``/
    ``|`` (outside quotes) and strip redirections into their targets.

    Returns the argv lists; redirection targets are validated by the
    caller via ``_redirect_targets``.  Raises ``ValueError`` when the
    command cannot be tokenized.
    """
    lex = shlex.shlex(command, posix=True, punctuation_chars=True)
    lex.whitespace_split = True
    tokens = list(lex)
    segments: list[list[str]] = []
    current: list[str] = []
    for token in tokens:
        if token in ("&&", "||", ";", "|", "&"):
            if current:
                segments.append(current)
            current = []
        else:
            current.append(token)
    if current:
        segments.append(current)
    return segments


def _redirect_targets(command: str) -> list[str]:
    """Paths a command redirects into/from (validated by the caller)."""
    targets: list[str] = []
    for match in _REDIRECT_RE.finditer(command):
        value = match.group("target") or match.group("in_target")
        if value:
            targets.append(value)
    return targets


def _strip_redirects(argv: list[str]) -> list[str]:
    """Drop redirection operators and their targets from one simple
    command so the per-program operand checks only see real arguments.
    """
    out: list[str] = []
    skip_next = False
    for token in argv:
        if skip_next:
            skip_next = False
            continue
        if token in (">", ">>", "<", "2>", "2>>", "1>", "1>>"):
            skip_next = True
            continue
        if token.startswith((">", ">>", "<")) and len(token) > 1:
            # ``>file`` / ``2>file`` / ``>>file`` attached form
            continue
        if re.fullmatch(r"\d?>>?&\d", token):
            # fd duplication (``2>&1``): not a file operand
            continue
        out.append(token)
    return out


def _git_verdict(args: list[str]) -> tuple[str, str, str] | None:
    """Validate ``git <args>``; ``None`` means allowed (paths checked
    by the caller), otherwise ``(risk, rule, reason)``.
    """
    for a in args:
        if a.split("=", 1)[0] in _GIT_FORBIDDEN_FLAGS:
            return (
                RISK_DENY,
                "git-flag-forbidden",
                f"git flag not allowed: {a}",
            )
    sub = next((a for a in args if not a.startswith("-")), None)
    if sub is None:
        return (RISK_DENY, "git-disallowed", "git without a subcommand")
    rest = args[args.index(sub) + 1:]
    # 只读子命令白名单（2026-11-04 扩：ls-files/ls-tree/cat-file/grep/describe/shortlog/blame/remote）
    if sub in (
        "status",
        "diff",
        "log",
        "show",
        "rev-parse",
        "ls-files",
        "ls-tree",
        "cat-file",
        "grep",
        "describe",
        "shortlog",
        "blame",
        "remote",
        "check-ignore",
        "check-attr",
        "var",
        "for-each-ref",
    ):
        return None
    if sub == "branch":
        if all(a in _GIT_BRANCH_LIST_FLAGS for a in rest):
            return None
        return (
            RISK_DENY,
            "git-disallowed",
            "git branch only allows list flags (--list/-a/-v)",
        )
    if sub == "checkout":
        if rest and rest[0] == "--":
            return None
        return (
            RISK_DENY,
            "git-disallowed",
            "git checkout only allows: checkout -- <path>",
        )
    if sub == "add":
        if "--" in rest:
            # End-of-options delimiter: every token before it must be
            # a pathspec (no flags); after it any spelling is a
            # pathspec — the real guard is the shared operand loop.
            pre = rest[: rest.index("--")]
            ok = not any(a.startswith("-") for a in pre)
        else:
            ok = not any(a.startswith("-") for a in rest)
        if ok:
            return None
        return (
            RISK_DENY,
            "git-disallowed",
            "git add only accepts pathspecs (no flags)",
        )
    if sub == "commit":
        if not rest or (len(rest) == 2 and rest[0] == "-m"):
            return None
        return (
            RISK_DENY,
            "git-disallowed",
            "git commit only allows: commit [-m <message>]",
        )
    return (
        RISK_DENY,
        "git-disallowed",
        f"git subcommand not allowed: {sub}",
    )


#: find actions that execute commands or write files (the flag may be
#: abbreviated: find accepts any unambiguous prefix, so match by prefix).
_FIND_ACTION_PREFIXES = (
    "-exec",
    "-execdir",
    "-ok",
    "-okdir",
    "-delete",
    "-fprint",
    "-fprintf",
    "-fls",
)


def _find_has_action(args: list[str]) -> bool:
    return any(a.startswith(_FIND_ACTION_PREFIXES) for a in args)


def _rg_has_pre(args: list[str]) -> bool:
    return any(a == "--pre" or a.startswith("--pre=") for a in args)


def _path_like(arg: str) -> bool:
    return (
        arg in (".", "..", "~")
        or arg.startswith(("/", "./", "../", "~/"))
        or "/" in arg
    )


def _classify_argv(
    argv: list[str], cwd_input: str | None, root_list: list[str], command: str
) -> dict[str, Any]:
    """Policy for one simple command: program allowlist, git verdict and
    the shared operand/path guards."""
    prog = os.path.basename(argv[0])
    if prog in _ALLOW_BUILTINS:
        if prog == "cd":
            base = cwd_input or (root_list[0] if root_list else os.getcwd())
            target = argv[1] if len(argv) > 1 else base
            if not _inside(os.path.join(base, os.path.expanduser(target)), root_list):
                return _result(
                    RISK_DENY,
                    "outside-root",
                    "cd target is outside the run root",
                    target,
                )
        return _result(RISK_ALLOW, "exec-allow", "shell builtin allowed", prog)
    if prog in ("bash", "sh") and any(
        a in ("-c", "-lc", "-ic", "-ec", "-xc", "--command") for a in argv[1:]
    ):
        return _result(
            RISK_DENY,
            "exec-inline-shell",
            "inline shell code (-c) is not auto-approved; run a script file instead",
            argv[0],
        )
    if prog in _DENY_PROGRAMS:
        return _result(
            RISK_DENY,
            "exec-denied",
            f"program not allowed in worker runs: {argv[0]}",
            argv[0],
        )
    skip_message = False
    if prog == "git":
        verdict = _git_verdict(argv[1:])
        if verdict is not None:
            return _result(verdict[0], verdict[1], verdict[2], argv[0])
        # ``git commit -m <message>``: the message is data, not a path.
        if len(argv) == 4 and argv[1] == "commit" and argv[2] == "-m":
            skip_message = True
    elif prog in _DENY_SCRIPT_PROGRAMS:
        return _result(
            RISK_DENY,
            "exec-script-program",
            (
                f"{prog} is not auto-approved: its scripts can execute commands "
                "or write files (e/E, w/W, r, s///e, s///w, -f); use edit_file/"
                "apply_patch or request explicit review"
            ),
            argv[0],
        )
    elif prog == "find" and _find_has_action(argv[1:]):
        return _result(
            RISK_DENY, "find-action", "find action flag not allowed", argv[0]
        )
    elif prog == "rg" and _rg_has_pre(argv[1:]):
        return _result(
            RISK_DENY,
            "rg-pre",
            "rg --pre external preprocessor not allowed",
            argv[0],
        )
    elif prog not in _ALLOW_PROGRAMS:
        return _result(
            RISK_ESCALATE,
            "exec-unknown",
            "command not in the auto-approve allowlist",
            argv[0],
        )

    base_dir = cwd_input or (root_list[0] if root_list else os.getcwd())

    def _guard_operand(value: str) -> dict[str, Any] | None:
        """The two path-guard checks for one candidate operand: a
        secret-like name is refused, and the resolved path (symlinks
        followed) must stay inside the run roots.  Returns the deny
        result, or ``None`` when the operand passes both checks."""
        if _looks_secret(value):
            return _result(
                RISK_DENY,
                "secret-path",
                "refuses to touch credential-like files",
                value,
            )
        if not _inside(os.path.join(base_dir, os.path.expanduser(value)), root_list):
            return _result(
                RISK_DENY,
                "outside-root",
                "path argument is outside the run root",
                value,
            )
        return None

    after_sep = False
    for i, arg in enumerate(argv[1:]):
        if _looks_secret(arg):
            return _result(
                RISK_DENY,
                "secret-path",
                "refuses to touch credential-like files",
                arg,
            )
        if skip_message and i == 2:
            continue
        if not after_sep and arg == "--":
            # End-of-options delimiter: not an operand itself; every
            # token after it is a literal operand, never a flag.
            after_sep = True
            continue
        if after_sep:
            # After the delimiter a leading ``-`` and an embedded
            # ``=`` are part of the file name — never flag syntax, so
            # the token is never split on ``=``.  Same existence gate
            # as a bare operand: a present (or path-like) name is a
            # real file (or a symlink to one) and gets the same two
            # checks; a missing bare name is a pattern or argument
            # left to the command itself.
            resolved = os.path.join(base_dir, os.path.expanduser(arg))
            if not (_path_like(arg) or os.path.lexists(resolved)):
                continue
            verdict = _guard_operand(arg)
            if verdict is not None:
                return verdict
            continue
        if arg.startswith("-"):
            # A bare flag (``tail -f``, ``-n`` …): never a path
            # operand.  (Only before the end-of-options delimiter —
            # after ``--`` a dashed token is a file name, handled
            # above.)  The value part of an equals-form flag
            # (``--files0-from=<file>``) is different: it is data the
            # program actually opens (a file, a NUL-separated file
            # list), so it is an operand exactly like a bare one and
            # gets the same two checks whenever it is path-like or
            # names something that exists in the run dir.  Non-path
            # values (``--color=auto``, ``--include=*.py``) are
            # neither path-like nor present and are left to the
            # command itself, just like missing bare names.
            if "=" not in arg:
                continue
            value = arg.split("=", 1)[1]
            v_resolved = os.path.join(base_dir, os.path.expanduser(value))
            if not (_path_like(value) or os.path.lexists(v_resolved)):
                continue
            verdict = _guard_operand(value)
            if verdict is not None:
                return verdict
            continue
        path_like = _path_like(arg)
        resolved = os.path.join(base_dir, os.path.expanduser(arg))
        # A bare name (no path separator) is only an operand when it
        # names something that exists in the run dir: the shell resolves
        # bare names against the cwd, so an existing one is a real file
        # (or a symlink to one) — while a missing bare name is a pattern
        # or argument (``grep hello nomatch``), and only the command
        # itself can fail. Explicit path spellings (``./x``, ``/x``,
        # ``../x``, ``x/y``, ``.``, ``..``, ``~``) keep the unconditional
        # check, exactly as before.
        if not (path_like or os.path.lexists(resolved)):
            continue
        verdict = _guard_operand(arg)
        if verdict is not None:
            return verdict
    if not root_list:
        return _result(
            RISK_ESCALATE,
            "exec-unrooted",
            "no run root configured, nothing is auto-approved",
            argv[0],
        )
    return _result(
        RISK_ALLOW,
        "exec-allow",
        "command passed the shell policy",
        command[:200],
    )


def _classify_shell(tool_input: Any, root_list: list[str]) -> dict[str, Any]:
    """Fail-closed shell policy (see module docstring).

    Compound commands are allowed as long as **every** simple command and
    every redirection target passes the same program/path policy: pipes,
    ``&&``, ``||`` and ``;`` are normal in a coding loop (``go test ... |
    tail``, ``pytest && git status``).  Command substitution (backticks,
    ``$(...)``) stays denied because it hides a nested command from the
    static check.
    """
    command = _command_from_input(tool_input).strip()
    if not command:
        return _result(RISK_ESCALATE, "exec-empty", "empty command", command)
    if _has_command_substitution(command):
        return _result(
            RISK_DENY,
            "exec-substitution",
            "command substitution (backticks / $(...)) is not allowed",
            command[:200],
        )
    try:
        segments = _split_compound(command)
    except ValueError:
        return _result(
            RISK_DENY, "exec-parse", "command could not be tokenized", command[:200]
        )
    if not segments:
        return _result(RISK_DENY, "exec-parse", "command has no argv", command[:200])

    cwd_input: str | None = None
    if isinstance(tool_input, dict):
        raw = tool_input.get("cwd")
        if isinstance(raw, str) and raw:
            cwd_input = raw
    if cwd_input is not None:
        if not os.path.isdir(cwd_input):
            return _result(
                RISK_DENY, "outside-root-cwd", "cwd does not exist", cwd_input
            )
        if not _inside(cwd_input, root_list):
            return _result(
                RISK_DENY,
                "outside-root-cwd",
                "cwd is outside the run root",
                cwd_input,
            )

    base_dir = cwd_input or (root_list[0] if root_list else os.getcwd())
    for target in _redirect_targets(command):
        if target in ("/dev/null", "/dev/stdout", "/dev/stderr"):
            continue
        if _looks_secret(target):
            return _result(
                RISK_DENY,
                "secret-path",
                "refuses to redirect into credential-like files",
                target,
            )
        if not _inside(os.path.join(base_dir, os.path.expanduser(target)), root_list):
            return _result(
                RISK_DENY,
                "outside-root",
                "redirection target is outside the run root",
                target,
            )

    for segment in segments:
        argv = _strip_redirects(segment)
        if not argv:
            continue
        verdict = _classify_argv(argv, cwd_input, root_list, command)
        if verdict["risk"] != RISK_ALLOW:
            return verdict
    return _result(
        RISK_ALLOW,
        "exec-allow",
        "command passed the shell policy",
        command[:200],
    )


def classify_tool(
    tool_name: str,
    tool_input: Any,
    roots: Iterable[str] | None = None,
) -> dict[str, Any]:
    """Map one tool call to a risk decision (pure function).

    Returns ``{"risk","rule","reason","target"}``; never raises.
    """
    name = _norm(tool_name)
    root_list = _roots(roots)
    lowered = name.lower()

    if name.startswith("mcp__") or name.startswith("mcp."):
        return _result(RISK_ESCALATE, "mcp", "MCP tool call needs review", name)

    if lowered in _FETCH_TOOLS or "fetch" in lowered:
        return _result(RISK_DENY, "network", "network access is not allowed", name)

    if lowered in _EXEC_TOOLS or "shell" in lowered or "exec" in lowered:
        return _classify_shell(tool_input, root_list)

    if lowered in _WRITE_TOOLS or "write" in lowered or "edit" in lowered:
        paths = _paths_from_input(tool_input)
        if not paths:
            return _result(RISK_ESCALATE, "write-no-path", "write without a path", name)
        for path in paths:
            if _looks_secret(path):
                return _result(
                    RISK_DENY,
                    "secret-path",
                    "refuses to write credential-like files",
                    path,
                )
            if root_list and not _inside(path, root_list):
                return _result(
                    RISK_DENY,
                    "outside-root",
                    "path is outside the run root",
                    path,
                )
        return _result(RISK_ALLOW, "write-inside", "write inside the run root", paths[0])

    if lowered in _READ_TOOLS or "read" in lowered or "list" in lowered:
        paths = _paths_from_input(tool_input)
        for path in paths:
            if _looks_secret(path):
                return _result(
                    RISK_DENY,
                    "secret-path",
                    "refuses to read credential-like files",
                    path,
                )
            if root_list and not _inside(path, root_list):
                return _result(
                    RISK_DENY,
                    "outside-root",
                    "path is outside the run root",
                    path,
                )
        return _result(RISK_ALLOW, "read", "read-only tool", paths[0] if paths else name)

    return _result(
        RISK_ESCALATE,
        "unknown-tool",
        "tool is not in the policy table",
        name,
    )


def _result(risk: str, rule: str, reason: str, target: str) -> dict[str, Any]:
    return {"risk": risk, "rule": rule, "reason": reason, "target": target}


def decide(
    tool_name: str,
    tool_input: Any,
    roots: Iterable[str] | None = None,
    escalate_behavior: str = "deny",
) -> dict[str, Any]:
    """Turn a classification into a control-protocol permission result."""
    info = classify_tool(tool_name, tool_input, roots)
    risk = info["risk"]
    behavior = "allow" if risk == RISK_ALLOW else "deny"
    if risk == RISK_ESCALATE:
        behavior = "allow" if escalate_behavior == "allow" else "deny"
    return {
        **info,
        "behavior": behavior,
        "message": (
            f"worker-proxy approval: {info['rule']} — {info['reason']}"
            if behavior == "deny"
            else f"worker-proxy approval: {info['rule']}"
        ),
        "requires_human": risk == RISK_ESCALATE and behavior == "deny",
    }


def control_response(request_id: str, decision: dict[str, Any]) -> dict[str, Any]:
    """Build the stream-json ``control_response`` for one approval request."""
    if decision.get("behavior") == "allow":
        payload: dict[str, Any] = {"behavior": "allow"}
        updated_input = decision.get("updated_input")
        if isinstance(updated_input, dict) and updated_input:
            # AskUserQuestionTool consumes ``updatedInput.answers`` (answerKey
            # = question index as a string). Passing the human's answer here
            # is how the tool receives it in SDK/stream-json mode.
            payload["updatedInput"] = updated_input
    else:
        payload = {
            "behavior": "deny",
            "message": str(decision.get("message") or "denied"),
            "interrupt": False,
        }
    return {
        "type": "control_response",
        "response": {
            "subtype": "success",
            "request_id": request_id,
            "response": payload,
        },
    }


def receipt_entry(
    task_id: str,
    tool_name: str,
    tool_input: Any,
    decision: dict[str, Any],
    request_id: str,
    source: str = "control",
) -> dict[str, Any]:
    """One append-only audit record for a proxy-approval decision.

    ``source`` names the deciding path: ``"control"`` (the host answered
    a ``can_use_tool`` control request) or ``"hook"`` (the PreToolUse
    command hook decided — no control request reaches the host).
    """
    return {
        "ts": __import__("datetime").datetime.utcnow().isoformat() + "Z",
        "task_id": task_id,
        "kind": "approval",
        "request_id": request_id,
        "tool_name": tool_name,
        "input": tool_input,
        "risk": decision.get("risk"),
        "rule": decision.get("rule"),
        "reason": decision.get("reason"),
        "behavior": decision.get("behavior"),
        "requires_human": bool(decision.get("requires_human")),
        "source": source,
    }


def append_receipt(path: Path, entry: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as fh:
        fh.write(json.dumps(entry, ensure_ascii=False) + "\n")
