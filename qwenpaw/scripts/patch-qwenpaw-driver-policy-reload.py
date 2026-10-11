"""QwenPaw 2.2.1 compatibility fix: do not publish a stale reload policy."""
from importlib.metadata import distribution


def main():
    package = distribution("qwenpaw")
    if package.version != "2.2.1":
        raise RuntimeError("Review the Driver reload compatibility patch for QwenPaw " + package.version)
    path = package.locate_file("qwenpaw/drivers/manager.py")
    source = path.read_text(encoding="utf-8")
    start = source.index("    async def reload_driver(")
    end = source.index("    async def refresh_driver(", start)
    method = source[start:end]
    old = "                await self._card_store.save(card)\n"
    new = (
        "                # Policy may change while the replacement handler initializes.\n"
        "                # Read it under the same lock as sync_driver_policy; never\n"
        "                # write the stale card back over newer persisted configuration.\n"
        "                latest = await self._card_store.load_path(path)\n"
        "                card.policy = latest.policy\n"
        "                if handler is not None:\n"
        "                    handler.set_policy(card.policy)\n"
    )
    if new in method:
        return
    if method.count(old) != 1:
        raise RuntimeError("Unexpected QwenPaw reload_driver source; review compatibility patch")
    path.write_text(source[:start] + method.replace(old, new) + source[end:], encoding="utf-8")


if __name__ == "__main__":
    main()
