#!/usr/bin/env python3
"""Mechanical split of team_controller.go by concern (Phase C10.1)."""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "internal" / "controller"
src_path = ROOT / "team_controller.go"
src = src_path.read_text(encoding="utf-8")

GROUPS = {
    "team_rooms.go": {
        "type teamAdminActor",
        "func (r *TeamReconciler) resolveTeamAdminActor",
        "func (r *TeamReconciler) deriveTeamWithResolvedIdentities",
        "func (r *TeamReconciler) appendAccessibleTeamHumans",
        "func (r *TeamReconciler) syncTeamRoomHumanStatuses",
        "func (r *TeamReconciler) resolveHumanMatrixUserID",
        "func (r *TeamReconciler) resolveHumanMemberMatrixUserID",
        "func forceSoloPeerMentions",
        "func (r *TeamReconciler) archiveTeamRooms",
        "func (r *TeamReconciler) decoupledLeaderRuntimeName",
    },
    "team_members_legacy.go": {
        "func (r *TeamReconciler) reconcileTeamLegacy",
        "func (r *TeamReconciler) legacyTeamMembers",
        "func (r *TeamReconciler) legacyMemberContext",
        "func legacyLeaderWorkerSpec",
        "func legacyTeamWorkerSpec",
        "func legacyTeamWorkerNames",
        "func (r *TeamReconciler) injectLegacyTeamContext",
    },
    "team_members_shared.go": {
        "func (r *TeamReconciler) reconcileMember",
        "func (r *TeamReconciler) summarizeBackendReadiness",
        "func (r *TeamReconciler) writeInlineConfigs",
    },
    "team_delete.go": {
        "func (r *TeamReconciler) handleDelete",
        "func (r *TeamReconciler) handleDeleteLegacy",
        "func (r *TeamReconciler) cleanupStaleLegacyMembers",
        "func (r *TeamReconciler) legacyDeleteMembers",
        "func (r *TeamReconciler) legacyDeleteMemberFromStatus",
        "func (r *TeamReconciler) handleDeleteDecoupled",
        "func (r *TeamReconciler) removeLegacyMember",
    },
    "team_members_decoupled.go": {
        "type decoupledTeamMember",
        "func (r *TeamReconciler) decoupledMemberRuntime",
        "func (r *TeamReconciler) reconcileTeamDecoupled",
        "func (r *TeamReconciler) resolveDecoupledMembers",
        "func decoupledLeaderMember",
        "func decoupledWorkerRuntimeNames",
        "func decoupledTeamWorkerEntries",
        "func decoupledMemberStatusSnapshot",
        "func syncDecoupledMemberStatus",
        "func (r *TeamReconciler) cleanupStaleDecoupledMembers",
        "func (r *TeamReconciler) detachDecoupledMember",
        "func decoupledMemberContext",
        "func aggregateDecoupledTeamStatus",
        "func validateWorkerMembers",
    },
    "team_status.go": {
        "func (r *TeamReconciler) reconcileLegacyMember",
        "func (r *TeamReconciler) failTeam",
        "func memberStatus",
        "func pruneMembers",
        "func removeString",
        "func sortMembers",
        "func observedMemberNames",
        "func (r *TeamReconciler) runtimeConfigTeamMembers",
        "func teamAdminMatrixID",
        "func teamAdminName",
        "func teamCoordinatorIDs",
        "func (r *TeamReconciler) workerToTeamRequests",
        "func workerStatusChangePredicate",
        "func teamAdminRegistryEntry",
        "func teamMemberRegistryEntries",
    },
}

KEEP_PREFIXES = (
    "const (",
    "type TeamReconciler",
    "func (r *TeamReconciler) Reconcile",
    "func (r *TeamReconciler) reconcileTeamNormal",
    "func (r *TeamReconciler) SetupWithManager",
    "func TeamPodMapFunc",
)

IMPORTS = """import (
\t"context"
\t"fmt"
\t"sort"
\t"strings"
\t"time"

\tv1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
\t"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
\t"github.com/hiclaw/hiclaw-controller/internal/auth"
\t"github.com/hiclaw/hiclaw-controller/internal/backend"
\t"github.com/hiclaw/hiclaw-controller/internal/controller/humanidentity"
\t"github.com/hiclaw/hiclaw-controller/internal/gateway"
\t"github.com/hiclaw/hiclaw-controller/internal/matrix"
\t"github.com/hiclaw/hiclaw-controller/internal/metrics"
\t"github.com/hiclaw/hiclaw-controller/internal/service"
\t"github.com/hiclaw/hiclaw-controller/internal/slicesx"
\tcorev1 "k8s.io/api/core/v1"
\t"k8s.io/client-go/dynamic"
\tctrl "sigs.k8s.io/controller-runtime"
\t"sigs.k8s.io/controller-runtime/pkg/builder"
\t"sigs.k8s.io/controller-runtime/pkg/client"
\t"sigs.k8s.io/controller-runtime/pkg/controller"
\t"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
\t"sigs.k8s.io/controller-runtime/pkg/event"
\t"sigs.k8s.io/controller-runtime/pkg/handler"
\t"sigs.k8s.io/controller-runtime/pkg/log"
\t"sigs.k8s.io/controller-runtime/pkg/predicate"
\t"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

"""

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
    (ROOT / fname).write_text("package controller\n\n" + IMPORTS + "".join(chunks), encoding="utf-8")

core_header = src[: src.index("const (")]
src_path.write_text(core_header + "".join(core), encoding="utf-8")
print("split into:", ", ".join(sorted(assigned.keys())))
