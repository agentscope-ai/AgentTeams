"""Opt-in #1631 live acceptance; only the named disposable installation.

Run with a real AGENTTEAMS_LLM_API_KEY. Never discovers or changes a default
installation. Runtime/model execution evidence is separate from setup readiness.
"""
import json
import base64
from datetime import datetime
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

DRIVER_SHA256 = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
PREFIX = os.getenv("RECOVERY_E2E_PREFIX", "recovery-1631-live-20261116")
if not re.fullmatch(r"recovery-1631-live-\d{8}(?:-[a-z0-9]+)?", PREFIX):
    raise ValueError("Use a dated, disposable recovery-1631-live-YYYYMMDD prefix")
CONTROLLER = PREFIX + "-controller"
NETWORK = PREFIX + "-net"
STATE = Path("/tmp/recovery-1631-e2e-state.json" if PREFIX == "recovery-1631-live-20261116" else "/tmp/" + PREFIX + "-state.json")
PRIVATE_BOOTSTRAP = Path("/tmp/recovery-1631-bootstrap.private.json" if PREFIX == "recovery-1631-live-20261116" else "/tmp/" + PREFIX + "-bootstrap.private.json")
CORE_IMAGE = os.getenv("RECOVERY_E2E_CORE_IMAGE", "recovery-1631/controller:local-20261117053228")
HARNESS_IMAGES = {"codex": "recovery-1631/codex:local-20261117013200",
                  "hermes": "recovery-1631/hermes:local-20261117053547",
                  "deepseek-harness": "recovery-1631/deepseek:local-20261117053547"}
image_overrides = json.loads(os.getenv("RECOVERY_E2E_HARNESS_IMAGES", "{}"))
if (not isinstance(image_overrides, dict)
        or not set(image_overrides) <= {"codex", "hermes", "qwenpaw", "deepseek-harness", "openclaw", "openhuman"}
        or any(not isinstance(image, str) or not image.strip() for image in image_overrides.values())):
    raise ValueError("Explicit recovery images must name supported Harnesses and nonempty image references")
HARNESS_IMAGES.update(image_overrides)
SOURCE, TARGET = os.getenv("RECOVERY_E2E_FROM", "codex"), os.getenv("RECOVERY_E2E_TO", "hermes")
MODE = os.getenv("RECOVERY_E2E_MODE", "replace")
OPERATOR_HANDOFF = os.getenv("RECOVERY_E2E_HANDOFF") == "1"
# Old case 08 used a journal-only image. Its original verifier stays opt-in.
LEGACY_CODEX = os.getenv("RECOVERY_E2E_LEGACY_CODEX") == "1"
RETURN_TO_SOURCE = os.getenv("RECOVERY_E2E_RETURN") == "1"
FAULT = os.getenv("RECOVERY_E2E_FAULT", "")
EXPIRE_HERMES_SESSION = os.getenv("RECOVERY_E2E_EXPIRE_HERMES_SESSION") == "1"
ABRUPT_RESTART = os.getenv("RECOVERY_E2E_ABRUPT_RESTART") == "1"
OPENHUMAN_PREAPPROVED = os.getenv("RECOVERY_E2E_OPENHUMAN_PREAPPROVED") == "1"
if FAULT not in {"", "after-effect"}:
    raise ValueError("Unknown recovery fault injection")
if SOURCE not in HARNESS_IMAGES or TARGET not in HARNESS_IMAGES or MODE not in {"replace", "restart"}:
    raise ValueError("Select a packaged recovery Harness and replace/restart mode")
if (MODE == "restart") != (SOURCE == TARGET):
    raise ValueError("Restart keeps the Harness; replacement switches between packaged Harnesses")
if OPERATOR_HANDOFF and MODE != "replace":
    raise ValueError("The operator handoff requires a target Harness")
if RETURN_TO_SOURCE and (not OPERATOR_HANDOFF or (SOURCE, TARGET) != ("qwenpaw", "hermes") or FAULT):
    raise ValueError("The stale QwenPaw session regression needs two operator handoffs via Hermes")
if EXPIRE_HERMES_SESSION and (SOURCE != "hermes" or MODE != "restart" or FAULT):
    raise ValueError("Session expiry requires a Hermes restart at a committed step boundary")
if ABRUPT_RESTART and (MODE != "restart" or FAULT or EXPIRE_HERMES_SESSION):
    raise ValueError("Abrupt restart requires a same-Harness committed step boundary")
if OPENHUMAN_PREAPPROVED and "openhuman" not in {SOURCE, TARGET}:
    raise ValueError("The OpenHuman fixture's authorization requires OpenHuman")
CASE = os.getenv("RECOVERY_E2E_CASE", "01")
if not re.fullmatch(r"[0-9]{2,3}", CASE):
    raise ValueError("RECOVERY_E2E_CASE must be two or three decimal digits")
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
        desired = {**env, "MATRIX_HOME_ROOM": worker["roomID"], "RECOVERY_E2E_PREFIX": PREFIX}
        if name == "leader":
            # Native review remains enabled. Its tool-status bubbles would
            # otherwise inject extra room messages into the no-input test.
            desired["HERMES_TOOL_PROGRESS_MODE"] = "off"
        if desired != env:
            api("PUT", "workers/" + name, {"env": desired})
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
    ready_code = """import datetime,json,pathlib,sys
started=datetime.datetime.fromisoformat(sys.argv[1].replace('Z','+00:00'))
path=pathlib.Path('/var/log/agentteams/workers/agent-a/events.jsonl')
rows=[json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []
print(json.dumps(any(row.get('event')=='runtime.ready' and datetime.datetime.fromisoformat(row['timestamp'])>started for row in rows)))
"""
    if not json.loads(docker("exec", PREFIX + "-worker-agent-a", "python3", "-c", ready_code, actual["State"]["StartedAt"])):
        raise RuntimeError("Wait for native runtime.ready after this container start before assignment")
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
        "recovery_fixture": {"oracle_version": 4, "oracle_url": "http://" + PREFIX + "-oracle:8091"}}
    if FAULT == "after-effect":
        task["recovery_fixture"]["pause_after_effect"] = "one"
    if oracle_proof()["effects"]:
        raise RuntimeError("This Task already has independent effects; retain its proof")
    paused_turn = ("Once the helper commits the step, end the turn with a short progress reply. Leave the Task "
                   "in_progress and make no further tool calls. The driver requests the cutover.")
    continuation = "From generation 2 onward, run the helper for each remaining step."
    if RETURN_TO_SOURCE:
        task["recovery_fixture"]["return_to_source"] = True
        continuation = ("In generation 2, run the helper only for step two. " + paused_turn
                        + "\nIn generation 3, run the helper for step three.")
    spec = f"""You are executing a bounded real runtime-recovery acceptance task {task_id} in team {team}.
Use your native TeamHarness taskflow MCP tool (Hermes: mcp__teamharness__taskflow; Codex: taskflow from the teamharness server). ACK with action=ack_task and confirm synced=true. Keep its stable task ID.
Unless your active Harness is OpenHuman, use mc cp to pull acceptance.py from $AGENTTEAMS_STORAGE_PREFIX/teams/{team}/shared/tasks/{task_id}/acceptance.py into your task scratch directory.
The acceptance.py helper owns the fixture:* operation identities and records both started and verified checkpoints itself. Invoke it as one native terminal tool call; do not pre-register a checkpoint for the helper or duplicate its operation IDs.
Use your native terminal tool to run python3 acceptance.py {task_id} {team}. If your active Harness is OpenHuman, use its Shell tool with the single command agentteams-recovery-step {task_id} {team} instead. Use the command directly without cd, an interpreter, or a shell prefix. This is the same acceptance helper installed in the isolated test image.
Each invocation executes ONE real persisted side effect and commits its checkpoint through the real Controller.
Read the authoritative completed_steps. Never repeat a completed step or edit the fixture, ledger or checkpoint outside the helper.
In generation 1, run the helper only for step one. {paused_turn}
{continuation}
When all three steps are complete, use the native taskflow MCP tool with action=submit_task, status=SUCCESS, summary, 3 conclusions and workspace-relative deliverables shared/tasks/{task_id}/output/step-one.json, shared/tasks/{task_id}/output/step-two.json, shared/tasks/{task_id}/output/step-three.json. Do not edit meta.json or submit via raw HTTP/legacy filesync. Do not release execution just to end the turn; the adapter persists final delivery.
Outputs are durable in the task's shared object-storage output/ directory. On OpenHuman, submit the existing relative deliverable paths through TeamHarness as soon as all three steps are committed. The helper has already persisted and verified these files. Do not use native file_read or a shell probe on the shared paths before submission. On the other Harnesses, pull the outputs to your task output/ directory before submission. Do not create a new task or send messages to other members.
The external test driver will replace your container at a step boundary without sending any new Matrix message.
"""
    if FAULT == "after-effect":
        spec += "\nFault scenario: in generation 1 the helper deliberately waits after the external effect, before its artifact/checkpoint. While that invocation runs, do not invoke it again, cancel it or manually finish its checkpoint. The driver will terminate your container. In generation 2 or later, use the same helper to verify the external outcome before continuing; never re-execute the unknown effect.\n"
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


