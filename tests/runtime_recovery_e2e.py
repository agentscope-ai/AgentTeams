"""Opt-in #1631 live acceptance; only the named disposable installation.

Run with a real AGENTTEAMS_LLM_API_KEY. Never discovers or changes a default
installation. Runtime/model execution evidence is separate from setup readiness.
"""
import json
import base64
import hashlib
import os
import re
from pathlib import Path
import secrets
import subprocess
import sys
import time
import urllib.parse
import urllib.request

PREFIX = os.getenv("RECOVERY_E2E_PREFIX", "recovery-1631-live-20261116")
if not re.fullmatch(r"recovery-1631-live-\d{8}(?:-[a-z0-9]+)?", PREFIX):
    raise ValueError("Use a dated, disposable recovery-1631-live-YYYYMMDD prefix")
CONTROLLER = PREFIX + "-controller"
NETWORK = PREFIX + "-net"
STATE = Path("/tmp/recovery-1631-e2e-state.json" if PREFIX == "recovery-1631-live-20261116" else "/tmp/" + PREFIX + "-state.json")
PRIVATE_BOOTSTRAP = Path("/tmp/recovery-1631-bootstrap.private.json" if PREFIX == "recovery-1631-live-20261116" else "/tmp/" + PREFIX + "-bootstrap.private.json")
CORE_IMAGE = "recovery-1631/controller:local-20261117052000"
HARNESS_IMAGES = {"codex": "recovery-1631/codex:local-20261117013200",
                  "hermes": "recovery-1631/hermes:local-20261117052000",
                  "deepseek-harness": "recovery-1631/deepseek:local-20261117052000"}
SOURCE, TARGET = os.getenv("RECOVERY_E2E_FROM", "codex"), os.getenv("RECOVERY_E2E_TO", "hermes")
MODE = os.getenv("RECOVERY_E2E_MODE", "replace")
if SOURCE not in HARNESS_IMAGES or TARGET not in HARNESS_IMAGES or MODE not in {"replace", "restart"}:
    raise ValueError("Select a packaged recovery Harness and replace/restart mode")
if (MODE == "restart") != (SOURCE == TARGET):
    raise ValueError("Restart keeps the Harness; replacement switches between packaged Harnesses")
CASE = os.getenv("RECOVERY_E2E_CASE", "01")
if len(CASE) != 2 or not CASE.isdecimal():
    raise ValueError("RECOVERY_E2E_CASE must be two decimal digits")
TASK_ID, MAIN_ID = "live-" + SOURCE + "-" + TARGET + "-" + CASE, "live-main-" + CASE


