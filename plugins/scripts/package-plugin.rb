#!/usr/bin/env ruby
# frozen_string_literal: true

require "digest"
require "fileutils"
require "json"
require "open3"
require "pathname"
require "rbconfig"
require "tmpdir"
require "yaml"

manifest_path = Pathname.new(ARGV[0] || "plugins/teamharness/plugin.yaml").expand_path
plugin_root = manifest_path.dirname
repo_root = plugin_root.ascend.find { |path| (path / ".git").exist? } || plugin_root
out_dir = Pathname.new(ENV["OUT_DIR"] || (repo_root / "dist/plugins").to_s).expand_path

validate = Pathname.new(__dir__).expand_path / "validate-plugin.rb"
system(RbConfig.ruby, validate.to_s, manifest_path.to_s) || abort("plugin validation failed")

manifest = YAML.load_file(manifest_path)
metadata = manifest.fetch("metadata")
name = metadata.fetch("name")
version = metadata["version"].to_s
requires = metadata["requires"] || manifest["requires"] || {}
out_dir.mkpath
out_path = out_dir / "#{name}.tar.gz"
FileUtils.rm_f(out_path)

includes = manifest.fetch("package").fetch("include")

def copy_entry(plugin_root, staging_root, entry)
  src = plugin_root / entry
  abort("package include missing: #{src}") unless src.exist?

  dst = staging_root / entry
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

# Directories (at any depth) and file suffixes excluded from the qwenpaw zip.
ZIP_EXCLUDED_DIRS = %w[__pycache__ tests state].freeze

def zip_excluded?(rel)
  parts = rel.split("/")
  parts.any? { |part| ZIP_EXCLUDED_DIRS.include?(part) } ||
    parts.last.end_with?(".pyc") || parts.last.end_with?(".log")
end

# Minimal deterministic ZIP writer (stored entries, fixed 1980-01-01 timestamps)
# so the qwenpaw zip needs no gems beyond the stdlib.
file_count = 0
Dir.mktmpdir("teamharness-plugin-") do |tmp|
  staging = Pathname.new(tmp) / name
  staging.mkpath
  includes.each { |entry| copy_entry(plugin_root, staging, entry) }
  prune_generated(staging)

  file_count = Dir.glob((staging / "**/*").to_s, File::FNM_DOTMATCH).count do |item|
    next false if File.directory?(item)
    rel = Pathname.new(item).relative_path_from(staging).to_s
    !zip_excluded?(rel)
  end

  python = <<~PY
    import os, tarfile
    root = #{staging.to_s.dump}
    out = #{out_path.to_s.dump}
    with tarfile.open(out, "w:gz") as tf:
        for dirpath, dirnames, filenames in os.walk(root):
            dirnames[:] = [d for d in dirnames if d != "__pycache__" and d != ".DS_Store"]
            for filename in sorted(filenames):
                if filename == ".DS_Store" or filename.startswith("._") or filename.endswith(".pyc"):
                    continue
                path = os.path.join(dirpath, filename)
                rel = os.path.relpath(path, root)
                tf.add(path, arcname=rel)
  PY
  stdout, stderr, status = Open3.capture3("python3", "-c", python)
  abort("python tar failed: #{stderr}#{stdout}") unless status.success?
end

# The bundle tar.gz is runtime-neutral; the QwenPaw-installable artifact is
# produced by the adapter build script (plugin id + adapter payload + core).
adapter_build = plugin_root / "adapters/qwenpaw/scripts/build-qwenpaw-plugin.rb"
if adapter_build.file?
  stdout, stderr, status = Open3.capture3(
    { "OUT_DIR" => out_dir.to_s },
    "ruby", adapter_build.to_s
  )
  abort("qwenpaw adapter build failed: #{stderr}#{stdout}") unless status.success?
end

commit = begin
  stdout, _stderr, status = Open3.capture3("git", "-C", repo_root.to_s, "rev-parse", "HEAD")
  status.success? ? stdout.strip : ""
rescue StandardError
  ""
end

artifacts = Dir.glob((out_dir / "*").to_s).sort.select do |path|
  File.file?(path) && (path.end_with?(".tar.gz") || path.end_with?(".zip"))
end.map do |path|
  { name: File.basename(path), sha256: Digest::SHA256.file(path).hexdigest }
end

File.write(
  out_dir / "sha256sums.txt",
  artifacts.map { |artifact| "#{artifact[:sha256]}  #{artifact[:name]}" }.join("\n") + "\n"
)

release_manifest = {
  name: name,
  version: version,
  commit: commit,
  files: file_count,
  artifacts: artifacts,
  requires: requires
}
File.write(out_dir / "release-manifest.json", "#{JSON.pretty_generate(release_manifest)}\n")

puts out_path