def native_sessions(previous_qwen_ref="", *, verify_qwen_route=False):
    runtime = api("GET", "workers/agent-a").get("runtime")
    task = api("GET", "tasks/"+TASK_ID+"/meta?team=recovery-team")
    routes = []
    # Cutover validates the source SDK session against its shared descriptor.
    # Trace readback is mandatory when selecting the final QwenPaw route.
    if runtime == "qwenpaw" and verify_qwen_route:
        traces = api("GET", "traces?"+urllib.parse.urlencode({"worker": "agent-a", "task": TASK_ID, "runtime": "qwenpaw", "limit": 100, "hours": 1}))
        for row in traces.get("data", []):
            meta = row.get("metadata") or {}
            if (meta.get("agentteams.task.id") == TASK_ID
                    and meta.get("agentteams.room.id") == task["room_id"]
                    and str(meta.get("agentteams.execution.generation")) == str(task["execution"]["generation"])
                    and meta.get("agentteams.runtime") == "qwenpaw"):
                routes.append({"trace_id": row["id"], "session_id": meta["agentteams.runtime.session_id"],
                    "generation": task["execution"]["generation"], "task_id": TASK_ID, "runtime": "qwenpaw"})
    code = '''import hashlib,json,pathlib,sys,sqlite3,os,subprocess
task_id=sys.argv[1]; home=pathlib.Path("/root/agentteams-fs/agents/agent-a")
events=pathlib.Path("/var/log/agentteams/workers/agent-a/events.jsonl")
rows=[json.loads(x) for x in events.read_text().splitlines()] if events.exists() else []
journal=home/"runtime/codex-bridge-state.json"
cache=json.loads(journal.read_text()).get("task_sessions",{}).get("recovery-team/"+task_id) if journal.exists() else None
hermes=[r for r in rows if r.get("event")=="task.recovery.session" and r.get("task_id")==task_id]
counts={}; db=home/".hermes/state.db"
hermes_shared=None
try:
 from hermes_matrix.session_cache import session_fingerprint
except ImportError:
 pass # Historical SDK images keep their original evidence boundary.
else:
 import subprocess
 from gateway.config import GatewayConfig
 from gateway.session import SessionStore
 work_key=hashlib.sha256(json.dumps(['agents/agent-a','task','recovery-team',task_id],ensure_ascii=False,separators=(',',':')).encode()).hexdigest()
 object_key='agents/agent-a/runtime/sessions/hermes/'+work_key+'.json'
 fetched=subprocess.run(['mc','cat',os.environ['AGENTTEAMS_STORAGE_PREFIX'].rstrip('/')+'/'+object_key],capture_output=True,text=True)
 descriptor=json.loads(fetched.stdout) if fetched.returncode==0 else None
 hermes_shared={'cache':descriptor,'native_digest':None,'message_count':0,'tool_count':0,'task_mentioned':False}
 if descriptor:
  store=SessionStore(home/'.hermes/sessions',GatewayConfig())
  entry=next((s for s in store.list_sessions() if s.session_id==descriptor['native_ref']),None)
  if entry:
   fingerprint=session_fingerprint(store,entry)
   native_db=store._db_for_key(entry.session_key)
   tip=native_db.get_compression_tip(entry.session_id) or entry.session_id
   messages=native_db.get_messages(tip,include_ancestors=True)
   hermes_shared.update(native_digest=fingerprint,message_count=len(messages),tool_count=sum(r.get('role')=='tool' for r in messages),task_mentioned=task_id in json.dumps(messages))
  config=__import__('yaml').safe_load((home/'runtime/runtime.yaml').read_text())
  hermes_shared['config_generation']=config['metadata'].get('generation',0)
if db.exists():
 with sqlite3.connect('file:'+str(db)+'?mode=ro',uri=True) as connection:
  for r in hermes:
   counts[r['session_id']]=connection.execute('select count(*) from messages where session_id=?',(r['session_id'],)).fetchone()[0]
qwen_dir=home/".qwenpaw/workspaces/default/sessions"
qwen_cache=qwen_dir/".agentteams-task-recovery"
digest=lambda value: hashlib.sha256(json.dumps(value,sort_keys=True,separators=(',',':')).encode()).hexdigest()
qwen=json.loads(qwen_cache.read_text()).get(digest(['recovery-team',task_id])) if qwen_cache.exists() else None
# Read the common descriptor independently from native session contents.
work_key=hashlib.sha256(json.dumps(['agents/agent-a','task','recovery-team',task_id],ensure_ascii=False,separators=(',',':')).encode()).hexdigest()
object_key='agents/agent-a/runtime/sessions/qwenpaw/'+work_key+'.json'
fetched=subprocess.run(['mc','cat',os.environ['AGENTTEAMS_STORAGE_PREFIX'].rstrip('/')+'/'+object_key],capture_output=True,text=True)
qwen_descriptor=json.loads(fetched.stdout) if fetched.returncode==0 else None
qwen_shared={'cache':qwen_descriptor,'native':[],'routes':json.loads(sys.argv[3])}
if sys.argv[4]=='qwenpaw':
 import asyncio
 from qwenpaw.app.chats.session import SafeJSONSession,session_filename,sanitize_filename
 config=__import__('yaml').safe_load((home/'runtime/runtime.yaml').read_text())
 qwen_shared['config_generation']=config['metadata'].get('generation',0)
 store=SafeJSONSession(str(qwen_dir))
 refs={r['session_id'] for r in qwen_shared['routes']}
 if sys.argv[5]: refs.add(sys.argv[5])
 if qwen_descriptor: refs.add(qwen_descriptor['native_ref'])
 for ref in sorted(refs):
  # Avoid the SDK's missing-file directory creation or legacy migration while
  # probing. Existing rows are still decoded through the real SDK reader.
  path=qwen_dir/sanitize_filename('agentteams_matrix')/session_filename(ref,sys.argv[2])
  if not path.is_file(): continue
  native=asyncio.run(store.get_session_state_dict(session_id=ref,user_id=sys.argv[2],channel='agentteams_matrix',allow_not_exist=False))
  def nodes(value):
   if isinstance(value,dict):
    yield value
    for child in value.values(): yield from nodes(child)
   elif isinstance(value,list):
    for child in value: yield from nodes(child)
  facts=list(nodes(native))
  qwen_shared['native'].append({'session_id':ref,'native_digest':digest(native),
   'agent_state_present':bool(native.get('agent')),'task_mentioned':task_id in json.dumps(native),
   'message_count':sum(r.get('role') in {'user','assistant','tool','system'} for r in facts),
   'tool_count':sum(r.get('role')=='tool' or r.get('type') in {'tool_use','tool_result'} for r in facts)})
openclaw_file=home/"runtime/openclaw-task-sessions.json"
openclaw_cache=json.loads(openclaw_file.read_text()).get(digest(['recovery-team',task_id])) if openclaw_file.exists() else None
openhuman_file=home/"runtime/openhuman-task-sessions"/(digest(['recovery-team',task_id])+'.json')
openhuman_cache=json.loads(openhuman_file.read_text()) if openhuman_file.exists() else None
openhuman_descriptor=None
openhuman_config_generation=None
if sys.argv[4]=="openhuman":
 key='agents/agent-a/runtime/sessions/openhuman/'+work_key+'.json'
 fetched=subprocess.run(['mc','cat',os.environ['AGENTTEAMS_STORAGE_PREFIX'].rstrip('/')+'/'+key],capture_output=True,text=True)
 openhuman_descriptor=json.loads(fetched.stdout) if fetched.returncode==0 else None
 config=__import__('yaml').safe_load((home/'runtime/runtime.yaml').read_text())
 openhuman_config_generation=config['metadata'].get('generation',0)
openhuman_authority=hashlib.sha256(json.dumps([os.getenv('AGENTTEAMS_CONTROLLER_URL'),os.getenv('AGENTTEAMS_WORKER_NAME')],ensure_ascii=False,separators=(',',':')).encode()).hexdigest()
qwen_native=[]
if qwen:
 for path in (qwen_dir/"agentteams_matrix").glob('*.json'):
  native=json.loads(path.read_text())
  if digest(native)==qwen['native_digest']:
   qwen_native.append({'file':path.name,'native_digest':digest(native),'agent_state_present':bool(native.get('agent')),'task_mentioned':task_id in json.dumps(native)})
print(json.dumps({"hermes":hermes,"hermes_message_counts":counts,"hermes_shared":hermes_shared,
    "hermes_cache":[r for r in rows if r.get("event")=="task.recovery.cache" and r.get("task_id")==task_id],"codex":cache,
    "qwenpaw":qwen,"qwenpaw_native":qwen_native,"qwenpaw_shared":qwen_shared,"openclaw_cache":openclaw_cache,
    "openhuman":{"cache":openhuman_cache,"authority":openhuman_authority,"descriptor":openhuman_descriptor,"config_generation":openhuman_config_generation}}))
'''
    interpreter = {"hermes": "/opt/venv/hermes/bin/python", "qwenpaw": "/opt/venv/qwenpaw/bin/python"}.get(runtime, "python3")
    result = json.loads(docker("exec", PREFIX + "-worker-agent-a", interpreter, "-c", code, TASK_ID, task["room_id"], json.dumps(routes), runtime, previous_qwen_ref))
    if runtime == "codex" and not LEGACY_CODEX and task.get("execution"):
        script = (Path(__file__).parent / "fixtures/runtime_recovery_codex.py").read_text()
        result["codex_shared"] = json.loads(docker("exec", "-i", PREFIX + "-worker-agent-a",
            "/opt/venv/bin/python", "-", TASK_ID, "recovery-team", task["room_id"],
            str(task["execution"]["generation"]), text=script))
    if runtime == "deepseek-harness" and task.get("execution"):
        name = PREFIX + "-worker-agent-a"
        home = "/root/agentteams-fs/agents/agent-a"
        script = (Path(__file__).parent / "fixtures/runtime_recovery_deepseek.cjs").read_text()
        key = hashlib.sha256(json.dumps(["agents/agent-a", "task", "recovery-team", TASK_ID],
            ensure_ascii=False, separators=(",", ":")).encode()).hexdigest() + ".json"
        base = "agentteams/recovery-1631/agents/agent-a/runtime/sessions/deepseek-harness/"
        listing = docker("exec", CONTROLLER, "mc", "ls", "--json", base)
        cache = (json.loads(docker("exec", CONTROLLER, "mc", "cat", base + key))
                 if any(json.loads(line).get("key") == key for line in listing.splitlines()) else None)
        result["deepseek"] = {"native": json.loads(docker("exec", "-i", name, "node", "-",
            home, TASK_ID, str(task["execution"]["generation"]), text=script)), "cache": cache}
    if runtime == "openclaw":
        # Use the pinned SDK's actual SQLite APIs, not a guessed session filename.
        key = hashlib.sha256(json.dumps(["agents/agent-a", "task", "recovery-team", TASK_ID],
            ensure_ascii=False, separators=(",", ":")).encode()).hexdigest() + ".json"
        base = "agentteams/recovery-1631/agents/agent-a/runtime/sessions/openclaw/"
        listing = docker("exec", CONTROLLER, "mc", "ls", "--json", base)
        shared = (json.loads(docker("exec", CONTROLLER, "mc", "cat", base + key))
                  if any(json.loads(line).get("key") == key for line in listing.splitlines()) else None)
        probe = '''import fs from "node:fs";
import {createHash} from "node:crypto";
import {getSessionEntryAsync,listSessionEntries,loadTranscriptEventsSync} from "openclaw/plugin-sdk/session-store-runtime";
const digest=(value)=>createHash("sha256").update(JSON.stringify(value)).digest("hex");
const [task, generation, sharedRaw]=process.argv.slice(1);
const home="/root/agentteams-fs/agents/agent-a";
const file=home+"/runtime/openclaw-task-sessions.json";
const key=digest(["recovery-team",task]);
const cache=fs.existsSync(file)?JSON.parse(fs.readFileSync(file,"utf8"))[key]:null;
const shared=JSON.parse(sharedRaw);
const agentId=shared?.native_ref.split(":")[1]??"main";
const generationMarker=JSON.stringify('"generation": '+generation).slice(1,-1);
async function read(sessionKey, legacy=false) {
 if(!sessionKey) return null;
 const agentId=sessionKey.split(":")[1];
 const entry=await getSessionEntryAsync({agentId,sessionKey,readConsistency:"latest"});
 if(!entry) return null;
 const events=loadTranscriptEventsSync({agentId,sessionKey,sessionId:entry.sessionId});
 const hasRecoveryTurn=events.some(event=>{
  const content=JSON.stringify(event);
  return content.includes(task)&&content.includes("Recover your existing Task from the authoritative task system")
    &&content.includes(generationMarker);
 });
 return {session_key:sessionKey,session_id:entry.sessionId,
  native_digest:digest(legacy?{entry,events}:{sessionId:entry.sessionId,events}),
  message_count:events.length,task_mentioned:JSON.stringify(events).includes(task),has_recovery_turn:hasRecoveryTurn};
}
const sessions=[];
for(const {sessionKey} of listSessionEntries({agentId,readOnly:true})){
 if(!sessionKey.startsWith("agent:"+agentId+":agentteams-task:"+key+":")) continue;
 const native=await read(sessionKey);
 if(native?.has_recovery_turn) sessions.push(native);
}
console.log("AGENTTEAMS_NATIVE_PROBE="+JSON.stringify({cache:cache??null,native:await read(cache?.session_key,true),
 shared_cache:shared,shared_native:await read(shared?.native_ref),turn_sessions:sessions}));
'''
        output = docker("exec", "-w", "/opt/openclaw", PREFIX + "-worker-agent-a",
            "node", "--input-type=module", "-e", probe, TASK_ID, str((task.get("execution") or {}).get("generation", 0)), json.dumps(shared))
        result["openclaw"] = json.loads(next(line.removeprefix("AGENTTEAMS_NATIVE_PROBE=")
            for line in output.splitlines() if line.startswith("AGENTTEAMS_NATIVE_PROBE=")))
        if shared:
            result["openclaw"]["config_generation"] = int(docker("exec", PREFIX + "-worker-agent-a", "python3", "-c",
                "import yaml; print(yaml.safe_load(open('/root/agentteams-fs/agents/agent-a/runtime/runtime.yaml'))['metadata'].get('generation', 0))"))
    return result


