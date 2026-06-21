#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"
require "tmpdir"

repo_root = Pathname.new(__dir__).join("../../../../..").expand_path
mcp_dir = repo_root / "plugins/teamharness/mcp"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

Dir.mktmpdir("teamharness-filesync-") do |dir|
  root = Pathname.new(dir)
  workspace = root / "workspace"
  bin_dir = root / "bin"
  log_path = root / "mc.log"
  bin_dir.mkpath
  (bin_dir / "mc").write(<<~SH)
    #!/usr/bin/env bash
    printf '%s\\n' "$*" >> "#{log_path}"
    printf 'ENV MC_HOST_hiclaw=%s\\n' "${MC_HOST_hiclaw:-}" >> "#{log_path}"
    case "$*" in
      *"hiclaw/hiclaw-storage"*)
        if [ -z "${MC_HOST_hiclaw:-}" ]; then
          echo "missing MC_HOST_hiclaw" >&2
          exit 3
        fi
        ;;
    esac
    case "$*" in
      *"tasks/denied"*)
        echo "mc.bin: <ERROR> Unable to list comparison retrying.. Access Denied." >&2
        exit 0
        ;;
    esac
    if [ "$1" = "ls" ]; then
      echo "2026-06-03 12:00:00      42 projects/demo/plan.md"
    fi
  SH
  (bin_dir / "mc").chmod(0o755)

  python_test = <<~PY
    import json
    import os
    import pathlib
    import sys

    sys.path.insert(0, str(pathlib.Path("#{mcp_dir}")))
    from server import call_tool

    common = {
        "workspaceDir": "#{workspace}",
        "storage": {
            "sharedPrefix": "mock/shared",
            "globalSharedPrefix": "mock/global-shared",
        },
    }

    def payload(args):
        merged = dict(common)
        merged.update(args)
        result = call_tool("filesync", merged)
        return json.loads(result["content"][0]["text"])

    def payload_without_storage(args):
        result = call_tool("filesync", args)
        return json.loads(result["content"][0]["text"])

    dry = payload({
        "action": "pull",
        "path": "shared/projects/demo",
        "dryRun": True,
    })
    if not dry.get("ok"):
        raise AssertionError(f"dry-run pull failed: {dry!r}")
    if dry.get("path") != "shared/projects/demo/":
        raise AssertionError(f"path was not normalized: {dry!r}")
    if dry.get("kind") != "shared":
        raise AssertionError(f"kind mismatch: {dry!r}")
    if dry.get("localPath") != "#{workspace}/shared/projects/demo":
        raise AssertionError(f"local path mismatch: {dry!r}")
    if dry.get("remotePath") != "mock/shared/projects/demo/":
        raise AssertionError(f"remote path mismatch: {dry!r}")
    if dry.get("command") != ["mc", "mirror", "mock/shared/projects/demo/", "#{workspace}/shared/projects/demo", "--overwrite"]:
        raise AssertionError(f"dry-run command mismatch: {dry!r}")

    blocked_global_push = payload({
        "action": "push",
        "path": "global-shared/readme.md",
        "dryRun": True,
    })
    if blocked_global_push.get("ok") is not False or "read-only" not in blocked_global_push.get("error", ""):
        raise AssertionError(f"global-shared push was not rejected: {blocked_global_push!r}")

    blocked_escape = payload({
        "action": "pull",
        "path": "../shared/tasks/t-001",
        "dryRun": True,
    })
    if blocked_escape.get("ok") is not False or "relative shared path" not in blocked_escape.get("error", ""):
        raise AssertionError(f"workspace escape was not rejected: {blocked_escape!r}")

    result_path = pathlib.Path("#{workspace}") / "shared/tasks/t-001/result.md"
    result_path.parent.mkdir(parents=True, exist_ok=True)
    result_path.write_text("# Result\\n", encoding="utf-8")
    pushed = payload({
        "action": "push",
        "path": "shared/tasks/t-001",
        "exclude": ["*.tmp"],
    })
    if not pushed.get("ok") or pushed.get("path") != "shared/tasks/t-001/":
        raise AssertionError(f"push failed: {pushed!r}")
    if pushed.get("exclude") != ["*.tmp"]:
        raise AssertionError(f"push exclude mismatch: {pushed!r}")

    pushed_file = payload({
        "action": "push",
        "path": "shared/tasks/t-001/result.md",
    })
    if not pushed_file.get("ok"):
        raise AssertionError(f"single-file push failed: {pushed_file!r}")
    if pushed_file.get("command") != ["mc", "cp", "#{workspace}/shared/tasks/t-001/result.md", "mock/shared/tasks/t-001/result.md"]:
        raise AssertionError(f"single-file push command mismatch: {pushed_file!r}")

    pulled_file = payload({
        "action": "pull",
        "path": "shared/tasks/t-001/result.md",
    })
    if not pulled_file.get("ok"):
        raise AssertionError(f"single-file pull failed: {pulled_file!r}")
    if pulled_file.get("command") != ["mc", "cp", "mock/shared/tasks/t-001/result.md", "#{workspace}/shared/tasks/t-001/result.md"]:
        raise AssertionError(f"single-file pull command mismatch: {pulled_file!r}")

    denied_dir = pathlib.Path("#{workspace}") / "shared/tasks/denied"
    denied_dir.mkdir(parents=True, exist_ok=True)
    (denied_dir / "result.md").write_text("# Denied\\n", encoding="utf-8")
    denied = payload({
        "action": "push",
        "path": "shared/tasks/denied",
    })
    if denied.get("ok") is not False or "Access Denied" not in denied.get("error", ""):
        raise AssertionError(f"zero-exit mc error was not detected: {denied!r}")

    listed = payload({
        "action": "list",
        "path": "shared/projects/demo",
    })
    if not listed.get("ok") or listed.get("entries") != ["2026-06-03 12:00:00      42 projects/demo/plan.md"]:
        raise AssertionError(f"list failed: {listed!r}")

    stat = payload({
        "action": "stat",
        "path": "shared/tasks/t-001/result.md",
    })
    if not stat.get("ok") or stat.get("exists") is not True:
        raise AssertionError(f"stat failed: {stat!r}")
    if stat.get("remotePath") != "mock/shared/tasks/t-001/result.md":
        raise AssertionError(f"stat remote path mismatch: {stat!r}")

    runtime_config = pathlib.Path("#{root}") / "runtime.yaml"
    runtime_config.write_text(
        "storage:\\n"
        "  sharedPrefix: teams/demo-team/shared\\n"
        "  globalSharedPrefix: shared\\n",
        encoding="utf-8",
    )
    os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(runtime_config)
    os.environ["HICLAW_STORAGE_PREFIX"] = "hiclaw/hiclaw-storage"
    from_runtime = payload_without_storage({
        "workspaceDir": "#{workspace}",
        "action": "list",
        "path": "shared/tasks/demo",
        "dryRun": True,
    })
    if from_runtime.get("remotePath") != "hiclaw/hiclaw-storage/teams/demo-team/shared/tasks/demo/":
        raise AssertionError(f"runtime storage prefix mismatch: {from_runtime!r}")

    runtime_result = pathlib.Path("#{workspace}") / "shared/tasks/runtime-push/result.md"
    runtime_result.parent.mkdir(parents=True, exist_ok=True)
    runtime_result.write_text("# Runtime Push\\n", encoding="utf-8")
    pushed_runtime = payload_without_storage({
        "workspaceDir": "#{workspace}",
        "action": "push",
        "path": "shared/tasks/runtime-push",
    })
    if not pushed_runtime.get("ok"):
        raise AssertionError(f"runtime-prefix push failed: {pushed_runtime!r}")
    if pushed_runtime.get("remotePath") != "hiclaw/hiclaw-storage/teams/demo-team/shared/tasks/runtime-push/":
        raise AssertionError(f"runtime-prefix push remote mismatch: {pushed_runtime!r}")

    print(json.dumps({
        "ok": True,
        "dryRunPath": dry["path"],
        "remotePath": dry["remotePath"],
      "runtimeRemotePath": from_runtime["remotePath"],
      "runtimePushRemotePath": pushed_runtime["remotePath"],
      "pushPath": pushed["path"],
      "pushFileCommand": pushed_file["command"],
      "entries": listed["entries"],
      "statPath": stat["remotePath"],
    }, ensure_ascii=False))
  PY

  env = {
    "PATH" => "#{bin_dir}:#{ENV.fetch("PATH", "")}",
    "HICLAW_FS_ENDPOINT" => "https://oss.example.test",
    "HICLAW_FS_ACCESS_KEY" => "access-key",
    "HICLAW_FS_SECRET_KEY" => "secret-key"
  }
  stdout, stderr, status = Open3.capture3(env, "python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
  fail!(["teamharness filesync MCP test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

  commands = log_path.read.lines.map(&:strip)
  fail!("mc mirror was not called: #{commands.inspect}") unless commands.include?(
    "mirror #{workspace}/shared/tasks/t-001/ mock/shared/tasks/t-001/ --overwrite --exclude *.tmp"
  )
  fail!("mc cp push was not called: #{commands.inspect}") unless commands.include?(
    "cp #{workspace}/shared/tasks/t-001/result.md mock/shared/tasks/t-001/result.md"
  )
  fail!("mc cp pull was not called: #{commands.inspect}") unless commands.include?(
    "cp mock/shared/tasks/t-001/result.md #{workspace}/shared/tasks/t-001/result.md"
  )
  fail!("mc ls was not called: #{commands.inspect}") unless commands.include?(
    "ls --recursive mock/shared/projects/demo/"
  )
  fail!("mc stat was not called: #{commands.inspect}") unless commands.include?(
    "stat mock/shared/tasks/t-001/result.md"
  )
  fail!("runtime-prefix mc mirror was not called: #{commands.inspect}") unless commands.include?(
    "mirror #{workspace}/shared/tasks/runtime-push/ hiclaw/hiclaw-storage/teams/demo-team/shared/tasks/runtime-push/ --overwrite"
  )
  fail!("runtime-prefix mc mirror did not receive MC_HOST_hiclaw: #{commands.inspect}") unless commands.any? do |line|
    line.start_with?("ENV MC_HOST_hiclaw=https://access-key:secret-key@oss.example.test")
  end

  puts JSON.pretty_generate(JSON.parse(stdout).merge("mcCommands" => commands))
end
