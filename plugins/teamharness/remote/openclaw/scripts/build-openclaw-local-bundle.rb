#!/usr/bin/env ruby
# frozen_string_literal: true

require "fileutils"
require "digest"
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
    ((path / "plugins/teamharness/remote/openclaw").directory? && (path / "changelog").directory?)
end || remote_root.parent.parent.parent
assets_root = adapter_root / "assets"
manifest_path = assets_root / "plugin.yaml"
node_worker_root = adapter_root / "node-worker"
node_mcp_root = adapter_root / "node-mcp"
loongsuite_js_repo = ENV.fetch("LOONGSUITE_JS_REPO", "https://github.com/alibaba/loongsuite-js.git")
loongsuite_js_ref = ENV.fetch("LOONGSUITE_JS_REF", "25c7f8b074dc434818bca78b9f66deb3e2466284")
node_runtime_core_root = repo_root / "plugins/teamharness/remote/node-runtime-core"
out_dir = Pathname.new(ENV["OUT_DIR"] || (repo_root / "dist/teamharness/remote/openclaw").to_s).expand_path

abort("missing openclaw adapter: #{adapter_root}") unless adapter_root.directory?
abort("missing openclaw assets: #{assets_root}") unless assets_root.directory?
abort("missing openclaw assets manifest: #{manifest_path}") unless manifest_path.file?
abort("missing openclaw node worker: #{node_worker_root}") unless node_worker_root.directory?
abort("missing openclaw node mcp: #{node_mcp_root}") unless node_mcp_root.directory?
abort("missing node runtime core: #{node_runtime_core_root}") unless node_runtime_core_root.directory?

manifest = YAML.load_file(manifest_path)
name = manifest.fetch("metadata").fetch("name")
version = manifest.fetch("metadata").fetch("version").to_s
package_name = "#{name}-openclaw-local-#{version}"
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
  abort("missing OpenClaw local bundle source: #{src}") unless src.exist?

  dst = target_root / entry
  if src.directory?
    copy_tree(src, dst)
  else
    FileUtils.mkdir_p(dst.dirname)
    FileUtils.cp(src, dst)
  end
end

def copy_skills(source_root, target_root, manifest)
  skills_root = target_root / "skills"
  skills_root.mkpath

  (manifest.fetch("skills", {}) || {}).each_value do |entries|
    (entries || []).each do |entry|
      next unless entry.is_a?(Hash)

      skill_id = entry.fetch("id").to_s
      abort("invalid TeamHarness skill id for OpenClaw bundle: #{skill_id.inspect}") unless skill_id.match?(/\A[\w.-]+\z/)

      src = source_root / entry.fetch("path").to_s
      abort("missing TeamHarness skill source for OpenClaw bundle: #{src}") unless (src / "SKILL.md").file?

      dst = skills_root / skill_id
      abort("duplicate TeamHarness skill id for OpenClaw bundle: #{skill_id}") if dst.exist?

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

def directory_digest(root)
  digest = Digest::SHA256.new
  entries = Dir.glob((root / "**/*").to_s, File::FNM_DOTMATCH).reject do |path|
    [".", ".."].include?(File.basename(path))
  end
  entries.sort.each do |entry|
    relative = Pathname.new(entry).relative_path_from(root).to_s
    stat = File.lstat(entry)
    if stat.symlink?
      digest.update("link:#{relative}:#{File.readlink(entry)}\n")
    elsif stat.directory?
      digest.update("dir:#{relative}\n")
    elsif stat.file?
      digest.update("file:#{relative}\n")
      digest.update(File.binread(entry))
      digest.update("\n")
    end
  end
  "sha256:#{digest.hexdigest}"
end

def install_node_dependencies(package_root, omit_dev: true, ci: true)
  return unless (package_root / "package.json").file?

  env = {
    "npm_config_audit" => "false",
    "npm_config_fund" => "false"
  }
  command = ["npm", ci ? "ci" : "install", "--ignore-scripts", "--no-audit", "--no-fund"]
  command << "--omit=dev" if omit_dev
  stdout, stderr, status = Open3.capture3(
    env,
    *command,
    chdir: package_root.to_s
  )
  abort("npm #{ci ? "ci" : "install"} failed in #{package_root}: #{stderr}#{stdout}") unless status.success?
end

def run_npm_script(package_root, script)
  stdout, stderr, status = Open3.capture3(
    {
      "npm_config_audit" => "false",
      "npm_config_fund" => "false"
    },
    "npm", "run", script,
    chdir: package_root.to_s
  )
  abort("npm run #{script} failed in #{package_root}: #{stderr}#{stdout}") unless status.success?