def step_ledger(task):
    if task.get("recovery_fixture", {}).get("oracle_version") in {3, 4}:
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


def assert_effect_authorization(proof, effect):
    at_step = lambda row: all(row[k] == effect[k] for k in ("team_id", "task_id", "step"))
    intents = [r for r in proof["intents"] if at_step(r)]
    calls = [r for r in proof["calls"] if at_step(r)]
    registers = [r for r in calls if r["method"] == "REGISTER" and r["accepted"]]
    posts = [r for r in calls if r["method"] == "POST" and r["accepted"]]
    assert len(intents) == len(registers) == len(posts) == 1
    intent, register, post = intents[0], registers[0], posts[0]
    assert all(intent[k] == effect[k] for k in ("instance_id", "process_id", "generation", "checkpoint_version"))
    assert register["process_id"] == post["process_id"] == effect["process_id"]
    assert register["at_ns"] < post["at_ns"] <= effect["executed_at_ns"]
    for observation in (r for r in calls if r.get("verification") == "never_authorized"):
        assert observation["method"] == "GET" and observation["accepted"] and observation["at_ns"] < register["at_ns"]


def fixture_tool_processes():
    code = '''import pathlib,json,sys
rows=[]
for path in pathlib.Path('/proc').glob('[0-9]*/cmdline'):
 try:
  args=path.read_bytes().decode().split('\\0')
  if sys.argv[1] in args and any(pathlib.PurePosixPath(arg).name in {'acceptance.py','agentteams-recovery-step'} for arg in args):
   rows.append({'pid':int(path.parent.name),'start_ticks':path.with_name('stat').read_text().split()[21]})
 except FileNotFoundError:pass
print(json.dumps(rows))
'''
    return json.loads(docker("exec", PREFIX + "-worker-agent-a", "python3", "-c", code, TASK_ID))


def evidence(*, emit=True):
    task = api("GET", "tasks/" + TASK_ID + "/meta?team=recovery-team")
    instances = []
    for name in (CONTROLLER, PREFIX + "-worker-agent-a", PREFIX + "-worker-leader"):
        item = json.loads(docker("inspect", name))[0]
        instances.append({"name": name, "id": item["Id"], "image": item["Config"]["Image"],
            "image_id": item["Image"],
            "running": item["State"]["Running"], "pid": item["State"]["Pid"],
            "started_at": item["State"]["StartedAt"], "finished_at": item["State"]["FinishedAt"]})
    ledger = step_ledger(task)
    result = {"recorded_at": time.time(), "task": task, "ledger": ledger, "instances": instances, "native_sessions": native_sessions()}
    if FAULT == "after-effect":
        result["tool_processes"] = fixture_tool_processes()
    path = Path("/tmp/1631-live-evidence.json")
    path.write_text(json.dumps(result, indent=2))
    with Path("/tmp/1631-live-" + CASE + "-evidence.jsonl").open("a") as stream:
        stream.write(json.dumps(result) + "\n")
    if emit:
        print(json.dumps(result))
    return result


