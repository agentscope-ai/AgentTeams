#!/usr/bin/env ruby
# frozen_string_literal: true

require "fileutils"
require "json"
require "open3"
require "pathname"
require "tmpdir"
require "yaml"

script_dir = Pathname.new(__dir__).expand_path
adapter_root = script_dir.parent
remote_root = adapter_root.parent
repo_root = adapter_root.ascend.find do |path|
  (path / ".git").directory? ||
    (path / ".git").file? ||
    ((path / "plugins/teamharness/remote/claude-code").directory? && (path / "changelog").directory?)
end || remote_root.parent.parent.parent
assets_root = adapter_root / "assets"
manifest_path = assets_root / "plugin.yaml"
node_worker_root = adapter_root / "node-worker"
node_mcp_root = adapter_root / "node-mcp"
node_hooks_root = adapter_root / "node-hooks"
node_runtime_core_root = repo_root / "plugins/teamharness/remote/node-runtime-core"
out_dir = Pathname.new(ENV["OUT_DIR"] || (repo_root / "dist/teamharness/remote/claude-code").to_s).expand_path

abort("missing claude-code adapter: #{adapter_root}") unless adapter_root.directory?
abort("missing claude-code assets: #{assets_root}") unless assets_root.directory?
abort("missing claude-code assets manifest: #{manifest_path}") unless manifest_path.file?
abort("missing claude-code node worker: #{node_worker_root}") unless node_worker_root.directory?
abort("missing claude-code node mcp: #{node_mcp_root}") unless node_mcp_root.directory?
abort("missing claude-code node hooks: #{node_hooks_root}") unless node_hooks_root.directory?
abort("missing node runtime core: #{node_runtime_core_root}") unless node_runtime_core_root.directory?

manifest = YAML.load_file(manifest_path)
name = manifest.fetch("metadata").fetch("name")
version = manifest.fetch("metadata").fetch("version").to_s
package_name = "#{name}-claude-local-#{version}"
runtime_bundle_version = "0.0.2"

def copy_tree(source, target)
  abort("missing source: #{source}") unless source.exist?

  FileUtils.mkdir_p(target)
  entries = Dir.glob((source / "*").to_s, File::FNM_DOTMATCH).reject do |path|
    [".", ".."].include?(File.basename(path))
  end
  FileUtils.cp_r(entries, target) unless entries.empty?
end

def copy_entry(source_root, target_root, entry)
  src = source_root / entry
  abort("missing claude local bundle source: #{src}") unless src.exist?

  dst = target_root / entry
  if src.directory?
    copy_tree(src, dst)
  else
    FileUtils.mkdir_p(dst.dirname)
    FileUtils.cp(src, dst)
  end
end

def copy_claude_skills(source_root, claude_plugin, manifest)
  target_root = claude_plugin / "skills"
  target_root.mkpath

  (manifest.fetch("skills", {}) || {}).each_value do |entries|
    (entries || []).each do |entry|
      next unless entry.is_a?(Hash)

      skill_id = entry.fetch("id").to_s
      abort("invalid TeamHarness skill id for Claude Code bundle: #{skill_id.inspect}") unless skill_id.match?(/\A[\w.-]+\z/)

      src = source_root / entry.fetch("path").to_s
      abort("missing TeamHarness skill source for Claude Code bundle: #{src}") unless (src / "SKILL.md").file?

      dst = target_root / skill_id
      abort("duplicate TeamHarness skill id for Claude Code bundle: #{skill_id}") if dst.exist?

      copy_tree(src, dst)
    end
  end
end

def prune_generated(path)
  Dir.glob((path / "**/*").to_s, File::FNM_DOTMATCH).each do |item|
    base = File.basename(item)
    FileUtils.rm_rf(item) if base == "__pycache__" || base == ".DS_Store" || base.end_with?(".pyc") || base.end_with?(".test.js")
  end
end

def write_json(path, payload)
  FileUtils.mkdir_p(path.dirname)
  path.write(JSON.pretty_generate(payload) + "\n", mode: "w", encoding: "UTF-8")
end

def write_executable(path, content)
  FileUtils.mkdir_p(path.dirname)
  path.write(content, mode: "w", encoding: "UTF-8")
  FileUtils.chmod(0o755, path)
end

def tar_dir(root, package_name, out_path)
  FileUtils.rm_f(out_path)
  stdout, stderr, status = Open3.capture3("tar", "-C", root.to_s, "-czf", out_path.to_s, package_name)
  abort("tar failed: #{stderr}#{stdout}") unless status.success?
end

def install_node_dependencies(package_root)
  return unless (package_root / "package.json").file?

  env = {
    "npm_config_audit" => "false",
    "npm_config_fund" => "false"
  }
  stdout, stderr, status = Open3.capture3(
    env,
    "npm", "ci", "--omit=dev", "--ignore-scripts", "--no-audit", "--no-fund",
    chdir: package_root.to_s
  )
  abort("npm ci failed in #{package_root}: #{stderr}#{stdout}") unless status.success?
