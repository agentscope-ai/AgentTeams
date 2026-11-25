# Claude Code Harness

Select `claudecode` to run a managed AgentTeams member with Anthropic's Python Agent SDK and its bundled Claude Code executable. The same image accepts the Manager, Leader and Worker role contracts. Like the [Codex Harness](codex.md), the adapter owns Matrix delivery and native sessions. Claude Code owns the model loop and tool execution. The SDK exposes structured results and SDK hooks while using the official headless runtime. [Agent SDK](https://code.claude.com/docs/en/agent-sdk/overview) · [Python package and terms](https://github.com/anthropics/claude-agent-sdk-python#license-and-terms)

This integration is experimental and has not completed full admission. The shared image workflow builds it only when `runtime=claudecode` is explicitly selected; `all` does not include it. Passing native tests does not replace end-to-end delivery, recovery and mixed-runtime acceptance.

## Build and select the image

Check out [the Claude Code Harness](https://github.com/agentteams-group/agentteams-harness-claudecode) beside AgentTeams at the `CLAUDECODE_REF` recorded in `.github/workflows/publish-harnesses.yml`. Build from a Core checkout with the pinned DeepSeek submodule initialized. Pass Core as the named build context `agentteams` and the independent Harness checkout as the positional context. The image uses the pinned Python Matrix transport helpers. Set `CONTROLLER_IMAGE` to a Controller image built from this revision for the matching `agt` and `mc` binaries.

```sh
git submodule update --init deepseek-harness
docker buildx build --load --build-context agentteams=. -f ../agentteams-harness-claudecode/Dockerfile \
  --build-arg AGENTTEAMS_CONTROLLER_IMAGE="$CONTROLLER_IMAGE" \
  -t agentteams/agentteams-harness-claudecode:dev ../agentteams-harness-claudecode/
```

Deploy the Controller and CRDs from this revision. With a single-host Docker backend, use the locally built image. For Kubernetes, make the image available to the nodes and use its published reference.

```sh
agt create worker --name claude-dev --runtime claudecode \
  --model "$AT_MODEL" --image agentteams/agentteams-harness-claudecode:dev
```

To set a deployment-wide image, configure `AGENTTEAMS_CLAUDECODE_WORKER_IMAGE` on the Controller. Helm accepts `worker.defaultImage.claudecode.repository` and `worker.defaultImage.claudecode.tag`. To select a Claude Code Manager, set `manager.runtime=claudecode` and `manager.model` to the gateway model. A Team Leader uses a Worker with this runtime and the Leader role.

## Model, tools and Matrix

The adapter resolves the model and gateway credential from the Controller's member contract. The gateway accepts the member's bearer token and serves streaming Anthropic Messages at `/v1/messages`. A route prefix such as `/team-a` is preserved. Test a tool call as well as plain text when qualifying a new provider. [Claude Code gateway protocol](https://code.claude.com/docs/en/llm-gateway-protocol) · [Higress AI Proxy](https://higress.io/en/docs/latest/plugins/ai/api-provider/ai-proxy/)

Send text to the member's personal room or mention it in the team room. The adapter applies the projected sender policy, binds the role prompts, skills and TeamHarness MCP server, and returns the result to the originating event. Incoming files are available in `inbox/`. Put files to return under `outbox/`. Declared HTTP MCP servers on the model gateway receive the member credential. Native file tools operate in the turn's workspace. `AGENTTEAMS_YOLO=1` also enables Bash for ordinary turns in the member container.

Each room has its own Claude session. Workers persist accepted Matrix inputs and device-specific cursors through the Controller. Manager and Leader room turns also persist their delivery journal and transcripts. Role prompts bind the public Task contract, the current roster and Matrix input references. The Manager reads final delivery facts on its next conversation turn. A transient preparation or SDK handshake timeout retries the same pending input with backoff. After three consecutive failed polls, the process exits with the error. A turn with a possible tool effect uses the persisted operation or delivery record before further execution.

On startup and after each Matrix scan, a Worker discovers recoverable managed Tasks. It claims each Task with the current instance, process and generation. It reads prepared upstream files from the current input directories and writes deliverables in the assigned output directory. File mutations publish the verified bytes and native call receipt to the execution checkpoint. A replacement Claude Worker restores those outputs before continuing. Pending replies use the saved result and Matrix transaction. An unresolved tool effect requires verification.

Use the Controller's Worker runtime handoff to switch the same Worker between configured Harness images. Each runtime retains its own Matrix cursor and native session cache. The Claude adapter checks the stored transcript bytes, session expiry, execution scope and checkpoint compatibility before resuming a session. A missing or incompatible transcript starts a fresh native session from the authoritative Task record. The target Worker reports readiness after its Matrix and Task recovery scans.

## Managed tasks

In the Manager's personal Matrix room, ask a registered team to produce a file. The Manager creates the main Task with the original event ID. Its Leader plans the work and assigns an execution Task. On startup, Leaders and Workers register their process with the Controller. The Worker claims its exact assignment, prepares its inputs, writes the file, and submits a candidate. The Leader reviews that submission and completes the main Task. The task system returns the final report and the accepted file to the original room.

For an end-to-end check, include a fresh batch ID and several numbers in the request and ask for a JSON file containing the batch ID and their sum. Use `agt task get <task-id> --team <team>` to check the original event ID, the Worker's submission ID, and the Leader's review of that submission. Download the file from the original Matrix room and compare its contents and SHA-256 with the candidate artifact.

This experimental integration supports unencrypted Matrix rooms and the projected model contract. Native read and write tools are available during managed execution. Configure an OTLP HTTP Collector with content capture disabled for acceptance. Record the exact images and collect Matrix events, accepted candidates, downloaded file hashes and native spans for each trial. Count model attempts using the `claude-code.model.request` spans under each turn, including retries and attempts that ended without a complete reply. Count completed responses separately by response ID.

## Verify

From the Harness checkout, run its native tests against the same Core revision used to build the image.

```sh
python -m pip install -r requirements.txt
AGENTTEAMS_CORE_DIR="$(cd ../AgentTeams && pwd)" python -m unittest discover -s tests
```