def docker(*args, text=None):
    result = subprocess.run(["docker", *args], input=text, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError("docker " + args[0] + " failed: " + result.stderr[:2048])
    return result.stdout.strip()


def agt(*args, text=None):
    return docker("exec", "-i", CONTROLLER, "agt", *args, text=text)


def state():
    return json.loads(STATE.read_text())


def api(method, path, body=None, *, headers=None):
    token = docker("exec", CONTROLLER, "cat", "/var/run/agentteams/cli-token")
    # OrbStack may rebind dynamically published ports on network attachment.
    port = docker("port", CONTROLLER, "8090/tcp").rsplit(":", 1)[1]
    request = urllib.request.Request("http://127.0.0.1:" + port + "/api/v1/" + path,
        data=json.dumps(body).encode() if body is not None else None, method=method,
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json", **(headers or {})})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def tracing():
    # Reuse the running local backend without changing its configuration or data.
    # Only our disposable Collector and Controller join its network.
    original = json.loads(docker("inspect", "agentteams-otel-collector"))[0]
    env = dict(item.split("=", 1) for item in original["Config"]["Env"])
    public, secret = base64.b64decode(env["LANGFUSE_BASIC_AUTH"]).decode().split(":", 1)
    collector = PREFIX + "-collector"
    if subprocess.run(["docker", "inspect", collector], capture_output=True).returncode:
        config = next(m["Source"] for m in original["Mounts"] if m["Destination"] == "/etc/otelcol-contrib/config.yaml")
        args = ["run", "-d", "--name", collector, "--network", NETWORK,
                "--label", "io.agentteams.recovery.owner=1631", "-v", config + ":/etc/otelcol-contrib/config.yaml:ro",
                "-v", PREFIX + "-traces:/var/lib/otelcol"]
        for key in ("LANGFUSE_BASIC_AUTH", "LANGFUSE_OTLP_BASE_URL"):
            args.extend(["-e", key + "=" + env[key]])
        docker(*args, original["Config"]["Image"], "--config=/etc/otelcol-contrib/config.yaml")
    for container in (CONTROLLER, collector):
        networks = json.loads(docker("inspect", container))[0]["NetworkSettings"]["Networks"]
        if "agentteams-net" not in networks:
            docker("network", "connect", "agentteams-net", container)
    if json.loads(docker("inspect", collector))[0]["State"]["Status"] != "running":
        docker("run", "--rm", "--entrypoint", "/bin/sh", "-v", PREFIX + "-traces:/traces", CORE_IMAGE,
               "-c", "chown 10001:10001 /traces")
        docker("start", collector)
    previous = api("GET", "observability/config")
    saved = api("PUT", "observability/config", {"revision": previous.get("revision"), "backend": "langfuse",
        "enabled": True, "captureContent": False, "mode": "collector", "langfuseUrl": "http://agentteams-langfuse:3000",
        "publicUrl": "http://127.0.0.1:13100", "publicKey": public, "secretKey": secret,
        "collectorEndpoint": "http://" + collector + ":4318"})
    result = api("POST", "observability/apply", {"revision": saved["revision"], "workers": ["leader"]})
    # Apply currently supports Hermes/QwenPaw; Codex receives the same ingestion
    # settings through its Worker spec, without any backend query credentials.
    worker = api("GET", "workers/agent-a")
    worker_env = worker.get("env", {})
    worker_env.update(AGENTTEAMS_OTEL_ENABLED="true", AGENTTEAMS_OTEL_CAPTURE_CONTENT="false",
        AGENTTEAMS_OTEL_CONFIG_REVISION=saved["revision"], OTEL_EXPORTER_OTLP_ENDPOINT="http://" + collector + ":4318",
        OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="http://" + collector + ":4318/v1/traces", OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf")
    api("PUT", "workers/agent-a", {"env": worker_env})
    print(json.dumps({"configured": True, "revision": saved["revision"], "apply": result,
                      "acceptance": "real trace ingestion and readback not yet verified"}))


def prepare():
    # Configure the SDK before assigning work. Its interactive home-channel
    # hints would otherwise introduce extra Matrix input during cutover.
    for name in ("agent-a", "leader"):
        worker = api("GET", "workers/" + name)
        if not worker.get("roomID"):
            raise RuntimeError("Wait for the disposable member's personal room before preparation")
        env = worker.get("env", {})
        if env.get("MATRIX_HOME_ROOM") != worker["roomID"]:
            env["MATRIX_HOME_ROOM"] = worker["roomID"]
            api("PUT", "workers/" + name, {"env": env})
    print("Prepared native home rooms only for the disposable test members; wait for Running before seeding")


def oracle():
    """Start a ledger with no Worker storage mount or Docker socket."""
    name = PREFIX + "-oracle"
    config_path = Path("/tmp/" + PREFIX + "-oracle.private.json")
    if config_path.exists() or subprocess.run(["docker", "inspect", name], capture_output=True).returncode == 0:
        raise RuntimeError("Oracle already exists; inspect and retain its ledger")
    config = {"controller": "http://" + CONTROLLER + ":8090", "proof_key": secrets.token_urlsafe(32)}
    with os.fdopen(os.open(config_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as stream:
        json.dump(config, stream)
    docker("volume", "create", "--label", "io.agentteams.recovery.owner=1631", name + "-ledger")
    docker("run", "-d", "--name", name, "--network", NETWORK,
           "--label", "io.agentteams.recovery.owner=1631", "-p", "127.0.0.1::8091",
           "-v", str(config_path) + ":/private/config.json:ro", "-v", name + "-ledger:/ledger",
           "-v", str(Path(__file__).resolve().parent / "fixtures/runtime_recovery_oracle.py") + ":/oracle.py:ro",
           "--entrypoint", "python3", CORE_IMAGE, "/oracle.py")
    print(json.dumps({"oracle": name, "worker_writable_ledger": False}))


def oracle_proof():
    config = json.loads(Path("/tmp/" + PREFIX + "-oracle.private.json").read_text())
    port = docker("port", PREFIX + "-oracle", "8091/tcp").rsplit(":", 1)[1]
    query = urllib.parse.urlencode({"team": "recovery-team", "task": TASK_ID})
    request = urllib.request.Request("http://127.0.0.1:" + port + "/proof?" + query,
        headers={"Authorization": "Bearer " + config["proof_key"]})
    with urllib.request.urlopen(request, timeout=20) as response:
        return json.load(response)


def seed():
    task_id, main_id, team = TASK_ID, MAIN_ID, "recovery-team"
    member = api("GET", "workers/agent-a")
    actual = json.loads(docker("inspect", PREFIX + "-worker-agent-a"))[0]
    if member.get("phase") != "Running" or member.get("runtime") != SOURCE or not actual["State"]["Running"] or actual["Config"]["Image"] != HARNESS_IMAGES[SOURCE]:
        raise RuntimeError("Wait for the selected source Harness and image to be running before assignment")
    room = api("GET", "teams/" + team)["teamRoomID"]
    if subprocess.run(["docker", "exec", CONTROLLER, "mc", "stat",
            "agentteams/recovery-1631/teams/" + team + "/shared/tasks/" + task_id + "/meta.json"], capture_output=True).returncode == 0:
        raise RuntimeError("Fixture Task already exists; inspect instead of overwriting")
    main = {"task_id": main_id, "task_kind": "main", "team_id": team, "status": "in_progress", "room_id": room,
        "title": "Runtime recovery live acceptance", "dispatch_mode": "manual", "plan_type": "dag",
        "tasks": [{"task_id": task_id, "parent_task_id": main_id, "status": "assigned"}]}
    task = {"task_id": task_id, "main_task_id": main_id, "parent_task_id": main_id, "team_id": team,
        "task_kind": "subtask", "title": f"Run three stable steps across {SOURCE} to {TARGET}", "status": "assigned",
        "assigned_to": "@agent-a:recovery-1631.test", "room_id": room, "notifyTxn": "live-assignment-" + CASE,
        "recovery_fixture": {"oracle_version": 3, "oracle_url": "http://" + PREFIX + "-oracle:8091"}}
    if oracle_proof()["effects"]:
        raise RuntimeError("This Task already has independent effects; retain its proof")
    spec = f"""You are executing a bounded real runtime-recovery acceptance task {task_id} in team {team}.
Use your native TeamHarness taskflow MCP tool (Hermes: mcp__teamharness__taskflow; Codex: taskflow from the teamharness server). ACK with action=ack_task and confirm synced=true. Keep its stable task ID.
Pull acceptance.py from $AGENTTEAMS_STORAGE_PREFIX/teams/{team}/shared/tasks/{task_id}/acceptance.py into your task scratch directory using mc cp.
The acceptance.py helper owns the fixture:* operation identities and records both started and verified checkpoints itself. Invoke it as one native terminal tool call; do not pre-register a checkpoint for the helper or duplicate its operation IDs.
Use your native terminal tool to run python3 acceptance.py {task_id} {team}. Each invocation executes ONE real persisted side effect and commits its checkpoint through the real Controller.
Read the authoritative completed_steps. Never repeat a completed step or edit the fixture, ledger or checkpoint outside the helper.
In execution generation 1, run only step one. Then call the helper once more to verify PAUSED_AT_STEP_BOUNDARY and end your turn with a short progress report. Leave task status in_progress. Do not submit, wait, poll or request a new message.
When recovered in generation 2 or later, continue remaining steps two and three with the same helper, then use the native taskflow MCP tool with action=submit_task, status=SUCCESS, summary, 3 conclusions and workspace-relative deliverables shared/tasks/{task_id}/output/step-one.json, shared/tasks/{task_id}/output/step-two.json, shared/tasks/{task_id}/output/step-three.json. Do not edit meta.json or submit via raw HTTP/legacy filesync. Do not release execution just to end the turn; the adapter persists final delivery.
Outputs are durable in the task's shared object-storage output/ directory; pull them to your task output/ directory before submission. Do not create a new task or send messages to other members.
The external test driver will replace your container at a step boundary without sending any new Matrix message.
"""
    objects = {task_id + "/spec.md": spec, task_id + "/attempts.json": "[]",
        task_id + "/acceptance.py": (Path(__file__).parent / "fixtures/runtime_recovery_step.py").read_text()}
    for suffix, value in objects.items():
        docker("exec", "-i", CONTROLLER, "mc", "pipe", "agentteams/recovery-1631/teams/" + team + "/shared/tasks/" + suffix, text=value)
    docker("exec", "-i", CONTROLLER, "mc", "pipe", "agentteams/recovery-1631/teams/" + team
        + "/shared/recovery-fixtures/" + task_id + "/effects/.keep", text="")
    # Use the real conditional Task creation contract. mc pipe uses a multipart
    # ETag even for small input; it is not a substitute for Task metadata CAS.
    for meta in (main, task):
        api("PUT", "tasks/" + meta["task_id"] + "/meta?team=" + team, meta, headers={"If-None-Match": "*"})
    event = {"kind": "task_assigned", "main_task_id": main_id, "task_id": task_id,
             "delivery_id": task["notifyTxn"], "text": spec}
    code = """import json,os,pathlib,urllib.request,sys
token=pathlib.Path(os.environ['AGENTTEAMS_AUTH_TOKEN_FILE']).read_text().strip()
request=urllib.request.Request(os.environ['AGENTTEAMS_CONTROLLER_URL']+'/api/v1/coordination/events',data=sys.stdin.buffer.read(),headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
with urllib.request.urlopen(request,timeout=20) as response: print(response.read().decode())
"""
    receipt = docker("exec", "-i", PREFIX + "-worker-leader", "python3", "-c", code, text=json.dumps(event))
    print(json.dumps({"task_id": task_id, "main_task_id": main_id, "room_id": room, "notification": json.loads(receipt)}))


def refresh_controller():
    original = json.loads(docker("inspect", CONTROLLER))[0]
    if original["Config"]["Labels"].get("io.agentteams.recovery.owner") != "1631":
        raise RuntimeError("Only this fixture's labelled disposable Controller may be replaced")
    # Preserve the exact private bootstrap environment before stopping. A disk
    # or runtime error during replacement must remain safely retryable.
    backup = Path("/tmp/" + PREFIX + "-controller-inspect.private.json")
    with os.fdopen(os.open(backup, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as stream:
        json.dump(original, stream)
    args = ["run", "-d", "--name", CONTROLLER, "--network", NETWORK,
            "--label", "io.agentteams.recovery.owner=1631"]
    for port, bindings in original["NetworkSettings"]["Ports"].items():
        for binding in bindings or []:
            args.extend(["-p", binding["HostIp"] + ":" + binding["HostPort"] + ":" + port])
    for bind in original["HostConfig"]["Binds"]:
        args.extend(["-v", bind])
    for value in original["Config"]["Env"]:
        args.extend(["-e", value])
    docker("stop", "--timeout", "15", CONTROLLER)
    if json.loads(docker("inspect", CONTROLLER))[0]["State"]["Running"]:
        raise RuntimeError("Old disposable Controller is still running")
    docker("rm", CONTROLLER)
    docker(*args, CORE_IMAGE)
    docker("network", "connect", "agentteams-net", CONTROLLER)
    print(json.dumps({"controller_image": CORE_IMAGE, "replaced_only_owned_fixture": True}))


def native_sessions():
    code = '''import json,pathlib,sys,sqlite3
task_id=sys.argv[1]; home=pathlib.Path("/root/agentteams-fs/agents/agent-a")
events=pathlib.Path("/var/log/agentteams/workers/agent-a/events.jsonl")
rows=[json.loads(x) for x in events.read_text().splitlines()] if events.exists() else []
journal=home/"runtime/codex-bridge-state.json"
cache=json.loads(journal.read_text()).get("task_sessions",{}).get("recovery-team/"+task_id) if journal.exists() else None
hermes=[r for r in rows if r.get("event")=="task.recovery.session" and r.get("task_id")==task_id]
counts={}; db=home/".hermes/state.db"
if db.exists():
 with sqlite3.connect('file:'+str(db)+'?mode=ro',uri=True) as connection:
  for r in hermes:
   counts[r['session_id']]=connection.execute('select count(*) from messages where session_id=?',(r['session_id'],)).fetchone()[0]
print(json.dumps({"hermes":hermes,"hermes_message_counts":counts,
    "hermes_cache":[r for r in rows if r.get("event")=="task.recovery.cache" and r.get("task_id")==task_id],"codex":cache}))
'''
    return json.loads(docker("exec", PREFIX + "-worker-agent-a", "python3", "-c", code, TASK_ID))


def step_ledger(task):
    if task.get("recovery_fixture", {}).get("oracle_version") == 3:
        return oracle_proof()["effects"]
    if task.get("recovery_fixture", {}).get("oracle_version") != 2:
        return json.loads(docker("exec", CONTROLLER, "mc", "cat",
            "agentteams/recovery-1631/teams/recovery-team/shared/tasks/" + TASK_ID + "/attempts.json"))
    prefix = "agentteams/recovery-1631/teams/recovery-team/shared/recovery-fixtures/" + TASK_ID + "/effects/"
    listing = docker("exec", CONTROLLER, "mc", "ls", "--json", prefix)
    records = []
    for line in listing.splitlines():
        row = json.loads(line)
        if row.get("type") == "file" and row.get("key", "").endswith(".json"):
            records.append(json.loads(docker("exec", CONTROLLER, "mc", "cat", prefix + row["key"].rsplit("/", 1)[-1])))
    assert all(record["task_id"] == TASK_ID for record in records)
    return sorted(records, key=lambda record: record["checkpoint_version"])


def assert_step_boundaries(ledger, before_version):
    versions = [row["checkpoint_version"] for row in ledger]
    if versions[0] != 1 or versions[1] <= before_version or versions[2] != versions[1] + 2:
        raise ValueError("step boundary receipts must include a registered start and verified checkpoint per step")


def evidence():
    task = api("GET", "tasks/" + TASK_ID + "/meta?team=recovery-team")
    instances = []
    for name in (CONTROLLER, PREFIX + "-worker-agent-a", PREFIX + "-worker-leader"):
        item = json.loads(docker("inspect", name))[0]
        instances.append({"name": name, "id": item["Id"], "image": item["Config"]["Image"],
            "running": item["State"]["Running"], "pid": item["State"]["Pid"],
            "started_at": item["State"]["StartedAt"], "finished_at": item["State"]["FinishedAt"]})
    ledger = step_ledger(task)
    result = {"recorded_at": time.time(), "task": task, "ledger": ledger, "instances": instances, "native_sessions": native_sessions()}
    path = Path("/tmp/1631-live-evidence.json")
    path.write_text(json.dumps(result, indent=2))
    with Path("/tmp/1631-live-" + CASE + "-evidence.jsonl").open("a") as stream:
        stream.write(json.dumps(result) + "\n")
    print(json.dumps(result))


def cutover():
    task = api("GET", "tasks/" + TASK_ID + "/meta?team=recovery-team")
    cp = task.get("execution", {}).get("checkpoint", {})
    if task["status"] != "in_progress" or cp.get("completed_steps") != ["one"] or cp.get("remaining_steps") != ["two", "three"]:
        raise RuntimeError("Requires an actual durable step-one checkpoint on the active Task")
    name = PREFIX + "-worker-agent-a"
    old = json.loads(docker("inspect", name))[0]
    if old["Config"]["Image"] != HARNESS_IMAGES[SOURCE]:
        raise RuntimeError("The cutover requires this fixture's selected source image")
    started = int(time.time())
    if MODE == "restart":
        api("POST", "workers/agent-a/restart", {})
    else:
        api("PUT", "workers/agent-a", {"runtime": TARGET, "image": HARNESS_IMAGES[TARGET]})
    # No Matrix input is sent. Observe the Controller's actual replacement.
    deadline = time.monotonic() + 45
    replacement = None
    while time.monotonic() < deadline:
        result = subprocess.run(["docker", "inspect", name], capture_output=True, text=True)
        if result.returncode == 0:
            candidate = json.loads(result.stdout)[0]
            changed = candidate["Id"] != old["Id"] if MODE == "replace" else candidate["State"]["StartedAt"] != old["State"]["StartedAt"]
            if changed and candidate["State"]["Running"]:
                replacement = candidate
                break
        time.sleep(1)
    events = docker("events", "--since", str(started), "--until", str(int(time.time()) + 1),
        "--filter", "type=container", "--filter", "container=" + name, "--format", "{{json .}}")
    path = Path("/tmp/1631-live-" + CASE + "-cutover-events.jsonl")
    path.write_text(events + "\n")
    if replacement is None:
        raise RuntimeError("Replacement not observed; retained Docker event evidence")
    rows = [json.loads(row) for row in events.splitlines() if row]
    died = [row["timeNano"] for row in rows if row["Actor"]["ID"] == old["Id"] and row["Action"] == "die"]
    removed = [row["timeNano"] for row in rows if row["Actor"]["ID"] == old["Id"] and row["Action"] == "destroy"]
    created = [row["timeNano"] for row in rows if row["Actor"]["ID"] == replacement["Id"] and row["Action"] == "create"]
    starts = [row["timeNano"] for row in rows if row["Actor"]["ID"] == replacement["Id"] and row["Action"] == "start"]
    ordered = bool(died and removed and created and died[-1] <= removed[-1] <= created[0]) if MODE == "replace" else bool(died and starts and died[-1] <= starts[0])
    if not ordered:
        raise RuntimeError("Old process stop before target creation/start is not proved; inspect retained events")
    print(json.dumps({"old_container": old["Id"], "new_container": replacement["Id"],
        "events": str(path), "external_matrix_inputs_sent": 0, "task_continuation": "not yet verified"}))


def verify():
    """Read back real task, storage, native receipt and Matrix evidence."""
    task = api("GET", "tasks/" + TASK_ID + "/meta?team=recovery-team")
    execution, before = task["execution"], json.loads(Path("/tmp/1631-live-" + CASE + "-before-cutover.json").read_text())
    cp = execution["checkpoint"]
    assert task["task_id"] == before["task"]["task_id"] == TASK_ID
    assert before["task"]["status"] == "in_progress"
    assert before["task"]["execution"]["checkpoint"]["completed_steps"] == ["one"]
    assert task["status"] == "submitted" and cp["delivery"] == "delivered"
    assert cp["completed_steps"] == ["one", "two", "three"] and not cp["remaining_steps"]
    assert execution["instance_id"] != before["task"]["execution"]["instance_id"]
    assert execution["process_id"] != before["task"]["execution"]["process_id"]

    def stored(key):
        return subprocess.run(["docker", "exec", CONTROLLER, "mc", "cat", "agentteams/recovery-1631/" + key],
            check=True, capture_output=True).stdout

    ledger = step_ledger(task)
    assert [row["step"] for row in ledger] == ["one", "two", "three"]
    assert [row["generation"] for row in ledger] == [1, 2, 2]
    if task.get("recovery_fixture", {}).get("oracle_version") in {2, 3}:
        assert_step_boundaries(ledger, before["task"]["execution"]["checkpoint"]["version"])
    if task.get("recovery_fixture", {}).get("oracle_version") == 3:
        calls = oracle_proof()["calls"]
        assert len([row for row in calls if row["method"] == "POST"]) == 3
        assert all(row["accepted"] for row in calls), "Rejected effects/verification must be investigated"
    artifacts = cp["artifacts"] + [cp["result"]]
    for artifact in artifacts:
        data = stored(artifact["key"])
        assert len(data) == artifact["size"] and hashlib.sha256(data).hexdigest() == artifact["sha256"]
    report = stored("teams/recovery-team/" + task["result_path"])
    assert stored(cp["result"]["key"]) == report, "final reply differs from the authoritative submitted report"
    events = [json.loads(row) for row in Path("/tmp/1631-live-" + CASE + "-cutover-events.jsonl").read_text().splitlines() if row]
    old = before["instances"][1]["id"]
    died = next(row["timeNano"] for row in events if row["Action"] == "die" and row["Actor"]["ID"] == old)
    if MODE == "replace":
        destroyed = next(row["timeNano"] for row in events if row["Action"] == "destroy" and row["Actor"]["ID"] == old)
        created = next(row["timeNano"] for row in events if row["Action"] == "create" and row["Actor"]["ID"] != old)
        assert died <= destroyed <= created
        order = {"die": died, "destroy": destroyed, "create": created}
    else:
        started = next(row["timeNano"] for row in events if row["Action"] == "start" and row["Actor"]["ID"] == old)
        assert died <= started
        order = {"die": died, "start": started}
    code = '''import hashlib,json,pathlib,sys,urllib.parse
from agentteams_receipts import credentials,matrix_request,RECEIPT_TYPE
p=json.load(sys.stdin); request=matrix_request(*credentials(pathlib.Path("/root/agentteams-fs/agents/agent-a")))
room="/_matrix/client/v3/rooms/"+urllib.parse.quote(p["room"],safe="")
event=request("GET",room+"/event/"+urllib.parse.quote(p["event"],safe=""))
assert hashlib.sha256(event["content"]["body"].encode()).hexdigest()==p["sha256"]
rows=[]; cursor=None
while True:
 page=request("GET",room+"/messages?dir=b&limit=100"+("&from="+urllib.parse.quote(cursor,safe="") if cursor else ""))
 rows.extend(page["chunk"])
 if not page["chunk"] or not page.get("end") or page["end"]==cursor: break
 cursor=page["end"]
matches=[r["event_id"] for r in rows if hashlib.sha256(r.get("content",{}).get("body","").encode()).hexdigest()==p["sha256"]]
external=[r["event_id"] for r in rows if r.get("type")=="m.room.message" and p["stopped_ms"]<=r.get("origin_server_ts",0)<=event["origin_server_ts"] and r.get("sender")!="@agent-a:recovery-1631.test"]
assert matches==[p["event"]], matches
assert not external, external
receipts=[r for r in rows if r.get("type")==RECEIPT_TYPE and r.get("sender")=="@agent-a:recovery-1631.test" and r.get("content",{}).get("status")=="read"]
receipts=[r for r in receipts if r.get("origin_server_ts",0)<p["stopped_ms"]]
reads={r["content"]["m.relates_to"]["event_id"] for r in receipts}
sources=[r["event_id"] for r in rows if r["event_id"] in reads and r.get("origin_server_ts",0)<p["stopped_ms"] and p["task_id"] in r.get("content",{}).get("body","")]
assert sources, "source assignment has no delivered native read receipt"
print(json.dumps({"reply_event":p["event"],"report_copies":len(matches),"external_matrix_inputs_after_stop":external,"native_read_sources":sources,
    "read_receipt_events":[r["event_id"] for r in receipts if r["content"]["m.relates_to"]["event_id"] in sources]}))
'''
    matrix = json.loads(docker("exec", "-i", "-e", "PYTHONPATH=/opt/agentteams/scripts/lib", PREFIX + "-worker-agent-a", "python3", "-c", code,
        text=json.dumps({"room": task["room_id"], "event": cp["reply_event"], "sha256": cp["result"]["sha256"],
            "stopped_ms": died // 1000000, "task_id": TASK_ID})))
    old_source = next(row["Actor"]["Attributes"] for row in events if row["Action"] == "die" and row["Actor"]["ID"] == old)
    labels = next(row["Actor"]["Attributes"] for row in events if row["Action"] == ("create" if MODE == "replace" else "start") and
        ((row["Actor"]["ID"] != old) if MODE == "replace" else (row["Actor"]["ID"] == old)))
    native = native_sessions()
    if SOURCE == "hermes":
        selected = before["native_sessions"]["hermes"][-1]["session_id"]
        assert before["native_sessions"]["hermes_message_counts"].get(selected, 0) > 0, "new route still used a different warm native Agent"
    if TARGET == "hermes":
        selected = native["hermes"][-1]["session_id"]
        assert native["hermes_message_counts"].get(selected, 0) > 0, "recovered input was not persisted in the selected native session"
    if MODE == "restart" and SOURCE == "codex":
        cache = before["native_sessions"]["codex"]
        assert cache["runtime"] == "codex" and cache["expires_at"] > before["recorded_at"]
        digest = hashlib.sha256(json.dumps(before["task"]["execution"]["checkpoint"], sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        assert cache["checkpoint_digest"] == digest
        assert native["codex"]["thread_id"] == cache["thread_id"], "valid native session was not reused"
    if MODE == "restart" and SOURCE == "hermes":
        cache = before["native_sessions"]["hermes_cache"][-1]
        assert cache["expires_at"] > before["recorded_at"]
        digest = hashlib.sha256(json.dumps(before["task"]["execution"]["checkpoint"], sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        assert cache["checkpoint_digest"] == digest
        resumed = native["hermes"][-1]
        assert resumed["session_id"] == cache["session_id"] and resumed["reused"] is True
    controller = json.loads(docker("inspect", CONTROLLER))[0]
    current = json.loads(docker("inspect", PREFIX + "-worker-agent-a"))[0]
    assert current["Id"] == next(row["Actor"]["ID"] for row in events
        if row["Action"] == ("create" if MODE == "replace" else "start")
        and ((row["Actor"]["ID"] != old) if MODE == "replace" else row["Actor"]["ID"] == old)), "additional replacement requires separate evidence"
    assert current["Id"] in execution["instance_id"]
    sources = {"core": controller["Config"]["Labels"]["io.agentteams.recovery.core-sha"],
        "shared_contract": labels["io.agentteams.recovery.core-sha"],
        SOURCE: old_source["io.agentteams.recovery.harness-sha"], TARGET: labels["io.agentteams.recovery.harness-sha"]}
    proof = {"task_id": TASK_ID, "case": CASE, "scope": f"one real {SOURCE}-to-{TARGET} in-progress continuation", "mode": MODE,
        "fixture_prefix": PREFIX, "authoritative_report_verified": True,
        "sources": sources, "native_recovery_sessions": native,
        "generation_before": before["task"]["execution"]["generation"], "generation_after": execution["generation"],
        "checkpoint_before": before["task"]["execution"]["checkpoint"]["version"], "checkpoint_after": cp["version"],
        "execution": execution, "ledger": ledger, "artifacts_verified": artifacts,
        "docker_order_ns": order, "matrix": matrix}
    path = Path("/tmp/1631-live-" + CASE + "-verified.json")
    path.write_text(json.dumps(proof, indent=2))
    print(json.dumps({"passed": True, "evidence": str(path), **proof}))


def setup(*, restore=False):
    if STATE.exists() and not restore:
        raise RuntimeError("This disposable setup already has a state file; inspect it instead of replacing it")
    previous = state() if restore else None
    if restore and previous["prefix"] != PREFIX:
        raise RuntimeError("Only this fixture's preserved volume may be restored")
    if not os.getenv("AGENTTEAMS_LLM_API_KEY"):
        raise RuntimeError("A real AGENTTEAMS_LLM_API_KEY is required")
    if subprocess.run(["docker", "inspect", CONTROLLER], capture_output=True).returncode == 0:
        raise RuntimeError("The disposable container name already exists; refusing to replace it")
    password = previous["admin_password"] if restore else secrets.token_urlsafe(24)
    registration = secrets.token_urlsafe(24)
    # mc alias set currently treats a leading '-' credential as a flag.
    minio_secret = "recovery-" + secrets.token_urlsafe(24)
    env = {
        "AGENTTEAMS_KUBE_MODE": "embedded", "AGENTTEAMS_RUNTIME": "docker",
        "AGENTTEAMS_WORKER_BACKEND": "docker", "AGENTTEAMS_DOCKER_NETWORK": NETWORK,
        "AGENTTEAMS_RESOURCE_PREFIX": PREFIX + "-", "AGENTTEAMS_RESOURCE_AUTOPREFIX": "true",
        "AGENTTEAMS_CONTROLLER_NAME": PREFIX, "AGENTTEAMS_MANAGER_ENABLED": "false",
        "AGENTTEAMS_ADMIN_USER": "recovery-admin", "AGENTTEAMS_ADMIN_PASSWORD": password,
        "AGENTTEAMS_REGISTRATION_TOKEN": registration, "AGENTTEAMS_MINIO_USER": "recovery-minio",
        "AGENTTEAMS_MINIO_PASSWORD": minio_secret, "AGENTTEAMS_FS_ENDPOINT": "http://127.0.0.1:9000",
        "AGENTTEAMS_FS_ACCESS_KEY": "recovery-minio", "AGENTTEAMS_FS_SECRET_KEY": minio_secret,
        "AGENTTEAMS_FS_BUCKET": "recovery-1631", "AGENTTEAMS_STORAGE_PREFIX": "agentteams/recovery-1631",
        "AGENTTEAMS_LLM_PROVIDER": "qwen", "AGENTTEAMS_DEFAULT_MODEL": "qwen3.6-plus",
        "AGENTTEAMS_DEFAULT_WORKER_RUNTIME": "codex", "AGENTTEAMS_MATRIX_E2EE": "0",
        "AGENTTEAMS_CONTROLLER_URL": "http://" + CONTROLLER + ":8090",
        "AGENTTEAMS_AI_GATEWAY_URL": "http://" + CONTROLLER + ":8080",
        "AGENTTEAMS_MATRIX_URL": "http://127.0.0.1:6167",
        "AGENTTEAMS_MATRIX_DOMAIN": "recovery-1631.test",
        "AGENTTEAMS_MATRIX_APPSERVICE_AS_TOKEN": secrets.token_urlsafe(32),
        "AGENTTEAMS_MATRIX_APPSERVICE_HS_TOKEN": secrets.token_urlsafe(32),
    }
    private = PRIVATE_BOOTSTRAP
    if restore and private.exists():
        env = json.loads(private.read_text())
    with os.fdopen(os.open(private, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as stream:
        json.dump(env, stream)
    # This Mac's default Docker pools are exhausted by existing installations.
    # Use the separately inspected, non-overlapping disposable test subnet.
    if not restore:
        docker("network", "create", "--subnet", os.getenv("RECOVERY_E2E_SUBNET", "10.231.163.0/24"), NETWORK)
        docker("volume", "create", PREFIX + "-data")
    else:
        docker("volume", "inspect", PREFIX + "-data")
        docker("network", "inspect", NETWORK)
    args = ["run", "-d", "--name", CONTROLLER, "--network", NETWORK,
            "--label", "io.agentteams.recovery.owner=1631", "-p", "127.0.0.1::8090",
            "-p", "127.0.0.1::6167", "-p", "127.0.0.1::8080",
            "-v", PREFIX + "-data:/data", "-v", "/var/run/docker.sock:/var/run/docker.sock",
            "-e", "AGENTTEAMS_LLM_API_KEY"]
    for key, value in env.items():
        args.extend(["-e", key + "=" + value])
    docker(*args, CORE_IMAGE)
    ports = {}
    for name, port in {"controller": 8090, "matrix": 6167, "gateway": 8080}.items():
        host_port = docker("port", CONTROLLER, str(port) + "/tcp").rsplit(":", 1)[1]
        ports[name] = "http://127.0.0.1:" + host_port
    data = {"prefix": PREFIX, "core_image": CORE_IMAGE, "ports": ports,
            "admin_user": "recovery-admin", "admin_password": password,
            "started_at": time.time(), "real_model_configured": True}
    flags = os.O_WRONLY | os.O_CREAT | (os.O_TRUNC if restore else os.O_EXCL)
    with os.fdopen(os.open(STATE, flags, 0o600), "w") as stream:
        json.dump(data, stream, indent=2)
    print(json.dumps({"container": CONTROLLER, "ports": ports, "state": str(STATE)}))


def status():
    data = state()
    result = {}
    for name, path in {"controller": "/healthz", "matrix": "/_matrix/client/versions"}.items():
        try:
            port = {"controller": 8090, "matrix": 6167}[name]
            host_port = docker("port", CONTROLLER, str(port) + "/tcp").rsplit(":", 1)[1]
            with urllib.request.urlopen("http://127.0.0.1:" + host_port + path, timeout=5) as response:
                result[name] = response.status
        except OSError as error:
            result[name] = type(error).__name__
    try:
        result["workers"] = json.loads(agt("get", "workers", "-o", "json"))
    except RuntimeError:
        result["workers"] = "not-ready"
    print(json.dumps(result, ensure_ascii=False))


def members():
    # This creates resources only through this fixture's Controller CLI.
    # The existing installation and its task graph are never selected.
    for name, runtime in (("agent-a", SOURCE), ("leader", "hermes")):
        manifest = {"apiVersion": "agentteams.io/v1beta1", "kind": "Worker",
            "metadata": {"name": name}, "spec": {"workerName": name, "runtime": runtime,
                "model": "qwen3.6-plus", "image": HARNESS_IMAGES[runtime]}}
        if runtime == "codex":
            # The fixture runs only inside our disposable container; its
            # deterministic step helper needs the Controller/storage network.
            manifest["spec"]["env"] = {"CODEX_SANDBOX_MODE": "danger-full-access"}
        agt("apply", "-f", "/dev/stdin", text=json.dumps(manifest))
    team = {"apiVersion": "agentteams.io/v1beta1", "kind": "Team", "metadata": {"name": "recovery-team"},
        "spec": {"teamName": "recovery-team", "admin": {"name": "recovery-admin"},
            "workerMembers": [{"name": "leader", "role": "team_leader"}, {"name": "agent-a", "role": "worker"}]}}
    agt("apply", "-f", "/dev/stdin", text=json.dumps(team))
    print("Created isolated Agent A and Leader; model/task execution not yet verified")


if __name__ == "__main__":
    {"setup": setup, "restore": lambda: setup(restore=True), "status": status, "members": members, "prepare": prepare, "oracle": oracle, "tracing": tracing, "seed": seed,
     "refresh-controller": refresh_controller, "evidence": evidence, "cutover": cutover, "verify": verify}[sys.argv[1]]()