end

def fetch_loongsuite_js_source(target_root, repo, ref)
  FileUtils.rm_rf(target_root)
  FileUtils.mkdir_p(target_root)
  [
    ["git", "init", "-q"],
    ["git", "remote", "add", "origin", repo],
    ["git", "fetch", "--depth", "1", "origin", ref],
    ["git", "checkout", "--detach", "FETCH_HEAD", "-q"]
  ].each do |command|
    stdout, stderr, status = Open3.capture3(*command, chdir: target_root.to_s)
    abort("loongsuite-js git command failed in #{target_root}: #{command.join(" ")}\n#{stderr}#{stdout}") unless status.success?
  end
end

def loongsuite_js_source(tmp_root, repo, ref)
  override = ENV.fetch("LOONGSUITE_JS_SOURCE", "").strip
  if override != ""
    source = Pathname.new(override).expand_path
    abort("missing LOONGSUITE_JS_SOURCE: #{source}") unless source.directory?
  else
    source = tmp_root / "loongsuite-js-source"
    fetch_loongsuite_js_source(source, repo, ref)
  end

  %w[
    opentelemetry-util-genai
    opentelemetry-instrumentation-openclaw
  ].each do |entry|
    abort("missing loongsuite-js package source: #{source / entry}") unless (source / entry).directory?
  end
  source
end

def use_local_loongsuite_util_dependency(package_root)
  package_path = package_root / "package.json"
  package = JSON.parse(package_path.read)
  package["dependencies"] ||= {}
  package["dependencies"]["@loongsuite/opentelemetry-util-genai"] = "file:../opentelemetry-util-genai"
  write_json(package_path, package)
end

def prune_node_dependencies(package_root)
  stdout, stderr, status = Open3.capture3(
    {
      "npm_config_audit" => "false",
      "npm_config_fund" => "false"
    },
    "npm", "prune", "--omit=dev", "--ignore-scripts", "--no-audit", "--no-fund",
    chdir: package_root.to_s
  )
  abort("npm prune failed in #{package_root}: #{stderr}#{stdout}") unless status.success?
end

def prune_loongsuite_openclaw_sources(package_root)
  %w[
    .gitignore
    CHANGELOG.md
    CONTRIBUTING.md
    README.md
    index.ts
    scripts
    src
    test
    tsconfig.json
    vitest.config.ts
  ].each do |entry|
    FileUtils.rm_rf(package_root / entry)
  end
end

def normalize_loongsuite_openclaw_package(package_root)
  package_path = package_root / "package.json"
  package = JSON.parse(package_path.read)
  package["openclaw"] = { "extensions" => ["./dist/index.js"] }
  write_json(package_path, package)
end

def prune_loongsuite_util_sources(package_root)
  %w[
    CHANGELOG.md
    README.md
    README_CN.md
    src
    test
    tsconfig.json
    vitest.config.ts
  ].each do |entry|
    FileUtils.rm_rf(package_root / entry)
  end
end

out_dir.mkpath
out_tar = out_dir / "agentteams-openclaw-local-runtime-#{runtime_bundle_version}.tar.gz"

