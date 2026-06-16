#!/usr/bin/env ruby
# frozen_string_literal: true

require "fileutils"
require "json"
require "open3"
require "pathname"
require "tmpdir"
require "yaml"

manifest_path = Pathname.new(ARGV[0] || "plugins/teamharness/plugin.yaml").expand_path
plugin_root = manifest_path.dirname
repo_root = plugin_root.ascend.find { |path| (path / ".git").directory? } || plugin_root
adapter_root = plugin_root / "adapters/qwenpaw"
out_dir = Pathname.new(ENV["OUT_DIR"] || (repo_root / "dist/adapters/qwenpaw").to_s).expand_path

abort("missing manifest: #{manifest_path}") unless manifest_path.file?
abort("missing qwenpaw adapter: #{adapter_root}") unless adapter_root.directory?

manifest = YAML.load_file(manifest_path)
name = manifest.fetch("metadata").fetch("name")
version = manifest.fetch("metadata").fetch("version")
package_name = "#{name}-qwenpaw-#{version}"

def copy_entry(source_root, target_root, entry)
  src = source_root / entry
  abort("missing qwenpaw package source: #{src}") unless src.exist?

  dst = target_root / entry
  if src.directory?
    FileUtils.mkdir_p(dst)
    entries = Dir.glob((src / "*").to_s, File::FNM_DOTMATCH).reject do |path|
      [".", ".."].include?(File.basename(path))
    end
    FileUtils.cp_r(entries, dst)
  else
    FileUtils.mkdir_p(dst.dirname)
    FileUtils.cp(src, dst)
  end
end

def prune_generated(path)
  Dir.glob((path / "**/*").to_s, File::FNM_DOTMATCH).each do |item|
    base = File.basename(item)
    FileUtils.rm_rf(item) if base == "__pycache__" || base == ".DS_Store" || base.end_with?(".pyc")
  end
end

def zip_dir(root, package_name, out_path)
  FileUtils.rm_f(out_path)
  if system("zip", "-v", out: File::NULL, err: File::NULL)
    Dir.chdir(root) do
      system("zip", "-qry", out_path.to_s, package_name) || abort("zip failed")
    end
    return
  end

  python = <<~PY
    import os, zipfile
    root = #{root.to_s.dump}
    package = #{package_name.dump}
    out = #{out_path.to_s.dump}
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
        base = os.path.join(root, package)
        for dirpath, _, filenames in os.walk(base):
            for filename in filenames:
                path = os.path.join(dirpath, filename)
                rel = os.path.relpath(path, root)
                zf.write(path, rel)
  PY
  stdout, stderr, status = Open3.capture3("python3", "-c", python)
  abort("python zip failed: #{stderr}#{stdout}") unless status.success?
end

out_dir.mkpath
out_zip = out_dir / "#{package_name}.zip"
stable_zip = out_dir / "teamharness-qwenpaw.zip"

Dir.mktmpdir("teamharness-qwenpaw-") do |tmp|
  tmp_root = Pathname.new(tmp)
  staging = tmp_root / package_name
  asset_dir = staging / "teamharness"
  staging.mkpath
  asset_dir.mkpath

  %w[
    plugin.yaml
    prompts
    skills
    mcp
  ].each do |entry|
    copy_entry(plugin_root, asset_dir, entry)
  end

  copy_entry(adapter_root, staging, "plugin.py")

  qwenpaw_manifest = {
    "id" => "teamharness",
    "name" => "TeamHarness",
    "version" => version,
    "type" => "general",
    "description" => "HiClaw TeamHarness runtime plugin for QwenPaw.",
    "author" => "HiClaw",
    "entry" => {
      "backend" => "plugin.py"
    },
    "dependencies" => [],
    "min_version" => "1.1.7",
    "meta" => {
      "category" => "teamharness",
      "features" => [
        "team-context",
        "tool-output-sanitizer",
        "teamharness-skills",
        "teamharness-mcp"
      ]
    }
  }
  (staging / "plugin.json").write(
    JSON.pretty_generate(qwenpaw_manifest) + "\n",
    mode: "w",
    encoding: "UTF-8"
  )

  prune_generated(staging)

  validate = adapter_root / "scripts/validate-qwenpaw-plugin.rb"
  system("ruby", validate.to_s, staging.to_s) || abort("qwenpaw plugin validation failed")

  zip_dir(tmp_root, package_name, out_zip)
  FileUtils.cp(out_zip, stable_zip)
end

puts out_zip
