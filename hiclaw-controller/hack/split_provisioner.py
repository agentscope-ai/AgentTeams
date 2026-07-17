#!/usr/bin/env python3
"""Mechanical split of provisioner.go by concern (Phase C10.8)."""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "internal" / "service"
src_path = ROOT / "provisioner.go"
src = src_path.read_text(encoding="utf-8")

IMPORTS = """import (
\t"context"
\t"fmt"
\t"net/http"
\t"strings"
\t"time"

\tv1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
\tauthpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
\t"github.com/hiclaw/hiclaw-controller/internal/backend"
\t"github.com/hiclaw/hiclaw-controller/internal/gateway"
\t"github.com/hiclaw/hiclaw-controller/internal/matrix"
\t"github.com/hiclaw/hiclaw-controller/internal/oss"
\t"github.com/hiclaw/hiclaw-controller/internal/slicesx"
\t"k8s.io/client-go/kubernetes"
\t"sigs.k8s.io/controller-runtime/pkg/log"
)

"""

GROUPS = {
    "provisioner_credentials.go": {
        "func (p *Provisioner) ensureMatrixToken",
        "func (p *Provisioner) ForceRefreshMatrixToken",
        "func (p *Provisioner) RefreshCredentials",
        "func (p *Provisioner) RefreshWorkerCredentials",
        "func (p *Provisioner) loadWorkerCredentials",
        "func (p *Provisioner) RefreshManagerCredentials",
        "func (p *Provisioner) EnsureManagerGatewayAuth",
        "func (p *Provisioner) EnsureWorkerGatewayAuth",
        "func (p *Provisioner) DeleteCredentials",
        "func (p *Provisioner) DeleteWorkerCredentials",
        "func (p *Provisioner) CredentialNames",
        "func (p *Provisioner) BackfillLegacyPasswords",
    },
    "provisioner_team_rooms.go": {
        "type TeamRoomRequest",
        "type TeamRoomResult",
        "type TeamRoomArchiveRequest",
        "func (p *Provisioner) ProvisionTeamRooms",
        "func (p *Provisioner) ensureTeamAdminJoinedLeaderDM",
        "func (p *Provisioner) leaderDMPowerLevels",
        "func (p *Provisioner) resolveTeamAdminMatrixID",
        "func (p *Provisioner) resolveTeamCoordinatorMatrixIDs",
        "func (p *Provisioner) resolveTeamMemberMatrixIDs",
        "func withoutString",
        "func (p *Provisioner) EnsureRoomMember",
        "func (p *Provisioner) EnsureRoomNonMember",
        "func (p *Provisioner) leaderInviteToken",
        "func (p *Provisioner) observedRoomMembership",
        "func (p *Provisioner) observedRoomMembershipWithToken",
        "func observedMembershipFromMembers",
        "func observedMembershipsFromMembers",
        "func shouldForceLeaveAfterKickError",
        "func (p *Provisioner) DeleteTeamRoomAliases",
        "func (p *Provisioner) ArchiveTeamRooms",
    },
    "provisioner_manager.go": {
        "type ManagerProvisionRequest",
        "type ManagerProvisionResult",
        "func (p *Provisioner) ProvisionManager",
        "type ManagerWelcomeRequest",
        "func (p *Provisioner) IsManagerLLMAuthReady",
        "func (p *Provisioner) IsManagerJoinedDM",
        "func (p *Provisioner) SendManagerWelcomeMessage",
        "func renderManagerWelcomeBody(",
        "func renderManagerWelcomeBodySolo",
        "func (p *Provisioner) DeprovisionManager",
    },
}

KEEP_PREFIXES = (
    "type WorkerProvisionRequest",
    "type WorkerProvisionResult",
    "type WorkerDeprovisionRequest",
    "type RefreshResult",
    "type ProvisionerConfig",
    "type Provisioner struct",
    "func NewProvisioner",
    "func (p *Provisioner) MatrixUserID",
    "func (p *Provisioner) SendAdminMessage",
    "func (p *Provisioner) MatrixAppServiceEnabled",
    "func roomAliasLocalpart",
    "func (p *Provisioner) roomAliasFull",
    "func (p *Provisioner) leaveAllRooms",
    "func (p *Provisioner) deleteRoom",
    "func (p *Provisioner) LeaveAllWorkerRooms",
    "func (p *Provisioner) DeleteWorkerRoom",
    "func (p *Provisioner) LeaveAllManagerRooms",
    "func (p *Provisioner) DeleteManagerRoom",
    "func (p *Provisioner) ProvisionWorker",
    "func (p *Provisioner) DeprovisionWorker",
    "func (p *Provisioner) DeleteWorkerRoomAlias",
    "func (p *Provisioner) DeleteManagerRoomAlias",
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

header_end = src.index("type WorkerProvisionRequest")
header = src[:header_end]
src_path.write_text(header + "".join(core), encoding="utf-8")
print("split into:", ", ".join(sorted(assigned.keys())))
