#!/usr/bin/env bash
# Regression for install/dev/build-main.sh failure propagation.
#
# The review's failure path: a component Make target exits non-zero while the
# target tag already has a cached image. The old script ignored make's status,
# found the pre-existing image, wrote a manifest with the new SHA + old image
# ID, and printed done. This test runs the real script, a real isolated Git
# worktree, and a real Make target that exits 17; Docker is a read-only stub.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
script="$root/install/dev/build-main.sh"
tmp_root="$(mktemp -d)"
trap 'rm -rf "$tmp_root"' EXIT

host_platform() {
  case "$(uname -m)" in
    x86_64|amd64) echo linux/amd64 ;;
    aarch64|arm64) echo linux/arm64 ;;
    *) echo "" ;;
  esac
}
platform="$(host_platform)"
if [ -z "$platform" ]; then
  echo "SKIP: unsupported host arch for this test" >&2
  exit 0
fi

make_repo() {
  local repo="$1" failing="$2"
  mkdir -p "$repo"
  git -C "$repo" init -q
  git -C "$repo" config user.email test@example.invalid
  git -C "$repo" config user.name test
  cat > "$repo/Makefile" <<EOF
.PHONY: build-agentteams-controller build-manager build-worker build-qwenpaw-worker
build-agentteams-controller:
	@echo controller-build >> \$(BUILD_LOG)
	@if [ "$failing" = "controller" ]; then exit 17; fi
build-manager:
	@echo manager-build >> \$(BUILD_LOG)
	@if [ "$failing" = "manager" ]; then exit 17; fi
build-worker:
	@echo worker-build >> \$(BUILD_LOG)
build-qwenpaw-worker:
	@echo qwenpaw-worker-build >> \$(BUILD_LOG)
EOF
  git -C "$repo" add Makefile
  git -C "$repo" commit -qm init
}

# Docker stub: info ok, any image inspect returns a pre-existing ID, save/push logged.
make_docker_stub() {
  local bin="$1" log="$2"
  mkdir -p "$bin"
  cat > "$bin/docker" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$log"
case "\$1" in
  info) exit 0 ;;
  image)
    [ "\${2:-}" = "inspect" ] && { echo "sha256:pre-existing-image"; exit 0; }
    exit 0
    ;;
  save|push) exit 0 ;;
  run) exit 0 ;;
  *) exit 0 ;;
esac
EOF
  chmod +x "$bin/docker"
}

run_case() {
  local name="$1" component="$2" failing="$3" want_rc="$4" want_manifest="$5"
  shift 5
  local extra=("$@")
  local dir="$tmp_root/$name"
  local repo="$dir/repo" bin="$dir/bin" log="$dir/docker.log" make_log="$dir/make.log"
  mkdir -p "$dir"
  make_repo "$repo" "$failing"
  make_docker_stub "$bin" "$log"
  # Seed a stale success manifest: a failed build must not leave it in place.
  printf '%s\n' '{"stale":true}' > "$repo/build-manifest.json"

  set +e
  (cd "$repo" && PATH="$bin:$PATH" BUILD_LOG="$make_log" bash "$script" \
     --ref HEAD --component "$component" --platform "$platform" \
     --tag "agentteams-dev:existing" "${extra[@]}" >"$dir/out.log" 2>&1)
  local rc=$?
  set -e

  if [ "$want_rc" = "nonzero" ] && [ "$rc" -eq 0 ]; then
    echo "ERROR: $name expected non-zero rc, got 0" >&2
    cat "$dir/out.log" >&2
    return 1
  fi
  if [ "$want_rc" = "zero" ] && [ "$rc" -ne 0 ]; then
    echo "ERROR: $name expected rc 0, got $rc" >&2
    cat "$dir/out.log" >&2
    return 1
  fi

  if [ "$want_manifest" = "absent" ]; then
    if [ -e "$repo/build-manifest.json" ]; then
      echo "ERROR: $name wrote build-manifest.json on a failed build" >&2
      return 1
    fi
    if grep -q '==> done' "$dir/out.log"; then
      echo "ERROR: $name printed success on a failed build" >&2
      return 1
    fi
    if grep -Eq '^(save|push)' "$log"; then
      echo "ERROR: $name saved/pushed on a failed build" >&2
      return 1
    fi
  else
    if [ ! -e "$repo/build-manifest.json" ]; then
      echo "ERROR: $name did not write build-manifest.json" >&2
      cat "$dir/out.log" >&2
      return 1
    fi
    grep -q '==> done' "$dir/out.log" || { echo "ERROR: $name did not print done" >&2; return 1; }
  fi

  echo "ok: $name (rc=$rc, manifest=$want_manifest)"
}

# Failed controller build with a pre-existing target tag: no manifest/save/push/done.
run_case failed-controller controller controller nonzero absent --save --push

# `all`: controller fails first; manager/worker must never run (no masking).
run_case failed-all all controller nonzero absent
if grep -q 'manager-build' "$tmp_root/failed-all/make.log"; then
  echo "ERROR: failed-all ran manager after a controller failure" >&2
  exit 1
fi

# `all`: a later component failure also aborts; the worker after it never runs.
run_case failed-manager-all all manager nonzero absent
if ! grep -q 'controller-build' "$tmp_root/failed-manager-all/make.log"; then
  echo "ERROR: failed-manager-all did not run the controller before the manager" >&2
  exit 1
fi
if grep -q 'worker-build' "$tmp_root/failed-manager-all/make.log"; then
  echo "ERROR: failed-manager-all ran worker after a manager failure" >&2
  exit 1
fi

# Success path still writes a manifest and prints done.
run_case success-controller controller none zero present

echo "build-main failure regression OK."
