#!/usr/bin/env ruby
# frozen_string_literal: true

require "fileutils"
require "open3"
require "pathname"
require "rbconfig"
require "tmpdir"
require "yaml"

manifest_path = Pathname.new(ARGV[0] || "plugins/teamharness/plugin.yaml").expand_path
plugin_root = manifest_path.dirname
repo_root = plugin_root.ascend.find { |path| (path / ".git").directory? } || plugin_root
out_dir = Pathname.new(ENV["OUT_DIR"] || (repo_root / "dist/plugins").to_s).expand_path

validate = Pathname.new(__dir__).expand_path / "validate-plugin.rb"
system(RbConfig.ruby, validate.to_s, manifest_path.to_s) || abort("plugin validation failed")

manifest = YAML.load_file(manifest_path)
name = manifest.fetch("metadata").fetch("name")
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

Dir.mktmpdir("teamharness-plugin-") do |tmp|
  staging = Pathname.new(tmp) / name
  staging.mkpath
  includes.each { |entry| copy_entry(plugin_root, staging, entry) }
  prune_generated(staging)

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

puts out_path
