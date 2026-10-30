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

## Runtime requirement: Matrix E2EE

The channel's E2EE preflight detects the crypto backend of the host
Python environment: `matrix-nio >= 0.26` provides E2EE through
`vodozemac` (the `e2e` extra; there is no top-level `olm` module
anymore), older releases through `olm`. Install the extra in the
environment that runs the channel:

```bash
pip install "matrix-nio[e2e]"
```

(On `matrix-nio < 0.26` the extra additionally needs the system
`libolm`, e.g. `apt/dnf install libolm-dev`.) Without any importable
backend the channel degrades by design: it logs

```text
MatrixChannel: no Matrix E2EE backend (vodozemac/olm) — falling back to
non-encrypted mode. Install matrix-nio[e2e]; on matrix-nio < 0.26 also
install libolm (apt/dnf install libolm-dev).
```

and continues unencrypted. `install.sh` deliberately does not pip
install this: the host/image Python environment is owned by the
packaging line, and the installer must not touch the host venv.

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
