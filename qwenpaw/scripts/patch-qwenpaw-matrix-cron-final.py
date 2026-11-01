"""QwenPaw 2.2.x compatibility: complete final-mode Matrix cron delivery.

For every version in SUPPORTED_VERSIONS the three anchor lines in _ANCHORS
have been checked against that version's actual
``qwenpaw/app/crons/executor.py`` (file contents, read-only): 2.2.1 and
2.2.2b4 carry the anchors from the original patch; 2.2.2.post1 was
re-verified against the installed 2.2.2.post1 executor (the pin in
hack/versions.env, QWENPAW_PIP_SPEC) on 2026-11-01 - each anchor occurs
exactly once and the marker is absent.  When widening SUPPORTED_VERSIONS,
re-verify the anchors against the new version's actual executor first;
never relax the version gate without that check.
"""
from importlib.metadata import distribution

SUPPORTED_VERSIONS = ("2.2.1", "2.2.2b4", "2.2.2.post1")

MARKER = "# AgentTeams Matrix needs the closing response to flush its final reply."

# Each anchor must occur exactly once in the unpatched executor.
_ANCHORS = (
    "            final_event: Any | None = None\n",
    '                if job.dispatch.mode == "final":\n',
    "                await _deliver(final_event)\n",
)

# (anchor, text inserted directly after the anchor line)
_REPLACEMENTS = (
    (
        "            final_event: Any | None = None\n",
        "            final_response: Any | None = None\n",
    ),
    (
        '                if job.dispatch.mode == "final":\n',
        '                    if getattr(event, "object", None) == "response":\n'
        "                        final_response = event\n",
    ),
    (
        "                await _deliver(final_event)\n",
        "                " + MARKER + "\n"
        '                if target_channel == "agentteams_matrix" and final_response is not None:\n'
        "                    await _deliver(final_response)\n",
    ),
)


def apply_patch(source: str) -> str:
    """Return the executor source with the completion patch applied.

    Pure function: no I/O, no package lookups.  Idempotent - a source that
    already contains MARKER is returned unchanged.  Raises RuntimeError if
    any anchor in _ANCHORS does not occur exactly once.
    """
    if MARKER in source:
        return source
    for anchor in _ANCHORS:
        if source.count(anchor) != 1:
            raise RuntimeError("Unexpected QwenPaw cron executor; review completion patch")
    for anchor, addition in _REPLACEMENTS:
        source = source.replace(anchor, anchor + addition)
    return source


def main():
    package = distribution("qwenpaw")
    if package.version not in SUPPORTED_VERSIONS:
        raise RuntimeError("Review the Matrix cron completion patch for QwenPaw " + package.version)
    path = package.locate_file("qwenpaw/app/crons/executor.py")
    source = path.read_text(encoding="utf-8")
    patched = apply_patch(source)
    if patched != source:
        path.write_text(patched, encoding="utf-8")


if __name__ == "__main__":
    main()
