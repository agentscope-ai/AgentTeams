#!/usr/bin/env python3
"""Mechanical split of config.go by concern (Phase C10.16)."""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "internal" / "config"
src_path = ROOT / "config.go"
src = src_path.read_text(encoding="utf-8")

IMPORTS = """import (
\t"encoding/json"
\t"fmt"
\t"net/url"
\t"os"
\t"path/filepath"
\t"strconv"
\t"strings"

\tv1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
\t"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
\t"github.com/hiclaw/hiclaw-controller/internal/backend"
\t"github.com/hiclaw/hiclaw-controller/internal/credentials"
\t"github.com/hiclaw/hiclaw-controller/internal/gateway"
\t"github.com/hiclaw/hiclaw-controller/internal/matrix"
\t"github.com/hiclaw/hiclaw-controller/internal/oss"
)

"""

GROUPS = {
    "config_types.go": {
        "type Config struct",
        "type WorkerEnvDefaults struct",
        "type managerSpecEnv struct",
    },
    "config_load.go": {
        "func LoadConfig",
        "func envOrDefault",
        "func envOrDefaultInt",
        "func envBool",
        "func envBoolDefault",
        "func firstNonEmpty",
        "func applyManagerSpec",
        "func agentResourcesEmpty",
        "func extractHost",
        "func replaceHost",
        "func normalizeMinIOS3Endpoint",
    },
    "config_derived.go": {
        "func (c *Config) Namespace",
        "func (c *Config) HasMinIOAdmin",
        "func (c *Config) CredsDir",
        "func (c *Config) AgentFSDir",
        "func (c *Config) ManagerStateFile",
        "func (c *Config) WorkerAgentDir",
        "func (c *Config) ManagerConfigPath",
        "func (c *Config) RegistryPath",
        "func (c *Config) ManagerResources",
        "func (c *Config) DockerConfig",
        "func (c *Config) STSConfig",
        "func (c *Config) AIGatewayConfig",
        "func (c *Config) UsesAIGateway",
        "func (c *Config) UsesExternalOSS",
        "func (c *Config) K8sConfig",
        "func (c *Config) SandboxConfig",
        "func (c *Config) MatrixConfig",
        "func appServicePushURL",
        "func (c *Config) GatewayConfig",
        "func (c *Config) OSSConfig",
        "func (c *Config) ManagerAgentEnv",
        "func (c *Config) AgentConfig",
    },
}

pattern = re.compile(r"^(?:type |func )", re.M)
starts = [m.start() for m in pattern.finditer(src)]
starts.append(len(src))
decls = []
for i in range(len(starts) - 1):
    chunk = src[starts[i] : starts[i + 1]].rstrip() + "\n"
    first_line = chunk.split("\n", 1)[0]
    decls.append((first_line.strip(), chunk))

assigned: dict[str, list[str]] = {}
for first, chunk in decls:
    for fname, prefixes in GROUPS.items():
        if any(first.startswith(p) for p in prefixes):
            assigned.setdefault(fname, []).append(chunk)
            break

for fname, chunks in assigned.items():
    (ROOT / fname).write_text("package config\n\n" + IMPORTS + "".join(chunks), encoding="utf-8")

src_path.write_text("package config\n\n// Config loading and derived views are split across config_types.go,\n// config_load.go, and config_derived.go (Phase C10.16).\n", encoding="utf-8")
print("split into:", ", ".join(sorted(assigned.keys())))