def source_read_receipt(task):
    # Check the server before stopping the source. Task discovery can run
    # before the independently queued Matrix assignment has been delivered.
    code = '''import json,pathlib,sys,urllib.parse
from agentteams_receipts import credentials,matrix_request,RECEIPT_TYPE
p=json.load(sys.stdin)
request=matrix_request(*credentials(pathlib.Path("/root/agentteams-fs/agents/agent-a")))
room="/_matrix/client/v3/rooms/"+urllib.parse.quote(p["room"],safe="")
reads,seen={},set()
cursor=None
while True:
 page=request("GET",room+"/messages?dir=b&limit=100"+("&from="+urllib.parse.quote(cursor,safe="") if cursor else ""))
 for row in page["chunk"]:
  content=row.get("content",{})
  if row.get("type")==RECEIPT_TYPE and row.get("sender")=="@agent-a:recovery-1631.test" and content.get("status")=="read":
   ref=content.get("m.relates_to",{}).get("event_id")
   if ref: reads[ref]=row["event_id"]
  notice=(content.get("org.agentteams.coordination") or {}).get("channel_meta",{}).get("teamharness_coordination",{})
  if row.get("type")=="m.room.message" and notice.get("kind")=="task_assigned" and notice.get("task_id")==p["task"]:
   seen.add(row["event_id"])
 if matched:=sorted(seen & reads.keys()):
  print(json.dumps({"source_event":matched[0],"read_event":reads[matched[0]]}))
  break
 if not page["chunk"] or not page.get("end") or page["end"]==cursor:
  print("null")
  break
 cursor=page["end"]
'''
    return json.loads(docker("exec", "-i", "-e", "PYTHONPATH=/opt/agentteams/scripts/lib",
        PREFIX + "-worker-agent-a", "python3", "-c", code,
        text=json.dumps({"room": task["room_id"], "task": TASK_ID})))


