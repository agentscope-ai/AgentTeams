"""QwenPaw 2.2.x compatibility: complete final-mode Matrix cron delivery."""
from importlib.metadata import distribution


def main():
    package = distribution("qwenpaw")
    if package.version not in ("2.2.1", "2.2.2b4"):
        raise RuntimeError("Review the Matrix cron completion patch for QwenPaw " + package.version)
    path = package.locate_file("qwenpaw/app/crons/executor.py")
    source = path.read_text(encoding="utf-8")
    marker = "# AgentTeams Matrix needs the closing response to flush its final reply."
    if marker in source:
        return
    replacements = (
        (
            "            final_event: Any | None = None\n",
            "            final_event: Any | None = None\n"
            "            final_response: Any | None = None\n",
        ),
        (
            '                if job.dispatch.mode == "final":\n',
            '                if job.dispatch.mode == "final":\n'
            '                    if getattr(event, "object", None) == "response":\n'
            '                        final_response = event\n',
        ),
        (
            "                await _deliver(final_event)\n",
            "                await _deliver(final_event)\n"
            "                " + marker + "\n"
            '                if target_channel == "agentteams_matrix" and final_response is not None:\n'
            "                    await _deliver(final_response)\n",
        ),
    )
    for old, new in replacements:
        if source.count(old) != 1:
            raise RuntimeError("Unexpected QwenPaw cron executor; review completion patch")
        source = source.replace(old, new)
    path.write_text(source, encoding="utf-8")


if __name__ == "__main__":
    main()