Dir.mktmpdir("teamharness-openclaw-local-") do |tmp|
  tmp_root = Pathname.new(tmp)
  staging = tmp_root / package_name
  assets = staging / "teamharness-openclaw-assets"

  staging.mkpath
  assets.mkpath

  copy_entry(assets_root, assets, "prompts")
  copy_entry(assets_root, assets, "plugin.yaml")
  copy_skills(assets_root, assets, manifest)
  copy_tree(node_mcp_root, assets / "mcp")
  copy_tree(node_runtime_core_root, staging / "node-runtime-core")
  install_node_dependencies(staging / "node-runtime-core")

  loongsuite_source = loongsuite_js_source(tmp_root, loongsuite_js_repo, loongsuite_js_ref)
  loongsuite_staging = staging / "loongsuite-js"
  loongsuite_staging.mkpath
  copy_tree(loongsuite_source / "opentelemetry-util-genai", loongsuite_staging / "opentelemetry-util-genai")
  copy_tree(loongsuite_source / "opentelemetry-instrumentation-openclaw", loongsuite_staging / "opentelemetry-instrumentation-openclaw")
  loongsuite_util_bundle = staging / "loongsuite-js/opentelemetry-util-genai"
  loongsuite_openclaw_bundle = staging / "loongsuite-js/opentelemetry-instrumentation-openclaw"
  install_node_dependencies(loongsuite_util_bundle, omit_dev: false)
  run_npm_script(loongsuite_util_bundle, "build")
  FileUtils.rm_rf(loongsuite_util_bundle / "node_modules")
  prune_loongsuite_util_sources(loongsuite_util_bundle)
  use_local_loongsuite_util_dependency(loongsuite_openclaw_bundle)
  install_node_dependencies(loongsuite_openclaw_bundle, omit_dev: false, ci: false)
  run_npm_script(loongsuite_openclaw_bundle, "build")
  prune_node_dependencies(loongsuite_openclaw_bundle)
  prune_loongsuite_openclaw_sources(loongsuite_openclaw_bundle)
  normalize_loongsuite_openclaw_package(loongsuite_openclaw_bundle)
  FileUtils.ln_s("opentelemetry-instrumentation-openclaw/node_modules", staging / "loongsuite-js/node_modules")
  (staging / "loongsuite-js/.teamharness-source-digest").write(directory_digest(staging / "loongsuite-js") + "\n")

  worker_cli = node_worker_root / "src/cli.js"
  abort("missing openclaw node worker cli: #{worker_cli}") unless worker_cli.file?
  FileUtils.mkdir_p(staging / "openclaw-worker/dist")
  FileUtils.cp(worker_cli, staging / "openclaw-worker/dist/cli.js")
  FileUtils.chmod(0o755, staging / "openclaw-worker/dist/cli.js")

  write_executable(
    staging / "scripts/install.sh",
    <<~SH
      #!/usr/bin/env bash
      set -euo pipefail

      bundle_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
      default_root="${PILOT_DATA:-${HOME}/.local/share/teamharness}"
      target_dir="${TEAMHARNESS_OPENCLAW_ASSETS_DIR:-${default_root}/plugins/teamharness-openclaw-assets}"
      tmp_dir="${target_dir}.tmp"

      rm -rf "${tmp_dir}"
      mkdir -p "${tmp_dir}"
      (cd "${bundle_dir}/teamharness-openclaw-assets" && tar -cf - .) | (cd "${tmp_dir}" && tar -xf -)
      rm -rf "${target_dir}"
      mv "${tmp_dir}" "${target_dir}"

      if [ -n "${TEAMHARNESS_INSTALL_LOG:-}" ]; then
        mkdir -p "$(dirname "${TEAMHARNESS_INSTALL_LOG}")"
        printf '{"event":"install","runtime":"openclaw","assetsDir":"%s"}\\n' "${target_dir}" >> "${TEAMHARNESS_INSTALL_LOG}"
      fi
    SH
  )

  write_executable(
    staging / "scripts/uninstall.sh",
    <<~SH
      #!/usr/bin/env bash
      set -euo pipefail

      default_root="${PILOT_DATA:-${HOME}/.local/share/teamharness}"
      target_dir="${TEAMHARNESS_OPENCLAW_ASSETS_DIR:-${default_root}/plugins/teamharness-openclaw-assets}"
      rm -rf "${target_dir}"

      if [ -n "${TEAMHARNESS_INSTALL_LOG:-}" ]; then
        mkdir -p "$(dirname "${TEAMHARNESS_INSTALL_LOG}")"
        printf '{"event":"uninstall","runtime":"openclaw","assetsDir":"%s"}\\n' "${target_dir}" >> "${TEAMHARNESS_INSTALL_LOG}"
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
      export TEAMHARNESS_OPENCLAW_ASSETS_DIR="${TEAMHARNESS_OPENCLAW_ASSETS_DIR:-${bundle_dir}/teamharness-openclaw-assets}"
      export TEAMHARNESS_STATE_DIR="${TEAMHARNESS_STATE_DIR:-${bundle_dir}/.agentteams/runtime/openclaw}"

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
        exec "${node_bin}" "${bundle_dir}/openclaw-worker/dist/cli.js" "$@"
      }

      normalize_args "$@"
    SH
  )

  write_json(
    staging / "worker.manifest.json",
    {
      "name" => "teamharness-openclaw-worker",
      "runtime" => "openclaw",
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
      "pluginDir" => "teamharness-openclaw-assets",
      "env" => {
        "TEAMHARNESS_OPENCLAW_ASSETS_DIR" => "${destDir}/teamharness-openclaw-assets",
        "TEAMHARNESS_OPENCLAW_COMMAND" => "${instance:openclawCommand}",
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
        "stateDir" => ".agentteams/runtime/openclaw"
      }
    }
  )

  prune_generated(staging)
  tar_dir(tmp_root, package_name, out_tar)
end

puts out_tar
