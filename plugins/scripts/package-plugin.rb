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
require "zlib"

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
zip_path = out_dir / "#{name}-#{version}-qwenpaw.zip"
FileUtils.rm_f(zip_path)

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
def write_zip(path, entries)
  dos_time = 0
  dos_date = 0x0021 # 1980-01-01
  local = String.new(encoding: Encoding::BINARY)
  central = String.new(encoding: Encoding::BINARY)
  offset = 0
  entries.each do |entry_name, data|
    name_bytes = entry_name.b
    crc = Zlib.crc32(data)
    local << [0x04034b50, 20, 0, 0, dos_time, dos_date, crc, data.bytesize, data.bytesize,
              name_bytes.bytesize, 0].pack("VvvvvvVVVvv")
    local << name_bytes << data
    central << [0x02014b50, 20, 20, 0, 0, dos_time, dos_date, crc, data.bytesize, data.bytesize,
                name_bytes.bytesize, 0, 0, 0, 0, 0, offset].pack("VvvvvvvVVVvvvvvVV")
    central << name_bytes
    offset += local.bytesize
  end
  eocd = [0x06054b50, 0, 0, entries.size, entries.size, central.bytesize, offset, 0].pack("VvvvvVVv")
  File.binwrite(path, local + central + eocd)
end

zip_entries = []
Dir.mktmpdir("teamharness-plugin-") do |tmp|
  staging = Pathname.new(tmp) / name
  staging.mkpath
  includes.each { |entry| copy_entry(plugin_root, staging, entry) }
  prune_generated(staging)

  zip_entries = Dir.glob((staging / "**/*").to_s, File::FNM_DOTMATCH).sort.map do |item|
    next if File.directory?(item)
    rel = Pathname.new(item).relative_path_from(staging).to_s
    next if zip_excluded?(rel)
    ["#{name}/#{rel}", File.binread(item)]
  end.compact

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

write_zip(zip_path, zip_entries)

commit = begin
  stdout, _stderr, status = Open3.capture3("git", "-C", repo_root.to_s, "rev-parse", "HEAD")
  status.success? ? stdout.strip : ""
rescue StandardError
  ""
end

artifacts = [out_path, zip_path].map do |path|
  { name: path.basename.to_s, sha256: Digest::SHA256.file(path.to_s).hexdigest }
end

File.write(
  out_dir / "sha256sums.txt",
  artifacts.map { |artifact| "#{artifact[:sha256]}  #{artifact[:name]}" }.join("\n") + "\n"
)

release_manifest = {
  name: name,
  version: version,
  commit: commit,
  files: zip_entries.size,
  artifacts: artifacts,
  requires: requires
}
File.write(out_dir / "release-manifest.json", "#{JSON.pretty_generate(release_manifest)}\n")

puts out_path