end

out_dir.mkpath
out_tar = out_dir / "agentteams-claude-code-local-runtime-#{runtime_bundle_version}.tar.gz"

Dir.mktmpdir("teamharness-claude-local-") do |tmp|
  tmp_root = Pathname.new(tmp)
  staging = tmp_root / package_name
  claude_plugin = staging / "teamharness-claude-plugin"
  teamharness_assets = claude_plugin / "teamharness-assets"

  staging.mkpath
  claude_plugin.mkpath
  teamharness_assets.mkpath

  copy_entry(assets_root, claude_plugin, "prompts")
  copy_claude_skills(assets_root, claude_plugin, manifest)
  copy_entry(assets_root, teamharness_assets, "plugin.yaml")
  copy_tree(node_mcp_root, teamharness_assets / "mcp")
  copy_tree(node_runtime_core_root, staging / "node-runtime-core")
  install_node_dependencies(staging / "node-runtime-core")
  copy_tree(node_hooks_root, claude_plugin / "hooks")
  worker_cli = node_worker_root / "src/cli.js"
  abort("missing claude-code node worker cli: #{worker_cli}") unless worker_cli.file?
  FileUtils.mkdir_p(staging / "claude-code-worker/dist")
  FileUtils.cp(worker_cli, staging / "claude-code-worker/dist/cli.js")
  FileUtils.chmod(0o755, staging / "claude-code-worker/dist/cli.js")

  write_json(
    claude_plugin / ".claude-plugin/plugin.json",
    {
      "name" => "teamharness-claude-code",
      "description" => "HiClaw TeamHarness Claude Code local plugin with team prompts, skills, MCP tools, and runtime-local hooks.",
      "version" => version,
      "author" => {
        "name" => "HiClaw"
      },
      "mcpServers" => "./.mcp.json"
    }
  )

  write_json(
    claude_plugin / ".mcp.json",
    {
      "mcpServers" => {
        "teamharness" => {
          "command" => "sh",
          "args" => [
            "-c",
            "exec \"${TEAMHARNESS_NODE_BIN:-node}\" \"${CLAUDE_PLUGIN_ROOT}/teamharness-assets/mcp/server.js\""
          ],
          "cwd" => "${CLAUDE_PLUGIN_ROOT}/teamharness-assets",
          "env" => {
            "PATH" => "${PATH}",
            "TEAMHARNESS_NODE_RUNTIME_CORE_DIR" => "${TEAMHARNESS_NODE_RUNTIME_CORE_DIR}"
          }
        }
      }
    }
  )

  write_json(
    claude_plugin / "hooks/hooks.json",
    {
      "hooks" => {
        "SessionStart" => [
          {
            "hooks" => [
              {
                "type" => "command",
                "command" => "\"${TEAMHARNESS_NODE_BIN:-node}\" \"${CLAUDE_PLUGIN_ROOT}/hooks/team-context.js\"",
                "timeout" => 5
              }
            ]
          }
        ],
        "PreToolUse" => [
          {
            "matcher" => "Bash|Read|Write|Edit|MultiEdit",
            "hooks" => [
              {
                "type" => "command",
                "command" => "\"${TEAMHARNESS_NODE_BIN:-node}\" \"${CLAUDE_PLUGIN_ROOT}/hooks/credential-guard.js\"",
                "timeout" => 5
              }
            ]
          }
        ],
        "PostToolUse" => [
          {
            "hooks" => [
              {
                "type" => "command",
                "command" => "\"${TEAMHARNESS_NODE_BIN:-node}\" \"${CLAUDE_PLUGIN_ROOT}/hooks/output-sanitizer.js\"",
                "timeout" => 5,
                "async" => true
              }
            ]
          }
        ]
      }
    }
  )

  write_executable(
    staging / "scripts/install.sh",
    <<~SH
      #!/usr/bin/env bash
      set -euo pipefail

      bundle_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
      default_root="${PILOT_DATA:-${HOME}/.local/share/teamharness}"
      target_dir="${TEAMHARNESS_CLAUDE_PLUGIN_DIR:-${default_root}/plugins/teamharness-claude-plugin}"
      tmp_dir="${target_dir}.tmp"

      rm -rf "${tmp_dir}"
      mkdir -p "${tmp_dir}"
      (cd "${bundle_dir}/teamharness-claude-plugin" && tar -cf - .) | (cd "${tmp_dir}" && tar -xf -)
      rm -rf "${target_dir}"
      mv "${tmp_dir}" "${target_dir}"

      if [ -n "${TEAMHARNESS_INSTALL_LOG:-}" ]; then
        mkdir -p "$(dirname "${TEAMHARNESS_INSTALL_LOG}")"
        printf '{"event":"install","runtime":"claude-code","pluginDir":"%s"}\\n' "${target_dir}" >> "${TEAMHARNESS_INSTALL_LOG}"
      fi
    SH
  )

  write_executable(
    staging / "scripts/uninstall.sh",
    <<~SH
      #!/usr/bin/env bash
      set -euo pipefail

      default_root="${PILOT_DATA:-${HOME}/.local/share/teamharness}"
      target_dir="${TEAMHARNESS_CLAUDE_PLUGIN_DIR:-${default_root}/plugins/teamharness-claude-plugin}"
      rm -rf "${target_dir}"

      if [ -n "${TEAMHARNESS_INSTALL_LOG:-}" ]; then
        mkdir -p "$(dirname "${TEAMHARNESS_INSTALL_LOG}")"
        printf '{"event":"uninstall","runtime":"claude-code","pluginDir":"%s"}\\n' "${target_dir}" >> "${TEAMHARNESS_INSTALL_LOG}"
      fi
    SH
  )

  write_executable(
    staging / "scripts/worker-entrypoint.sh",
    <<~SH
      #!/usr/bin/env bash
      set -euo pipefail

      bundle_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
      node_bin="${PILOT_NODE_BIN:-node}"
      export TEAMHARNESS_NODE_BIN="${TEAMHARNESS_NODE_BIN:-${node_bin}}"
      export TEAMHARNESS_CLAUDE_PLUGIN_DIR="${TEAMHARNESS_CLAUDE_PLUGIN_DIR:-${bundle_dir}/teamharness-claude-plugin}"
      export TEAMHARNESS_NODE_RUNTIME_CORE_DIR="${TEAMHARNESS_NODE_RUNTIME_CORE_DIR:-${bundle_dir}/node-runtime-core}"
      export TEAMHARNESS_STATE_DIR="${TEAMHARNESS_STATE_DIR:-${bundle_dir}/.agentteams/runtime/claude-code}"

      normalize_args() {
        local -a normalized=()
        while [ "$#" -gt 0 ]; do
          case "$1" in
            --plugin-install-scope)
              if [ "$#" -ge 2 ] && [ -z "${2:-}" ]; then
                normalized+=("--plugin-install-scope" "local")
                shift 2
              elif [ "$#" -ge 2 ] && [[ "${2}" != --* ]]; then
                normalized+=("--plugin-install-scope" "$2")
                shift 2
              else
                normalized+=("--plugin-install-scope" "local")
                shift
              fi
              ;;
            --model-config-mode)
              if [ "$#" -ge 2 ] && [ -z "${2:-}" ]; then
                normalized+=("--model-config-mode" "native-config")
                shift 2
              elif [ "$#" -ge 2 ] && [[ "${2}" != --* ]]; then
                normalized+=("--model-config-mode" "$2")
                shift 2
              else
                normalized+=("--model-config-mode" "native-config")
                shift
              fi
              ;;
            *)
              normalized+=("$1")
              shift
              ;;
          esac
        done
        set -- "${normalized[@]}"
        exec "${node_bin}" "${bundle_dir}/claude-code-worker/dist/cli.js" "$@"
      }

      normalize_args "$@"
    SH
  )

  write_json(
    staging / "worker.manifest.json",
    {
      "name" => "teamharness-claude-code-worker",
      "runtime" => "claude-code",
      "version" => version,
      "command" => [
        "scripts/worker-entrypoint.sh",
        "--bootstrap-token-file",
        "${instance:bootstrapTokenFile}",
        "--work-dir",
        "${instance:workDir}",
        "--instance-id",
        "${instance:id}",
        "--plugin-install-scope",
        "${instance:pluginInstallScope}",
        "--model-config-mode",
        "${instance:modelConfigMode}"
      ],
      "cwd" => ".",
      "pluginDir" => "teamharness-claude-plugin",
      "env" => {
        "TEAMHARNESS_CLAUDE_PLUGIN_DIR" => "${destDir}/teamharness-claude-plugin",
        "TEAMHARNESS_NODE_RUNTIME_CORE_DIR" => "${destDir}/node-runtime-core",
        "TEAMHARNESS_CLAUDE_COMMAND" => "${instance:claudeCommand}",
        "TEAMHARNESS_CLAUDE_PERMISSION_MODE" => "${instance:claudePermissionMode}",
        "TEAMHARNESS_STATE_DIR" => "${instance:stateDir}"
      },
      "paths" => {
        "pid" => "${instance:stateDir}/worker.pid",
        "status" => "${instance:stateDir}/supervisor-status.json",
        "log" => "${instance:logDir}/worker.log"
      },
      "restartPolicy" => {
        "type" => "on-failure",
        "maxRestarts" => 5,
        "backoffSeconds" => 3
      },
      "config" => {
        "stateDir" => ".agentteams/runtime/claude-code"
      }
    }
  )

  prune_generated(staging)
  tar_dir(tmp_root, package_name, out_tar)
end

puts out_tar