def cutover(*, watch_return=False, watch_dashboard=False, cli_return=False):
    if watch_dashboard and (not OPERATOR_HANDOFF or watch_return):
        raise ValueError("Dashboard cutover requires the operator handoff in replacement mode")
    if cli_return and not watch_return:
        raise ValueError("CLI return requires the second cutover")
    if watch_return and not RETURN_TO_SOURCE:
        raise ValueError("watch-return requires the two-handoff scenario")
    suffix = "-return" if watch_return else ""
    source, target = (TARGET, SOURCE) if watch_return else (SOURCE, TARGET)
    boundary_path = Path("/tmp/1631-live-" + CASE + suffix + "-before-cutover.json")
    if boundary_path.exists():
        raise RuntimeError("Cutover already has a boundary snapshot; inspect it instead of replacing it")
    task = api("GET", "tasks/" + TASK_ID + "/meta?team=recovery-team")
    cp = task.get("execution", {}).get("checkpoint", {})
    if FAULT == "after-effect":
        if (task["status"] != "in_progress" or cp.get("completed_steps") != []
                or cp.get("operation", {}).get("state") != "started"
                or len(step_ledger(task)) != 1 or not fixture_tool_processes()):
            raise RuntimeError("Requires an in-flight tool after its durable external effect and before checkpoint completion")
    elif (task["status"] != "in_progress" or cp.get("completed_steps") != (["one", "two"] if watch_return else ["one"])
            or cp.get("remaining_steps") != (["three"] if watch_return else ["two", "three"])):
        raise RuntimeError("Requires the expected durable step boundary on the active Task")
    name = PREFIX + "-worker-agent-a"
    old = json.loads(docker("inspect", name))[0]
    if old["Config"]["Image"] != HARNESS_IMAGES[source]:
        raise RuntimeError("The cutover requires this fixture's selected source image")
    read = source_read_receipt(task)
    if not read:
        raise RuntimeError("Wait for the source native Matrix read receipt before cutover")
    # Capture the source while it still exists; post-cutover reads cannot prove
    # the source native session or absence of the target Task session.
    deadline = time.monotonic() + 60
    openclaw_restart = source == "openclaw" and MODE == "restart" and not FAULT
    while True:
        before = evidence(emit=False)
        if before["task"]["execution"] != task["execution"]:
            break
        if source == "qwenpaw":
            cache = before["native_sessions"]["qwenpaw_shared"]["cache"]
        elif source == "openhuman" and MODE == "restart" and not FAULT:
            proof = before["native_sessions"]["openhuman"]
            local, shared = proof["cache"], proof["descriptor"]
            cache = shared if local and shared and local["checkpoint_digest"] == shared["checkpoint_digest"] else None
        elif openclaw_restart:
            proof = before["native_sessions"]["openclaw"]
            cache = proof["shared_cache"]
            if proof["cache"]:
                break  # Retain archived runs using the legacy cache format.
        else:
            break
        if (cache and cache.get("checkpoint_version") == cp.get("version")) or time.monotonic() >= deadline:
            break
        # The native SDK persists the session after the step result is delivered.
        time.sleep(2)
    before["source_read_receipt"] = read
    before["tool_processes"] = fixture_tool_processes()
    if before["instances"][1]["id"] != old["Id"] or before["task"]["execution"] != task["execution"]:
        raise RuntimeError("Task/source changed while collecting the boundary; inspect evidence before retrying")
    if MODE == "restart" and SOURCE == "hermes":
        assert_hermes_native_cache(before["native_sessions"]["hermes_shared"], cp, TASK_ID)
    if source == "openhuman" and MODE == "restart" and not FAULT:
        assert_openhuman_native_cache(before["native_sessions"]["openhuman"], task["execution"], TASK_ID)
    if source == "codex" and not LEGACY_CODEX and MODE == "restart":
        assert_codex_native(before["native_sessions"]["codex_shared"], task, require_cache=True)
    if source == "deepseek-harness" and MODE == "restart" and not FAULT:
        assert_deepseek_native(before["native_sessions"]["deepseek"], task, require_cache=True)
    if openclaw_restart:
        proof = before["native_sessions"]["openclaw"]
        if proof["shared_cache"]:
            assert_openclaw_shared_cache(proof, cp, TASK_ID)
        elif proof["cache"]:
            assert_openclaw_native_cache(proof, cp)
        else:
            raise RuntimeError("Wait for the OpenClaw native session descriptor at the committed boundary")
    if source == "qwenpaw":
        assert_qwenpaw_native_cache(before["native_sessions"]["qwenpaw_shared"], cp, TASK_ID)
    if RETURN_TO_SOURCE:
        before["matrix"] = matrix_cursors(task)
    if watch_return:
        first = json.loads(agt("worker", "runtime-status", "--name", "agent-a", "-o", "json"))["handoff"]
        if first["id"] != TASK_ID or first["phase"] != "completed" or first["target"]["process_id"] != task["execution"]["process_id"]:
            raise RuntimeError("First handoff is not complete on the measured middle process")
        before["first_handoff"] = first
    if watch_dashboard:
        state = api("GET", "workers/agent-a/runtime-handoff")
        previous = state.get("handoff") or {}
        if previous and previous["phase"] != "completed":
            raise RuntimeError("Another handoff is active")
        before["dashboard_cutover"] = {"previous_request_id": previous.get("id")}
    if EXPIRE_HERMES_SESSION:
        # Leave the native session and Task intact. Only age the fixture's shared descriptor.
        labels = json.loads(docker("inspect", CONTROLLER))[0]["Config"]["Labels"]
        assert labels.get("io.agentteams.recovery.owner") == "1631"
        work = json.dumps(["agents/agent-a", "task", "recovery-team", TASK_ID], separators=(",", ":"))
        key = "agentteams/recovery-1631/agents/agent-a/runtime/sessions/hermes/" + hashlib.sha256(work.encode()).hexdigest() + ".json"
        previous = before["native_sessions"]["hermes_shared"]["cache"]
        assert json.loads(docker("exec", CONTROLLER, "mc", "cat", key)) == previous
        expired = {**previous, "expires_at": int(time.time()) - 3600}
        docker("exec", "-i", CONTROLLER, "mc", "pipe", key, text=json.dumps(expired))
        assert json.loads(docker("exec", CONTROLLER, "mc", "cat", key)) == expired
        before["expired_session"] = {"native_ref": expired["native_ref"], "expires_at": expired["expires_at"]}
    with boundary_path.open("x") as stream:
        json.dump(before, stream, indent=2)
    path = Path("/tmp/1631-live-" + CASE + suffix + "-cutover-events.jsonl")
    args = ["docker", "events", "--since", str(int(time.time()) - 1),
            "--filter", "type=container", "--filter", "container=" + name,
            "--filter", "event=die", "--filter", "event=destroy",
            "--filter", "event=create", "--filter", "event=start", "--format", "{{json .}}"]
    # Subscribe before the mutation. Docker's retained event history can be
    # overwritten by other containers before the first status probe returns.
    with path.open("x") as stream, subprocess.Popen(args, stdout=stream) as capture:
        try:
            if watch_return:
                subprocess.Popen([sys.executable, __file__, "observe-return-cursors"], start_new_session=True,
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if watch_return and not cli_return:
                print("Captured second boundary; switch agent-a back to the original Harness through the Dashboard.", flush=True)
            elif watch_dashboard:
                print("Captured the step boundary; submit the switch through the Dashboard.", flush=True)
            elif ABRUPT_RESTART:
                labels = json.loads(docker("inspect", CONTROLLER))[0]["Config"]["Labels"]
                assert labels.get("io.agentteams.recovery.owner") == "1631"
                assert old["HostConfig"]["RestartPolicy"]["Name"] in {"", "no"}
                docker("kill", "--signal", "KILL", old["Id"])
                assert docker("inspect", "-f", "{{.State.ExitCode}}", old["Id"]) == "137"
                docker("start", old["Id"])
            elif MODE == "restart":
                api("POST", "workers/agent-a/restart", {})
            elif OPERATOR_HANDOFF:
                request = TASK_ID + suffix
                result = json.loads(agt("worker", "switch-runtime", "--name", "agent-a", "--runtime", target,
                    "--request-id", request, "-o", "json"))
                op = result["handoff"]
                if op["id"] != request or op["target_runtime"] != target or op["target_image"] != HARNESS_IMAGES[target]:
                    raise RuntimeError("Handoff selected a different request or target image")
            else:
                api("PUT", "workers/agent-a", {"runtime": TARGET, "image": HARNESS_IMAGES[TARGET]})
            # No Matrix input is sent. Observe the Controller's actual replacement.
            deadline = time.monotonic() + (600 if OPERATOR_HANDOFF else 45)
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
        finally:
            if capture.poll() is None:
                capture.terminate()
            capture.wait(timeout=5)
    rows = [json.loads(row) for row in path.read_text().splitlines() if row]
    if replacement is None:
        raise RuntimeError("Replacement not observed; retained Docker event evidence")
    died = [row["timeNano"] for row in rows if row["Actor"]["ID"] == old["Id"] and row["Action"] == "die"]
    removed = [row["timeNano"] for row in rows if row["Actor"]["ID"] == old["Id"] and row["Action"] == "destroy"]
    created = [row["timeNano"] for row in rows if row["Actor"]["ID"] == replacement["Id"] and row["Action"] == "create"]
    starts = [row["timeNano"] for row in rows if row["Actor"]["ID"] == replacement["Id"] and row["Action"] == "start"]
    ordered = bool(died and removed and created and died[-1] <= removed[-1] <= created[0]) if MODE == "replace" else bool(died and starts and died[-1] <= starts[0])
    if not ordered:
        raise RuntimeError("Old process stop before target creation/start is not proved; inspect retained events")
    print(json.dumps({"old_container": old["Id"], "new_container": replacement["Id"],
        "events": str(path), "external_matrix_inputs_sent": 0, "task_continuation": "not yet verified"}))


def assert_native_submission(task):
    submissions = [row for row in task.get("history", []) if row.get("action") == "submit_task"]
    if task["status"] not in {"submitted", "completed"} or len(submissions) != 1:
        raise ValueError("Expected one native Worker submission")
    submission = submissions[0]
    if (submission.get("actor") != "worker:agent-a" or submission.get("from") != "in_progress"
            or submission.get("to") != "submitted" or not isinstance(submission.get("seq"), int)):
        raise ValueError("Invalid native Worker submission history")
    if task["status"] == "completed" and not any(
            row.get("action") == "accept_task_result" and row.get("actor") == "leader:leader"
            and row.get("from") == "submitted" and row.get("to") == "completed"
            and isinstance(row.get("seq"), int) and row["seq"] > submission["seq"]
            for row in task.get("history", [])):
        raise ValueError("Completed Task lacks subsequent Leader acceptance")


def cutover_lifecycle(before, suffix):
    rows = [json.loads(row) for row in Path("/tmp/1631-live-" + CASE + suffix + "-cutover-events.jsonl").read_text().splitlines() if row]
    old = before["instances"][1]["id"]
    died = next(row for row in rows if row["Action"] == "die" and row["Actor"]["ID"] == old)
    if MODE == "restart":
        target = next(row for row in rows if row["Action"] == "start" and row["Actor"]["ID"] == old)
        assert died["timeNano"] <= target["timeNano"]
        order = {"die": died["timeNano"], "start": target["timeNano"]}
        if ABRUPT_RESTART:
            assert died["Actor"]["Attributes"]["exitCode"] == "137"
            order["exit_code"] = 137
    else:
        destroyed = next(row for row in rows if row["Action"] == "destroy" and row["Actor"]["ID"] == old)
        target = next(row for row in rows if row["Action"] == "create" and row["Actor"]["ID"] != old)
        assert died["timeNano"] <= destroyed["timeNano"] <= target["timeNano"]
        order = {"die": died["timeNano"], "destroy": destroyed["timeNano"], "create": target["timeNano"]}
    return order, died["Actor"], target["Actor"]


def verify_handoff(op, before, execution, runtime):
    assert op["phase"] == "completed" and op["target_image"] == HARNESS_IMAGES[runtime]
    source = before["task"]["execution"]
    for field in ("instance_id", "process_id", "runtime"):
        assert op["source"][field] == source[field]
        assert op["target"][field] == execution[field]
    # Verify execution directly in the Task, independently of Worker readiness.
    assert execution["generation"] > source["generation"]
    assert execution["checkpoint"]["version"] > source["checkpoint"]["version"]
    times = [datetime.fromisoformat(op[field].replace("Z", "+00:00"))
             for field in ("source_stop_observed_at", "target_registered_at", "completed_at")]
    assert times[0] <= times[1] <= times[2]


def observe_return_cursors():
    """Save only sync cursor hashes from the isolated Controller's HTTP listener."""
    path = Path("/tmp/1631-live-" + CASE + "-return-sync.jsonl")
    filter_ = "tcp and (dst port 6167 or dst port 8080) and tcp[((tcp[12] & 0xf0) >> 2):4] = 0x47455420"
    run = ["docker", "exec", CONTROLLER, "timeout", "660", "tcpdump", "-l", "-nn", "-tt", "-s", "1024", "-A", "-i", "any", filter_]
    endpoint = re.compile(rb"^(\d+\.\d+) .*?IP (\d+\.\d+\.\d+\.\d+)\.\d+ > ")
    request = re.compile(rb"GET (/_matrix/client/(?:v3|r0)/sync(?:\?[^ ]*)?) HTTP/")
    with path.open("x") as output, subprocess.Popen(run, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL) as capture:
        header = None
        for line in capture.stdout:
            if match := endpoint.match(line):
                header = match
            elif header and (match := request.search(line)):
                query = urllib.parse.parse_qs(urllib.parse.urlsplit(match[1].decode()).query)
                since = query.get("since", [""])[0]
                row = {"epoch": float(header[1]), "ip": header[2].decode(),
                       "full_state": query.get("full_state", [""])[0].lower() == "true",
                       "since_sha256": hashlib.sha256(since.encode()).hexdigest() if since else None}
                print(json.dumps(row), file=output, flush=True)


def verify_return_cursor(middle, final_state, container):
    code = '''import json,os,urllib.request
req=urllib.request.Request(os.environ["AGENTTEAMS_MATRIX_URL"].rstrip("/")+"/_matrix/client/v3/account/whoami",
    headers={"Authorization":"Bearer "+os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"]})
with urllib.request.urlopen(req,timeout=15) as response:
    identity=json.load(response)
print(json.dumps([identity["user_id"],identity["device_id"]]))
'''
    account, device = json.loads(docker("exec", PREFIX + "-worker-agent-a", "python3", "-c", code))
    previous = [row for row in middle["matrix"]["cursors"].values() if
                (row["runtime"], row["account"], row["device"], row["format"]) == (SOURCE, account, device, "matrix-nio/sync-v3")]
    assert len(previous) <= 1
    assert any(row["runtime"] == SOURCE and row["account"] == account and row["device"] == device
               for row in final_state["cursors"].values()), "returning device has no committed cursor"
    started = datetime.fromisoformat(container["State"]["StartedAt"].replace("Z", "+00:00")).timestamp()
    ip = container["NetworkSettings"]["Networks"][NETWORK]["IPAddress"]
    lines = Path("/tmp/1631-live-" + CASE + "-return-sync.jsonl").read_text().splitlines()
    requests = [row for row in map(json.loads, lines) if row["epoch"] >= started and row["ip"] == ip]
    assert requests and requests[0]["full_state"], "returning SDK's initial sync request was not observed"
    expected = previous[0]["token_sha256"] if previous else None
    assert requests[0]["since_sha256"] == expected, "returning SDK used a cursor from a different consumer"
    return {"account": account, "device": device, "expected_since_sha256": expected, "initial_request": requests[0]}


def matrix_cursors(task):
    state = json.loads(docker("exec", CONTROLLER, "mc", "cat", "agentteams/recovery-1631/agents/agent-a/runtime/matrix/state.json"))
    cursors = {}
    for key, consumer in state["consumers"].items():
        cursors[key] = {field: consumer.get(field) for field in ("runtime", "account", "device", "format")}
        cursors[key]["token_sha256"] = hashlib.sha256(consumer["token"].encode()).hexdigest()
    return {"start": state["active_start"], "cursors": cursors, "through": state["rooms"][task["room_id"]]["through"]}


def verify():
    """Read back real task, storage, native receipt and Matrix evidence."""
    task = api("GET", "tasks/" + TASK_ID + "/meta?team=recovery-team")
    execution, before = task["execution"], json.loads(Path("/tmp/1631-live-" + CASE + "-before-cutover.json").read_text())
    cp = execution["checkpoint"]
    middle = json.loads(Path("/tmp/1631-live-" + CASE + "-return-before-cutover.json").read_text()) if RETURN_TO_SOURCE else None
    final_runtime = SOURCE if middle else TARGET
    assert task["task_id"] == before["task"]["task_id"] == TASK_ID
    assert before["task"]["status"] == "in_progress"
    if FAULT == "after-effect":
        assert before["task"]["execution"]["checkpoint"]["completed_steps"] == []
        assert before["tool_processes"], "fault scenario must interrupt an actual tool process"
    else:
        assert before["task"]["execution"]["checkpoint"]["completed_steps"] == ["one"]
    assert_native_submission(task)
    assert cp["delivery"] == "delivered"
    assert cp["completed_steps"] == ["one", "two", "three"] and not cp["remaining_steps"]
    assert execution["instance_id"] != before["task"]["execution"]["instance_id"]
    assert execution["process_id"] != before["task"]["execution"]["process_id"]

    def stored(key):
        return subprocess.run(["docker", "exec", CONTROLLER, "mc", "cat", "agentteams/recovery-1631/" + key],
            check=True, capture_output=True).stdout

    ledger = step_ledger(task)
    assert [row["step"] for row in ledger] == ["one", "two", "three"]
    assert [row["generation"] for row in ledger] == ([1, 2, 3] if middle else [1, 2, 2])
    if middle:
        middle_task = middle["task"]
        assert middle_task["task_id"] == TASK_ID and middle_task["status"] == "in_progress"
        assert middle_task["execution"]["checkpoint"]["completed_steps"] == ["one", "two"]
        for effect, owner in zip(ledger, (before["task"]["execution"], middle_task["execution"], execution)):
            assert (effect["instance_id"], effect["process_id"]) == (owner["instance_id"], owner["process_id"])
        assert len({entry["process_id"] for entry in ledger}) == 3
        assert len({entry["instance_id"] for entry in ledger}) == 3
    if task.get("recovery_fixture", {}).get("oracle_version") in {2, 3, 4}:
        if middle:
            assert ledger[0]["checkpoint_version"] == 1
            assert ledger[1]["checkpoint_version"] > before["task"]["execution"]["checkpoint"]["version"]
            assert ledger[2]["checkpoint_version"] > middle["task"]["execution"]["checkpoint"]["version"]
        else:
            assert_step_boundaries(ledger, before["task"]["execution"]["checkpoint"]["version"])
    if task.get("recovery_fixture", {}).get("oracle_version") in {3, 4}:
        proof = oracle_proof()
        calls = proof["calls"]
        assert len([row for row in calls if row["method"] == "POST"]) == 3
        assert all(row["accepted"] for row in calls), "Rejected effects/verification must be investigated"
        if FAULT == "after-effect":
            assert any(row["method"] == "GET" and row["step"] == "one" and row["process_id"] == execution["process_id"] for row in calls), "replacement must verify the old effect"
        for row in ledger:
            if task["recovery_fixture"]["oracle_version"] == 4:
                assert_effect_authorization(proof, row)
            output = stored("teams/recovery-team/shared/tasks/" + TASK_ID + "/output/step-" + row["step"] + ".json")
            assert json.loads(output) == row, "step artifact differs from the independent effect result"
    artifacts = cp["artifacts"] + [cp["result"]]
    for artifact in artifacts:
        data = stored(artifact["key"])
        assert len(data) == artifact["size"] and hashlib.sha256(data).hexdigest() == artifact["sha256"]
    report = stored("teams/recovery-team/" + task["result_path"])
    assert stored(cp["result"]["key"]) == report, "final reply differs from the authoritative submitted report"
    order, old_source, first_target = cutover_lifecycle(before, "")
    last_target = first_target
    if middle:
        return_order, middle_source, last_target = cutover_lifecycle(middle, "-return")
        assert first_target["ID"] == middle_source["ID"] and order["create"] < return_order["die"]
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
    "read_receipt_events":[r["event_id"] for r in receipts if r["content"]["m.relates_to"]["event_id"] in sources],
    "first_source_read_ms":min(r["origin_server_ts"] for r in receipts if r["content"]["m.relates_to"]["event_id"] in sources)}))
