#!/usr/bin/env python3
"""Mechanical split of matrix/client.go by concern (Phase C10.8)."""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "internal" / "matrix"
src_path = ROOT / "client.go"
src = src_path.read_text(encoding="utf-8")

IMPORTS = """import (
\t"bytes"
\t"context"
\t"encoding/json"
\t"errors"
\t"fmt"
\t"io"
\t"net/http"
\t"net/url"
\t"strings"
\t"sync/atomic"
\t"time"

\thiclawmetrics "github.com/hiclaw/hiclaw-controller/internal/metrics"
\t"sigs.k8s.io/controller-runtime/pkg/log"
)

"""

GROUPS = {
    "client_users.go": {
        "func (c *TuwunelClient) ensureAdminToken",
        "func (c *TuwunelClient) EnsureUser",
        "func (c *TuwunelClient) Login",
        "func (c *TuwunelClient) EnsureAppServiceUser",
        "func (c *TuwunelClient) LoginAppServiceUser",
        "func (c *TuwunelClient) SetPasswordAsAdmin",
        "func (c *TuwunelClient) doJSONWithASToken",
        "func (c *TuwunelClient) VerifyAccessToken",
        "func (c *TuwunelClient) SetDisplayName",
        "func (c *TuwunelClient) accessTokenUserID",
    },
    "client_rooms.go": {
        "func (c *TuwunelClient) CreateRoom",
        "func (c *TuwunelClient) logCreateRoomFailureDiagnostics",
        "func (c *TuwunelClient) ResolveRoomAlias",
        "func (c *TuwunelClient) DeleteRoomAlias",
        "func (c *TuwunelClient) SetRoomName",
        "func (c *TuwunelClient) SetRoomState",
        "func (c *TuwunelClient) JoinRoom",
        "func (c *TuwunelClient) LeaveRoom",
        "func (c *TuwunelClient) ListRoomMembers",
        "func (c *TuwunelClient) ListRoomMembersWithToken",
        "func (c *TuwunelClient) listRoomMembers",
        "func (c *TuwunelClient) InviteToRoom",
        "func (c *TuwunelClient) InviteToRoomWithToken",
        "func (c *TuwunelClient) inviteToRoom",
        "func (c *TuwunelClient) KickFromRoom",
        "func (c *TuwunelClient) KickFromRoomWithToken",
        "func (c *TuwunelClient) kickFromRoom",
        "func (c *TuwunelClient) ListJoinedRooms",
    },
    "client_messages.go": {
        "func (c *TuwunelClient) SendMessage",
        "func (c *TuwunelClient) sendMessage",
        "func (c *TuwunelClient) ensureAdminRoomID",
        "func (c *TuwunelClient) SendMessageAsAdmin",
        "func (c *TuwunelClient) AdminCommand",
        "func (c *TuwunelClient) SyncMessages",
    },
    "client_http.go": {
        "func (c *TuwunelClient) doJSON",
        "func (c *TuwunelClient) doJSONAsAdmin",
        "func matrixOperation",
        "func encodeRoomID",
        "func roomAliasFullFor",
        "func encodeAlias",
        "func truncate",
    },
}

KEEP_PREFIXES = (
    "var ErrAppServiceNotReady",
    "type Client interface",
    "type MessageEvent",
    "type SyncMessagesResult",
    "type TuwunelClient struct",
    "func NewTuwunelClient",
    "func (c *TuwunelClient) UserID",
)

pattern = re.compile(r"^(?:type |func |var )", re.M)
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
    (ROOT / fname).write_text("package matrix\n\n" + IMPORTS + "".join(chunks), encoding="utf-8")

header_end = src.index("var ErrAppServiceNotReady")
header = src[:header_end]
src_path.write_text(header + "".join(core), encoding="utf-8")
print("split into:", ", ".join(sorted(assigned.keys())))
