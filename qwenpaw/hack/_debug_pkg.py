from pathlib import Path
import tempfile
import tarfile
from qwenpaw_worker.update import AgentPackageManager, MemberRuntimeConfig

tmp = Path(tempfile.mkdtemp())
source = tmp / "src"
config = source / "config"
config.mkdir(parents=True)
(source / "manifest.json").write_text('{"version":"1.0"}\n')
(config / "AGENTS.md").write_text("agent package 1\n")
pkg = tmp / "pkg.tar.gz"
with tarfile.open(pkg, "w:gz") as archive:
    archive.add(source, arcname=".")

mgr = AgentPackageManager(tmp / "packages", workspace_dir=tmp / "ws")
rc = MemberRuntimeConfig(
    path=tmp / "r.yaml",
    raw={
        "desired": {
            "agentPackage": {
                "ref": str(pkg),
                "name": "p",
                "version": "1",
                "digest": "",
            }
        }
    },
)
try:
    result = mgr.apply(rc)
    print("result", result)
    print("current listing", list(mgr.current_dir.rglob("*")) if mgr.current_dir.exists() else "no current")
except Exception as exc:
    print("ERROR", type(exc).__name__, exc)
