# agentteams-matrix — QwenPaw Adapter

This adapter projects the bundle contract (`../../contract.yaml`) onto the
QwenPaw runtime by installing the AgentTeams-owned Matrix channel plugin into
the QwenPaw working directory. The plugin files are copied verbatim from the
`agentteams-matrix-channel` compatibility path and keep the existing plugin
ID `agentteams-matrix-channel`, so runtimes that already recognize that ID
need no changes.

Payload (installed files):

```text
plugin.json            # plugin ID: agentteams-matrix-channel (unchanged)
plugin.py              # channel registration
agentteams_matrix/     # channel implementation
```

## Install

```bash
plugins/agentteams-matrix/scripts/install.sh
```

The installer copies the payload into:

```text
${QWENPAW_WORKING_DIR:-$HOME/.qwenpaw}/plugins/agentteams-matrix-channel/
```

and records per-file SHA-256 hashes in the marker:

```text
${QWENPAW_WORKING_DIR:-$HOME/.qwenpaw}/.agentteams/agentteams-matrix/installed.json
```

Re-running the install with unchanged content is a no-op: already-correct
files are not rewritten and the marker is left untouched. After a payload
change, only the files whose content changed are updated.

## Uninstall

```bash
plugins/agentteams-matrix/scripts/uninstall.sh
```

Removes exactly the files recorded in the marker, prunes the directories that
become empty, and deletes the bundle's installer state. Without a marker the
uninstall is a no-op, so re-running it leaves no residue.
