#!/usr/bin/env python3
"""Mechanical split of deployer.go by concern (Phase C10.8)."""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "internal" / "service"
src_path = ROOT / "deployer.go"
src = src_path.read_text(encoding="utf-8")

IMPORTS = """import (
\t"context"
\t"encoding/json"
\t"fmt"
\t"io/fs"
\t"net/url"
\t"os"
\t"path/filepath"
\t"sort"
\t"strings"

\tv1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
\t"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
\t"github.com/hiclaw/hiclaw-controller/internal/credprovider"
\t"github.com/hiclaw/hiclaw-controller/internal/executor"
\t"github.com/hiclaw/hiclaw-controller/internal/oss"
\t"sigs.k8s.io/controller-runtime/pkg/log"
)

"""

GROUPS = {
    "deployer_worker_config.go": {
        "func (d *Deployer) DeployWorkerConfig",
        "func heartbeatEvery",
        "func (d *Deployer) deployWorkerMcporterConfig",
        "func (d *Deployer) mergeExistingWorkerMcporterConfig",
        "func (d *Deployer) readExistingWorkerMcporterConfig",
        "type rawMcporterConfig",
        "type rawMcporterServer",
        "func mergeMcporterConfigPreservingExternal",
        "func mcporterGatewayOrigins",
        "func mcporterServerBelongsToGateway",
        "func parseMcporterServerURL",
        "func mcporterURLOrigin",
    },
    "deployer_coordination.go": {
        "func (d *Deployer) InjectCoordinationContext",
        "func (d *Deployer) renderAndPushSoulTemplate",
        "func (d *Deployer) InjectWorkerCoordination",
        "func (d *Deployer) InjectHeartbeatConfig",
        "func (d *Deployer) InjectChannelPolicy",
        "func (d *Deployer) SyncTeamLeaderAssets",
        "func hasDecoupledTeamContext",
    },
    "deployer_remote_skills.go": {
        "type nacosClientKey",
        "func (d *Deployer) pushRemoteSkills",
        "func mapRemoteSkillAuthType",
        "func remoteSkillSTSResources",
        "func parseNacosRemoteSource",
    },
    "deployer_manager.go": {
        "type ManagerDeployRequest",
        "func (d *Deployer) DeployManagerConfig",
        "func redactPackageURI",
    },
}

KEEP_PREFIXES = (
    "type WorkerDeployRequest",
    "type WorkerDepsPrepareRequest",
    "type RuntimeProjectionConfig",
    "type MemberRuntimeConfigDeployRequest",
    "type RuntimeConfigTeamMember",
    "type CoordinationDeployRequest",
    "type TeamWorkerEntry",
    "type WorkerCoordinationRequest",
    "type InjectHeartbeatRequest",
    "type InjectChannelPolicyRequest",
    "type SyncTeamLeaderAssetsRequest",
    "type DeployerConfig",
    "type Deployer struct",
    "func NewDeployer",
    "func (d *Deployer) DeployPackage",
    "func (d *Deployer) WriteInlineConfigs",
    "func (d *Deployer) PushOnDemandSkills",
    "func (d *Deployer) PrepareWorkerDeps",
    "func workerDepsObjectKey",
    "func workerDepsEnvFile",
    "func validEnvKey",
    "func shellSingleQuote",
    "func (d *Deployer) seedLocalAgentFiles",
    "func (d *Deployer) CleanLegacyPasswordFiles",
    "func (d *Deployer) CleanupOSSData",
    "func (d *Deployer) EnsureTeamStorage",
    "func (d *Deployer) ensureDirectoryObject",
    "func (d *Deployer) prepareAndPushAgentsMD",
    "func (d *Deployer) pushBuiltinSkills",
    "func (d *Deployer) pushBuiltinTopLevelFiles",
    "func (d *Deployer) builtinAgentDir",
)

pattern = re.compile(r"^(?:type |func )", re.M)
starts = [m.start() for m in pattern.finditer(src)]
starts.append(len(src))
decls = []
for i in range(len(starts) - 1):
    chunk = src[starts[i] : starts[i + 1]].rstrip() + "\n"
    first_line = chunk.split("\n", 1)[0]
    decls.append((first_line.strip(), chunk))

assigned: dict[str, list[str]] = {}
core: list[str] = []
for first, chunk in decls:
    matched = False
    for fname, prefixes in GROUPS.items():
        if any(first.startswith(p) for p in prefixes):
            assigned.setdefault(fname, []).append(chunk)
            matched = True
            break
    if not matched and any(first.startswith(p) for p in KEEP_PREFIXES):
        core.append(chunk)

for fname, chunks in assigned.items():
    (ROOT / fname).write_text("package service\n\n" + IMPORTS + "".join(chunks), encoding="utf-8")

header_end = src.index("type WorkerDeployRequest")
header = src[:header_end]
src_path.write_text(header + "".join(core), encoding="utf-8")
print("split into:", ", ".join(sorted(assigned.keys())))
