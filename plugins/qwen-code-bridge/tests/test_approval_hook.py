# -*- coding: utf-8 -*-
"""PreToolUse policy hook (#1411) — end-to-end via subprocess.

Runs ``hook_policy.py`` exactly the way the dispatch CLI home invokes it
(``<sys.executable> .../hook_policy.py``, hook JSON on stdin) and checks
the PreToolUse permission decision, then covers the settings merge done
by ``prepare_cli_home`` (hook installed, idempotent, other settings and
hooks preserved).
"""

import json
import os
import subprocess
import sys
from pathlib import Path

from qwen_code_bridge import state

HOOK_POLICY = (
    Path(__file__).resolve().parent.parent
    / "qwen_code_bridge"
    / "hook_policy.py"
)


def _run_hook(payload, roots, raw_input=None):
    """Invoke the hook script once; return the parsed stdout JSON."""
    env = {
        k: v for k, v in os.environ.items() if k != state.APPROVAL_ROOTS_ENV
    }
    if roots is not None:
        env[state.APPROVAL_ROOTS_ENV] = json.dumps(roots)
    data = raw_input if raw_input is not None else json.dumps(payload)
    proc = subprocess.run(
        [sys.executable, str(HOOK_POLICY)],
        input=data,
        capture_output=True,
        text=True,
        env=env,
        timeout=30,
    )
    assert proc.returncode == 0, proc.stderr
    return json.loads(proc.stdout)


def _decision(out):
    return out["hookSpecificOutput"]["permissionDecision"]


def _reason(out):
    return out["hookSpecificOutput"]["permissionDecisionReason"]


def _shell_payload(command, cwd):
    return {
        "session_id": "test",
        "hook_event_name": "PreToolUse",
        "cwd": str(cwd),
        "tool_name": "run_shell_command",
        "tool_input": {"command": command},
    }


# 1. compound shell command → deny (exec-compound)
def test_exec_compound_deny(tmp_path):
    out = _run_hook(
        _shell_payload("pwd; python3 -c 'print(1)'", tmp_path),
        roots=[str(tmp_path)],
    )
    assert _decision(out) == "deny"
    assert "exec-compound" in _reason(out)


# 2. read outside the run root → deny (outside-root)
def test_cat_outside_root_deny(tmp_path):
    out = _run_hook(
        _shell_payload("cat /var/private/data", tmp_path),
        roots=[str(tmp_path)],
    )
    assert _decision(out) == "deny"
    assert "outside-root" in _reason(out)


# 3. destructive git subcommand → deny (git-disallowed)
def test_git_branch_delete_deny(tmp_path):
    out = _run_hook(
        _shell_payload("git branch -D x", tmp_path),
        roots=[str(tmp_path)],
    )
    assert _decision(out) == "deny"
    assert "git-disallowed" in _reason(out)


# 4. plain git add → allow
def test_git_add_allow(tmp_path):
    out = _run_hook(
        _shell_payload("git add file", tmp_path),
        roots=[str(tmp_path)],
    )
    assert _decision(out) == "allow"


# 5. semicolon inside quotes is data, not a compound command → allow
def test_git_commit_quoted_semicolon_allow(tmp_path):
    out = _run_hook(
        _shell_payload('git commit -m "fix: a;b"', tmp_path),
        roots=[str(tmp_path)],
    )
    assert _decision(out) == "allow"


# 5b. sed scripts can execute commands / write files → deny (exec-script-program)
def test_sed_script_execution_deny(tmp_path):
    for command in (
        "sed '1e touch /tmp/qcb-marker' f",
        "sed 's/.*/x/e' f",
        "sed '1w /tmp/qcb-marker' f",
        "sed -f script.sed f",
    ):
        out = _run_hook(
            _shell_payload(command, tmp_path), roots=[str(tmp_path)]
        )
        assert _decision(out) == "deny", command
        assert "exec-script-program" in _reason(out), command


# 5c. find writing actions → deny (find-action)
def test_find_fprint_deny(tmp_path):
    out = _run_hook(
        _shell_payload("find . -fprint marker.txt", tmp_path),
        roots=[str(tmp_path)],
    )
    assert _decision(out) == "deny"
    assert "find-action" in _reason(out)


# 6. run roots missing from the environment → fail closed
def test_missing_roots_fail_closed(tmp_path):
    out = _run_hook(
        _shell_payload("git status", tmp_path),
        roots=None,
    )
    assert _decision(out) == "deny"
    assert state.APPROVAL_ROOTS_ENV in _reason(out)


# 7. malformed hook input → fail closed, reason without the input
def test_invalid_json_deny():
    out = _run_hook(None, roots=["/tmp"], raw_input="{not json")
    assert _decision(out) == "deny"
    assert "hook-fault" in _reason(out)
    assert "not json" not in _reason(out)


# 8. prepare_cli_home installs the hook (matcher *) and is idempotent
def test_prepare_cli_home_installs_hook_idempotent(tmp_path):
    home = tmp_path / "home"
    qdir = state.prepare_cli_home(str(home))
    settings_path = Path(qdir) / "settings.json"
    first = settings_path.read_text(encoding="utf-8")

    data = json.loads(first)
    pre = data["hooks"]["PreToolUse"]
    star = [e for e in pre if e.get("matcher") == "*"]
    assert star, f"no matcher '*' entry in {pre!r}"
    hooks = star[0]["hooks"]
    ours = [h for h in hooks if h.get("name") == state.APPROVAL_HOOK_NAME]
    assert len(ours) == 1
    h = ours[0]
    assert h["type"] == "command"
    assert h["timeout"] == 10
    assert "hook_policy.py" in h["command"]
    assert sys.executable in h["command"]

    qdir2 = state.prepare_cli_home(str(home))
    assert qdir2 == qdir
    second = settings_path.read_text(encoding="utf-8")
    assert second == first  # idempotent: byte-identical

    data2 = json.loads(second)
    all_hooks = [
        h
        for e in data2["hooks"]["PreToolUse"]
        for h in (e.get("hooks") or [])
        if isinstance(h, dict)
    ]
    assert (
        sum(1 for h in all_hooks if h.get("name") == state.APPROVAL_HOOK_NAME)
        == 1
    )


# 9. other settings and hooks survive the merge
def test_prepare_preserves_other_settings(tmp_path):
    home = tmp_path / "home"
    qdir = home / ".qwen"
    qdir.mkdir(parents=True)
    (qdir / "settings.json").write_text(
        json.dumps(
            {
                "model": {"name": "qwen3-coder"},
                "hooks": {
                    "PreToolUse": [
                        {
                            "matcher": "write_file",
                            "hooks": [
                                {
                                    "type": "command",
                                    "command": "echo other",
                                    "name": "other-hook",
                                }
                            ],
                        }
                    ]
                },
            }
        ),
        encoding="utf-8",
    )

    state.prepare_cli_home(str(home))
    data = json.loads((qdir / "settings.json").read_text(encoding="utf-8"))
    assert data["model"] == {"name": "qwen3-coder"}
    names = [
        h.get("name")
        for e in data["hooks"]["PreToolUse"]
        for h in (e.get("hooks") or [])
        if isinstance(h, dict)
    ]
    assert "other-hook" in names
    assert state.APPROVAL_HOOK_NAME in names
