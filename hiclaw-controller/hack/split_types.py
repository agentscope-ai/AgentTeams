#!/usr/bin/env python3
"""Mechanical split of api/v1beta1/types.go by kind (Phase C10.17)."""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "api" / "v1beta1"
src_path = ROOT / "types.go"
src = src_path.read_text(encoding="utf-8")

HEADER = """// +k8s:deepcopy-gen=package

package v1beta1

import (
\tapiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
\tmetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

"""

GROUPS = {
    "types_shared.go": {
        "const (",
        "const LabelController",
        "const LabelWorker",
        "const LabelWorkerSvcName",
        "const LabelWorkerEdgeUUID",
        "const AnnotationEdgeAppliedUUID",
        "type AccessEntry struct",
        "type AgentIdentitySpec struct",
        "type CredentialRef struct",
        "type CredentialBinding struct",
        "type MCPServer struct",
        "type RemoteSkill struct",
        "type RemoteSkillSource struct",
        "type AgentResourceRequirements struct",
        "type AgentResourceValues struct",
        "type WorkerResourceSpec struct",
        "type ExposePort struct",
        "type ChannelPolicySpec struct",
        "type ChannelsSpec struct",
        "type DingTalkChannelSpec struct",
        "type ExposedPortStatus struct",
    },
    "worker_types.go": {
        "type Worker struct",
        "type WorkerSpec struct",
        "func (s WorkerSpec) GetBackendRuntime",
        "func (s WorkerSpec) DesiredContainerMan",
        "func (s WorkerSpec) DesiredState",
        "func (s WorkerSpec) EffectiveWorkerName",
        "type WorkerVolumeSpec struct",
        "type WorkerMountSpec struct",
        "type WorkerOSSVolumeSpec struct",
        "type WorkerOSSAuthSpec struct",
        "type WorkerOSSRRSASpec struct",
        "type WorkerAccessKeyAuthSpec struct",
        "type NamespacedSecretRef struct",
        "type WorkerStatus struct",
        "type WorkerList struct",
    },
    "team_types.go": {
        "type Team struct",
        "type TeamSpec struct",
        "type TeamWorkerRef struct",
        "func (s TeamSpec) EffectiveTeamName",
        "type TeamAdminSpec struct",
        "type TeamMemberSpec struct",
        "type LeaderSpec struct",
        "type TeamLeaderHeartbeatSpec struct",
        "type TeamWorkerSpec struct",
        "func (s LeaderSpec) EffectiveWorkerName",
        "func (s TeamWorkerSpec) EffectiveWorkerName",
        "type TeamStatus struct",
        "func (s *TeamStatus) MemberByName",
        "type TeamMemberStatus struct",
        "type TeamList struct",
    },
    "human_types.go": {
        "type Human struct",
        "type HumanSpec struct",
        "type IdentitySourceSpec struct",
        "type HumanStatus struct",
        "func (s HumanSpec) EffectiveUsername",
        "type HumanList struct",
    },
    "manager_types.go": {
        "type Manager struct",
        "type ManagerSpec struct",
        "func (s ManagerSpec) DesiredState",
        "type ManagerConfig struct",
        "type ManagerStatus struct",
        "type ManagerList struct",
    },
    "project_types.go": {
        "type Project struct",
        "type ProjectSpec struct",
        "type ProjectRepo struct",
        "func (s ProjectSpec) EffectiveProjectName",
        "type ProjectStatus struct",
        "type ProjectCondition struct",
        "func (s *ProjectStatus) ConditionByType",
        "func (s *ProjectStatus) SetCondition",
        "type ProjectList struct",
    },
}

pattern = re.compile(r"^(?:const |type |func )", re.M)
starts = [m.start() for m in pattern.finditer(src)]
starts.append(len(src))
decls = []
for i in range(len(starts) - 1):
    chunk = src[starts[i] : starts[i + 1]].rstrip() + "\n"
    first_line = chunk.split("\n", 1)[0]
    decls.append((first_line.strip(), chunk))

assigned: dict[str, list[str]] = {}
for first, chunk in decls:
    matched = False
    for fname, prefixes in GROUPS.items():
        if any(first.startswith(p) for p in prefixes):
            assigned.setdefault(fname, []).append(chunk)
            matched = True
            break
    if not matched:
        raise SystemExit(f"unassigned decl: {first!r}")

for fname, chunks in assigned.items():
    body = HEADER if fname == "types_shared.go" else "package v1beta1\n\n"
    (ROOT / fname).write_text(body + "".join(chunks), encoding="utf-8")

src_path.write_text("// +k8s:deepcopy-gen=package\n\npackage v1beta1\n\n// CRD types are split by kind across types_shared.go, worker_types.go,\n// team_types.go, human_types.go, manager_types.go, and project_types.go\n// (Phase C10.17). register.go and zz_generated.deepcopy.go are unchanged.\n", encoding="utf-8")
print("split into:", ", ".join(sorted(assigned.keys())))
