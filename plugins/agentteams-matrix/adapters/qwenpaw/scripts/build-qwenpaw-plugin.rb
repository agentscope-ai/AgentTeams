#!/usr/bin/env ruby
# frozen_string_literal: true

# Build the QwenPaw-installable plugin package for the AgentTeams Matrix
# channel adapter. The bundle itself (plugin.json type=bundle) is not a
# QwenPaw plugin; the channel plugin id is agentteams-matrix-channel and it
# ships the adapter payload plus the shared core/ package.
#
# Usage: OUT_DIR=dist ruby build-qwenpaw-plugin.rb
# Output: <OUT_DIR>/agentteams-matrix-channel-<version>.zip

require "fileutils"
require "json"
require "open3"
require "pathname"
require "tmpdir"

adapter_root = Pathname.new(__dir__).parent
bundle_root = adapter_root.parent.parent
out_dir = Pathname.new(ENV["OUT_DIR"] || (bundle_root / "dist/adapters/qwenpaw").to_s).expand_path

manifest_path = adapter_root / "plugin.json"
abort("missing adapter plugin.json: #{manifest_path}") unless manifest_path.file?
manifest = JSON.parse(manifest_path.read)
plugin_id = manifest.fetch("id")
version = manifest.fetch("version")
package_name = "#{plugin_id}-#{version}"

def copy_entry(source_root, target_root, entry)
  src = source_root / entry
  abort("missing package source: #{src}") unless src.exist?
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

Dir.mktmpdir("agentteams-matrix-qwenpaw-") do |tmp|
  tmp_root = Pathname.new(tmp)
  staging = tmp_root / package_name
  staging.mkpath

  excluded_files = %w[install.sh uninstall.sh README.md]
  excluded_dirs = %w[.agentteams __pycache__ node_modules]
  adapter_root.children.sort.each do |path|
    rel = path.relative_path_from(adapter_root)
    next if rel.to_s == "scripts"
    next if path.file? && excluded_files.include?(rel.to_s)
    next if path.directory? && excluded_dirs.include?(rel.to_s)
    copy_entry(adapter_root, staging, rel.to_s)
  end

  core_dir = bundle_root / "core"
  copy_entry(bundle_root, staging, "core") if core_dir.directory?

  prune_generated(staging)

  generated = JSON.parse((staging / "plugin.json").read)
  abort("plugin id mismatch: #{generated['id']}") unless generated["id"] == plugin_id
  abort("plugin entry must be plugin.py") unless generated.dig("entry", "backend") == "plugin.py"

  # Syntax-check the payload with the bytecode written OUTSIDE staging, so
  # the packaged ZIP can never contain __pycache__ or .pyc entries.
  compile_dir = tmp_root / "compile-check"
  compile_dir.mkpath
  %w[plugin.py].each do |file|
    path = staging / file
    next unless path.file?
    cfile = compile_dir / "#{file}.pyc"
    python = <<~PY
      import py_compile
      py_compile.compile(#{path.to_s.dump}, cfile=#{cfile.to_s.dump}, doraise=True)
    PY
    stdout, stderr, status = Open3.capture3("python3", "-c", python)
    abort("py_compile failed for #{file}: #{stderr}#{stdout}") unless status.success?
  end

  zip_dir(tmp_root, package_name, out_zip)
end

puts out_zip