'''
    matrix = json.loads(docker("exec", "-i", "-e", "PYTHONPATH=/opt/agentteams/scripts/lib", PREFIX + "-worker-agent-a", "python3", "-c", code,
        text=json.dumps({"room": task["room_id"], "event": cp["reply_event"], "sha256": cp["result"]["sha256"],
            "stopped_ms": order["die"] // 1000000, "task_id": TASK_ID})))
    if read := before.get("source_read_receipt"):
        assert read["source_event"] in matrix["native_read_sources"]
        assert read["read_event"] in matrix["read_receipt_events"]
    if middle and (read := middle.get("source_read_receipt")):
        assert read["source_event"] in matrix["native_read_sources"]
        assert read["read_event"] in matrix["read_receipt_events"]
    if middle:
        first_registration = next(row for row in calls if row["method"] == "REGISTER" and row["step"] == "one")
        assert matrix["first_source_read_ms"] <= first_registration["at_ns"] // 1000000, "the initial source must be read before step one"
    old_ref = before["native_sessions"]["qwenpaw_shared"]["cache"]["native_ref"] if middle else ""
    native = native_sessions(old_ref, verify_qwen_route=final_runtime == "qwenpaw")
    if SOURCE == "openhuman" and MODE == "restart" and not FAULT:
        assert_openhuman_native_cache(before["native_sessions"]["openhuman"], before["task"]["execution"], TASK_ID)
    if final_runtime == "openhuman":
        assert_openhuman_native_cache(native["openhuman"], execution, TASK_ID, terminal=True)
        if MODE == "restart":
            assert native["openhuman"]["cache"]["session_id"] == before["native_sessions"]["openhuman"]["cache"]["session_id"], "valid native Task session was not reused"
    if SOURCE == "qwenpaw":
        assert_qwenpaw_native_cache(before["native_sessions"]["qwenpaw_shared"],
            before["task"]["execution"]["checkpoint"], TASK_ID)
    if final_runtime == "qwenpaw":
        selected = assert_qwenpaw_native_route(native["qwenpaw_shared"], TASK_ID, execution["generation"])
        if middle:
            assert selected["session_id"] != old_ref, "the stale first-generation session was reused"
            retained = _qwenpaw_native(native["qwenpaw_shared"], TASK_ID, old_ref)
            assert retained["native_digest"] == before["native_sessions"]["qwenpaw_shared"]["cache"]["native_digest"]
            assert len(matrix["read_receipt_events"]) == 1, "the initial assignment was marked read more than once"
        if MODE == "restart":
            source = before["native_sessions"]["qwenpaw_shared"]
            assert_qwenpaw_native_cache(source, before["task"]["execution"]["checkpoint"], TASK_ID)
            assert selected["session_id"] == source["cache"]["native_ref"], "valid native Task session was not reused"
    if SOURCE == "hermes":
        selected = before["native_sessions"]["hermes"][-1]["session_id"]
        assert before["native_sessions"]["hermes_message_counts"].get(selected, 0) > 0, "new route still used a different warm native Agent"
    if final_runtime == "hermes":
        selected = native["hermes"][-1]["session_id"]
        assert native["hermes_message_counts"].get(selected, 0) > 0, "recovered input was not persisted in the selected native session"
    if SOURCE == "deepseek-harness" and not FAULT:
        assert_deepseek_native(before["native_sessions"]["deepseek"], before["task"], require_cache=MODE == "restart")
    if final_runtime == "deepseek-harness":
        selected = assert_deepseek_native(native["deepseek"], task)
        if MODE == "restart":
            original = before["native_sessions"]["deepseek"]["native"]
            assert selected["session_id"] == original["session_id"] and selected["turn"] > original["turn"]
    if final_runtime == "openclaw":
        proof = native["openclaw"]
        if proof["cache"]:
            assert_openclaw_native_cache(proof, cp)
            selected = proof["native"]
        else:
            assert len(proof["turn_sessions"]) == 1, "native session for the recovered execution was not found"
            selected = proof["turn_sessions"][0]
            assert selected["message_count"] > 0 and selected["task_mentioned"]
        if MODE == "restart":
            source_cache = before["native_sessions"]["openclaw"]
            source_checkpoint = before["task"]["execution"]["checkpoint"]
            if source_cache["shared_cache"]:
                original = assert_openclaw_shared_cache(source_cache, source_checkpoint, TASK_ID)
            else:
                assert_openclaw_native_cache(source_cache, source_checkpoint)
                original = source_cache["native"]
            assert selected["session_id"] == original["session_id"], "valid native Task session was not reused"
    if not LEGACY_CODEX and SOURCE == "codex":
        assert_codex_native(before["native_sessions"]["codex_shared"], before["task"], require_cache=MODE == "restart")
    if not LEGACY_CODEX and final_runtime == "codex":
        target = assert_codex_native(native["codex_shared"], task)
        if MODE == "restart":
            original = before["native_sessions"]["codex_shared"]["native"]
            assert target["thread_id"] == original["thread_id"] and target["turn_id"] != original["turn_id"]
    if LEGACY_CODEX and MODE == "restart" and SOURCE == "codex":
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
        assert_hermes_native_cache(before["native_sessions"]["hermes_shared"],
            before["task"]["execution"]["checkpoint"], TASK_ID)
        resumed = native["hermes"][-1]
        if EXPIRE_HERMES_SESSION:
            expired = before["expired_session"]
            assert expired["native_ref"] == cache["session_id"] and expired["expires_at"] < before["recorded_at"]
            assert resumed["session_id"] != cache["session_id"] and resumed["reused"] is False
        else:
            assert resumed["session_id"] == cache["session_id"] and resumed["reused"] is True
    controller = json.loads(docker("inspect", CONTROLLER))[0]
    current = json.loads(docker("inspect", PREFIX + "-worker-agent-a"))[0]
    assert current["Id"] == last_target["ID"], "additional replacement requires separate evidence"
    assert current["Id"] in execution["instance_id"]
    assert current["State"]["StartedAt"] in execution["instance_id"], "execution must bind the actual process start, including automatic retries"
    if MODE == "restart":
        started = datetime.fromisoformat(current["State"]["StartedAt"].replace("Z", "+00:00")).timestamp()
        assert order["die"] / 1e9 <= started <= order["start"] / 1e9 + 1e-6, "restart retried after the captured startup"
    source_pin = old_source["Attributes"]["io.agentteams.recovery.core-sha"]
    target_pin = first_target["Attributes"]["io.agentteams.recovery.core-sha"]
    sources = {"core": controller["Config"]["Labels"]["io.agentteams.recovery.core-sha"],
        SOURCE: old_source["Attributes"]["io.agentteams.recovery.harness-sha"], TARGET: first_target["Attributes"]["io.agentteams.recovery.harness-sha"]}
    if source_pin == target_pin:
        sources["shared_contract"] = source_pin
    else:
        sources["shared_contract_by_runtime"] = {SOURCE: source_pin, TARGET: target_pin}
    proof = {"task_id": TASK_ID, "case": CASE, "scope": f"one real {SOURCE}-to-{TARGET} in-progress continuation", "mode": MODE,
        "fixture_prefix": PREFIX, "authoritative_report_verified": True,
        "sources": sources, "driver_sha256": DRIVER_SHA256, "native_recovery_sessions": native,
        "generation_before": before["task"]["execution"]["generation"], "generation_after": execution["generation"],
        "checkpoint_before": before["task"]["execution"]["checkpoint"]["version"], "checkpoint_after": cp["version"],
        "execution": execution, "final_task_status": task["status"], "task_history": task["history"], "ledger": ledger, "artifacts_verified": artifacts,
        "docker_order_ns": order, "matrix": matrix}
    if SOURCE == "openhuman":
        # Replacement only requires a durable Task boundary. The stopping
        # source may still be publishing its latest native session.
        proof["openhuman_source_session"] = before["native_sessions"]["openhuman"]["descriptor"]
    if EXPIRE_HERMES_SESSION:
        proof["scope"] = "Hermes restart with an expired shared session descriptor"
        proof["expired_session"] = before["expired_session"]
    if ABRUPT_RESTART:
        proof["scope"] = f"{SOURCE} restart after SIGKILL at a committed step boundary"
    if image_id := before["instances"][1].get("image_id"):
        proof["images"] = {"controller": controller["Image"], "source": image_id, "target": current["Image"]}
    if OPENHUMAN_PREAPPROVED:
        proof["openhuman_preapproved_fixture"] = True
    if OPERATOR_HANDOFF:
        op = json.loads(agt("worker", "runtime-status", "--name", "agent-a", "-o", "json"))["handoff"]
        if middle:
            assert op["id"] != TASK_ID and middle["first_handoff"]["id"] == TASK_ID
            verify_handoff(middle["first_handoff"], before, middle["task"]["execution"], TARGET)
            verify_handoff(op, middle, execution, SOURCE)
            assert last_target["Attributes"]["io.agentteams.recovery.harness-sha"] == sources[SOURCE]
            cursor_snapshots = [before["matrix"], middle["matrix"], matrix_cursors(task)]
            for sample, start in zip(cursor_snapshots, (before["task"]["execution"], middle["task"]["execution"], execution)):
                assert sample["start"]["process_id"] == start["process_id"]
            first_keys = {key for key, row in cursor_snapshots[0]["cursors"].items() if row["runtime"] == SOURCE}
            assert first_keys and all(first_keys <= set(sample["cursors"]) for sample in cursor_snapshots[1:])
            proof.update(scope=f"one real {SOURCE}-to-{TARGET}-to-{SOURCE} in-progress continuation",
                         first_handoff=middle["first_handoff"], return_docker_order_ns=return_order,
                         first_native_ref=old_ref, matrix_cursor_snapshots=cursor_snapshots,
                         initial_return_sync=verify_return_cursor(middle, cursor_snapshots[-1], current))
        else:
            dashboard = before.get("dashboard_cutover")
            if dashboard is None:
                assert op["id"] == TASK_ID
            else:
                assert op["id"] and op["id"] != dashboard["previous_request_id"]
            verify_handoff(op, before, execution, TARGET)
        proof["operator_handoff"] = op
    path = Path("/tmp/1631-live-" + CASE + "-verified.json")
    path.write_text(json.dumps(proof, indent=2))
    print(json.dumps({"passed": True, "evidence": str(path), **proof}))


def assert_deepseek_native(proof, task, *, require_cache=False):
    execution = task["execution"]
    item = proof["native"]
    marker = f"recovery:recovery-team/{task['task_id']}:{execution['generation']}"
    assert item and item["input_id"] == "matrix-" + hashlib.sha256(marker.encode()).hexdigest()
    assert item["task_mentioned"] and item["completed"] and item["tool_calls"] and item["tool_results"]
    if require_cache:
        cache = proof["cache"]
        cp = execution["checkpoint"]
        work = {"kind": "task", "team_id": "recovery-team", "task_id": task["task_id"]}
        digest = hashlib.sha256(json.dumps(cp, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        assert cache and cache["schema_version"] == 1 and cache["kind"] == "WorkerSessionCache"
        assert cache["runtime"] == "deepseek-harness" and cache["member_prefix"] == "agents/agent-a"
        assert cache["work"] == work and cache["generation"] == execution["generation"]
        assert all(cache["start"][key] == execution[key] for key in ("process_id", "instance_id", "runtime"))
        assert cache["expires_at"] > time.time() and cache["checkpoint_version"] == cp["version"]
        assert cache["checkpoint_digest"] == digest
        assert (cache["native_ref"], cache["native_digest"]) == (item["session_id"], item["native_digest"])
    return item


def assert_codex_native(proof, task, *, require_cache=False):
    execution = task["execution"]
    work = {"kind": "task", "team_id": "recovery-team", "task_id": task["task_id"]}
    route, native = proof["route"], proof["native"]
    assert route["team_id"] == work["team_id"] and route["task_id"] == work["task_id"]
    assert all(route[k] == execution[k] for k in ("instance_id", "process_id", "generation"))
    assert native and native["thread_id"] == native["metadata_id"] and native["turn_id"]
    assert native["native_bytes"] and native["source_inputs"] == 1 and native["tool_calls"] and native["tool_outputs"]
    if require_cache:
        cache = proof["cache"]
        assert cache and cache["schema_version"] == 1 and cache["kind"] == "WorkerSessionCache" and cache["runtime"] == "codex"
        assert cache["member_prefix"] == "agents/agent-a" and cache["work"] == work
        assert cache["generation"] == execution["generation"] and all(cache["start"][k] == execution[k] for k in ("instance_id", "process_id", "runtime"))
        cp = execution["checkpoint"]
        digest = hashlib.sha256(json.dumps(cp, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        assert cache["checkpoint_version"] == cp["version"] and cache["checkpoint_digest"] == digest
        assert cache["config_generation"] == proof["config_generation"] and cache["expires_at"] > time.time()
        assert (cache["native_ref"], cache["native_digest"]) == (native["thread_id"], native["native_digest"])
    return native


def assert_hermes_native_cache(proof, checkpoint, task_id):
    assert proof and proof.get("cache"), "shared Hermes session descriptor must be durable before a valid-reuse cutover"
    cache = proof["cache"]
    assert cache["schema_version"] == 1 and cache["kind"] == "WorkerSessionCache"
    assert cache["member_prefix"] == "agents/agent-a" and cache["runtime"] == "hermes"
    assert cache["work"] == {"kind": "task", "team_id": "recovery-team", "task_id": task_id}
    assert cache["expires_at"] > time.time() and cache["native_ref"]
    assert cache["checkpoint_version"] == checkpoint["version"]
    assert cache["checkpoint_digest"] == hashlib.sha256(json.dumps(checkpoint, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    assert cache["config_generation"] == proof["config_generation"]
    assert proof["native_digest"] and cache["native_digest"] == proof["native_digest"]
    assert proof["message_count"] > 0 and proof["tool_count"] > 0 and proof["task_mentioned"]


def _qwenpaw_native(proof, task_id, session_id):
    prefix = "matrix:task:" + hashlib.sha256(json.dumps(["recovery-team", task_id],
        sort_keys=True, separators=(",", ":")).encode()).hexdigest() + ":"
    assert session_id.startswith(prefix), "native route belongs to another Task"
    selected = [row for row in proof["native"] if row["session_id"] == session_id]
    assert len(selected) == 1, "actual SDK session must be persisted and unambiguous"
    native = selected[0]
    assert native["agent_state_present"] and native["task_mentioned"]
    assert native["message_count"] > 0 and native["tool_count"] > 0
    assert len(native["native_digest"]) == 64
    return native


def assert_qwenpaw_native_cache(proof, checkpoint, task_id):
    cache = proof["cache"]
    assert cache and cache["schema_version"] == 1 and cache["kind"] == "WorkerSessionCache"
    assert cache["member_prefix"] == "agents/agent-a" and cache["runtime"] == "qwenpaw"
    assert cache["work"] == {"kind": "task", "team_id": "recovery-team", "task_id": task_id}
    assert cache["checkpoint_version"] == checkpoint["version"]
    assert cache["checkpoint_digest"] == hashlib.sha256(json.dumps(checkpoint,
        sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    assert cache["config_generation"] == proof["config_generation"] and cache["expires_at"] > time.time()
    native = _qwenpaw_native(proof, task_id, cache["native_ref"])
    assert native["native_digest"] == cache["native_digest"]
    return native


def assert_qwenpaw_native_route(proof, task_id, generation):
    # Submitted Tasks intentionally reject session-cache writes. Their
    # read-back trace route selects the actual SDK session, while the Task
    # verifier independently checks its authoritative result and effects.
    routes = proof["routes"]
    assert routes and all(row["trace_id"] and row["task_id"] == task_id
        and row["runtime"] == "qwenpaw" and row["generation"] == generation for row in routes)
    refs = {row["session_id"] for row in routes}
    assert len(refs) == 1, "native execution used multiple Task sessions"
    return _qwenpaw_native(proof, task_id, refs.pop())


def assert_openhuman_native_cache(proof, execution, task_id, *, terminal=False):
    cache, shared, checkpoint = proof["cache"], proof["descriptor"], execution["checkpoint"]
    assert shared and shared["schema_version"] == 1 and shared["kind"] == "WorkerSessionCache"
    assert shared["runtime"] == "openhuman" and shared["member_prefix"] == "agents/agent-a"
    assert shared["work"] == {"kind": "task", "team_id": "recovery-team", "task_id": task_id}
    assert shared["generation"] == execution["generation"] and all(shared["start"][k] == execution[k] for k in ("instance_id", "process_id", "runtime"))
    assert shared["expires_at"] > time.time() and shared["config_generation"] == proof["config_generation"]
    assert cache and cache["session_id"] == shared["native_ref"] and cache["native_digest"] == shared["native_digest"]
    assert cache["checkpoint_digest"] == shared["checkpoint_digest"]
    if terminal:
        # Submission closes session publication. Verify the target generation's
        # last active native tool transcript and validate its final Task separately.
        assert 0 < shared["checkpoint_version"] <= checkpoint["version"]
    else:
        assert shared["checkpoint_version"] == checkpoint["version"]
        assert cache["checkpoint_digest"] == hashlib.sha256(json.dumps(checkpoint, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    assert cache and cache["format_version"] == 1 and cache["runtime"] == "openhuman"
    assert cache["scope"] == ["recovery-team", task_id] and cache["authority"] == proof["authority"], "native Task/Worker scope changed"
    assert cache["session_id"] and cache["expires_at"] > time.time()
    history = cache["history"]
    assert history and all(row["role"] in {"user", "assistant", "tool"} and isinstance(row["content"], str) for row in history)
    has_tools = any(row["role"] == "tool" for row in history)
    for previous, row in zip(history, history[1:]):
        if previous["role"] != "assistant" or row["role"] != "user":
            continue
        # The SDK's text-tool fallback serializes actual tool results as user
        # messages. Bind the native result tag to its preceding parsed call.
        result = re.match(r'^\[Tool results\]\n<tool_result name="([^"]+)">\n.*?\n</tool_result>', row["content"], re.S)
        if result:
            for body in re.findall(r"<tool_call>(.*?)</tool_call>", previous["content"], re.S):
                try:
                    call = json.loads(body)
                except ValueError:
                    continue
                if isinstance(call, dict) and call.get("name") == result[1] and isinstance(call.get("arguments"), dict):
                    has_tools = True
    assert has_tools, "native tool history is absent"
    # Rust serde_json persists native UTF-8, unlike Python's default ASCII escaping.
    transcript = json.dumps(history, sort_keys=True, ensure_ascii=False, separators=(",", ":"))
    assert cache["native_digest"] == hashlib.sha256(transcript.encode()).hexdigest(), "native history changed after checkpoint binding"
    assert task_id in transcript, "native history belongs to a different Task"


def assert_openclaw_native_cache(proof, checkpoint):
    cache, native = proof["cache"], proof["native"]
    assert cache and cache["runtime"] == "openclaw" and cache["expires_at"] > time.time()
    assert cache["checkpoint_digest"] == hashlib.sha256(json.dumps(checkpoint, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    assert native and native["session_id"] == cache["session_id"]
    assert native["native_digest"] == cache["native_digest"], "native transcript changed after checkpoint binding"
    assert native["message_count"] > 0 and native["task_mentioned"]


def assert_openclaw_shared_cache(proof, checkpoint, task_id):
    cache, native = proof["shared_cache"], proof["shared_native"]
    assert cache and cache["schema_version"] == 1 and cache["kind"] == "WorkerSessionCache"
    assert cache["member_prefix"] == "agents/agent-a" and cache["runtime"] == "openclaw"
    assert cache["work"] == {"kind": "task", "team_id": "recovery-team", "task_id": task_id}
    assert cache["checkpoint_version"] == checkpoint["version"] and cache["expires_at"] > time.time()
    assert cache["config_generation"] == proof["config_generation"]
    assert cache["checkpoint_digest"] == hashlib.sha256(json.dumps(checkpoint, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    assert native and native["session_key"] == cache["native_ref"] and native["native_digest"] == cache["native_digest"]
    assert native["has_recovery_turn"] and native["message_count"] > 0 and native["task_mentioned"]
    return native


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
    if OPERATOR_HANDOFF:
        target_image_env = ("AGENTTEAMS_WORKER_IMAGE" if TARGET == "openclaw" else
                            "AGENTTEAMS_" + TARGET.upper().replace("-", "_") + "_WORKER_IMAGE")
        env[target_image_env] = HARNESS_IMAGES[TARGET]
        if RETURN_TO_SOURCE:
            env["AGENTTEAMS_QWENPAW_WORKER_IMAGE"] = HARNESS_IMAGES[SOURCE]
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
        if name == "agent-a" and OPENHUMAN_PREAPPROVED:
            # Authorize the isolated fixture before assignment. Native command
            # restrictions still apply, and other installations keep their policy.
            manifest["spec"].setdefault("env", {})["AGENTTEAMS_YOLO"] = "1"
        agt("apply", "-f", "/dev/stdin", text=json.dumps(manifest))
    team = {"apiVersion": "agentteams.io/v1beta1", "kind": "Team", "metadata": {"name": "recovery-team"},
        "spec": {"teamName": "recovery-team", "admin": {"name": "recovery-admin"},
            "workerMembers": [{"name": "leader", "role": "team_leader"}, {"name": "agent-a", "role": "worker"}]}}
    agt("apply", "-f", "/dev/stdin", text=json.dumps(team))
    print("Created isolated Agent A and Leader; model/task execution not yet verified")


if __name__ == "__main__":
    {"setup": setup, "restore": lambda: setup(restore=True), "status": status, "members": members, "prepare": prepare, "oracle": oracle, "tracing": tracing, "seed": seed,
     "refresh-controller": refresh_controller, "evidence": evidence, "cutover": cutover, "watch-return": lambda: cutover(watch_return=True),
     "watch-dashboard-cutover": lambda: cutover(watch_dashboard=True),
     "return-cutover": lambda: cutover(watch_return=True, cli_return=True),
     "observe-return-cursors": observe_return_cursors, "verify": verify}[sys.argv[1]]()
