"""Backend-independent Harness health request/result transport over AgentFS.

The image supplies an executable adapter. This module knows no model protocol,
model configuration layout or SDK. It never treats process liveness as inference.
"""
import datetime
import json
import os
import subprocess
import time

COMMAND = "/opt/agentteams/harness-health"
REASONS = {"ok", "authentication", "billing_or_quota", "model_or_endpoint",
           "rate_limit_or_quota", "upstream_unavailable", "configuration", "timeout",
           "network", "invalid_response", "empty_response", "disabled", "unsupported"}


def now():
    return datetime.datetime.now(datetime.timezone.utc)


def invoke(command=COMMAND):
    try:
        process = subprocess.run([command, "--json"], capture_output=True, timeout=25)
        if len(process.stdout) > 16384:
            raise ValueError("oversized result")
        result = json.loads(process.stdout)
        state, reason = result.get("status"), result.get("reason")
        if reason not in REASONS or state not in {"ok", "failed", "unknown"}:
            raise ValueError("invalid result")
        if (state == "ok") != (reason == "ok") or (state == "ok" and process.returncode != 0):
            raise ValueError("inconsistent result")
        if state == "unknown" and reason not in {"configuration", "disabled", "unsupported"}:
            raise ValueError("invalid unknown reason")
        if state == "failed" and reason in {"disabled", "unsupported"}:
            raise ValueError("invalid failure reason")
        configuration = result.get("configuration", "")
        if configuration and (not isinstance(configuration, str) or len(configuration) != 64 or any(
                c not in "0123456789abcdef" for c in configuration)):
            raise ValueError("invalid configuration revision")
        return {"status": state, "reason": reason, "configuration": result.get("configuration", "")}
    except subprocess.TimeoutExpired:
        return {"status": "failed", "reason": "timeout"}
    except FileNotFoundError:
        return {"status": "unknown", "reason": "unsupported"}
    except (OSError, ValueError, TypeError, AttributeError):
        return {"status": "failed", "reason": "invalid_response"}


def readiness(command=COMMAND):
    try:
        process = subprocess.run([command, "--readiness"], capture_output=True, timeout=6)
        if process.returncode != 0 or len(process.stdout) > 4096:
            raise ValueError("invalid readiness result")
        result = json.loads(process.stdout)
        if result.get("version") != 1 or result.get("status") not in {"ready", "live", "starting"}:
            raise ValueError("invalid readiness contract")
        evidence = result.get("evidence", "harness_health")
        if not isinstance(evidence, str) or len(evidence) > 64 or any(
                c not in "abcdefghijklmnopqrstuvwxyz0123456789_.-" for c in evidence):
            evidence = "harness_health"
        return {"version": 1, "status": result["status"], "evidence": evidence,
                "matrix_verified": result.get("matrix_verified") is True}
    except (OSError, ValueError, TypeError, AttributeError, subprocess.SubprocessError):
        return {"version": 1, "status": "starting", "evidence": "harness_health_unavailable"}


class Reporter:
    def __init__(self, runtime, member, child, command=COMMAND):
        self.runtime, self.member, self.child, self.command = runtime, member, child, command
        self.prefix = os.environ["AGENTTEAMS_STORAGE_PREFIX"].rstrip("/") + "/agents/" + member + "/runtime/"
        self.last = None
        self.request_id = None
        self.pending = None

    def configuration(self):
        result = subprocess.run([self.command, "--configuration"], capture_output=True, timeout=5)
        value = result.stdout.decode().strip()
        return value if result.returncode == 0 and len(value) == 64 and all(c in "0123456789abcdef" for c in value) else None

    def request(self):
        process = subprocess.run(["mc", "cat", self.prefix + "health-request.json"],
                                 capture_output=True, timeout=10)
        if process.returncode != 0:
            return None
        if len(process.stdout) > 4096:
            return None
        try:
            value = json.loads(process.stdout)
            request_id = value.get("id")
            expires = datetime.datetime.fromisoformat(value["expires_at"].replace("Z", "+00:00"))
        except (ValueError, TypeError, KeyError, AttributeError):
            return None
        if value.get("version") == 1 and isinstance(request_id, str) and len(request_id) == 32 and all(
                c in "0123456789abcdef" for c in request_id) and now() < expires <= now() + datetime.timedelta(minutes=2):
            return request_id
        return None

    def check(self, request_id=None):
        if self.pending is None:
            result = {"status": "unknown", "reason": "disabled"} if os.getenv("AGENTTEAMS_MODEL_PREFLIGHT_ENABLED") == "0" else invoke(self.command)
            report = {"version": 1, "runtime": self.runtime, "member": self.member,
                      "checked_at": now().isoformat(), **result}
            if request_id:
                report["request_id"] = request_id
            self.pending = report
        report = self.pending
        if self.child.poll() is not None:
            raise OSError("native process exited during check")
        data = json.dumps(report, separators=(",", ":")).encode()
        subprocess.run(["mc", "pipe", self.prefix + "health.json"], input=data,
                       capture_output=True, timeout=15, check=True)
        self.last, self.request_id, self.pending = report, report.get("request_id"), None
        return report

    def run(self):
        next_check = 0
        while self.child.poll() is None:
            try:
                request_id = self.request()
                configuration = self.configuration() or ""
                if self.pending or time.monotonic() >= next_check or (request_id and request_id != self.request_id) or (
                        self.last and configuration != self.last.get("configuration", "")):
                    report = self.check(request_id)
                    next_check = time.monotonic() + (300 if report["status"] == "ok" else 60)
            except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
                # Missing storage/configuration during startup is not a successful check.
                # Keep retrying; the Controller will expose missing/expired results as unknown.
                pass
            time.sleep(30)


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime", required=True)
    parser.add_argument("--member", required=True)
    parser.add_argument("--parent", type=int, required=True)
    args = parser.parse_args()

    class Parent:
        def poll(self):
            try:
                os.kill(args.parent, 0)
                return None
            except ProcessLookupError:
                return 0

    Reporter(args.runtime, args.member, Parent()).run()
