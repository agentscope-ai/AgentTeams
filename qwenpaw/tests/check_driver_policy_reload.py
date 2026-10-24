"""Exercise the installed QwenPaw DriverManager across a paused reload."""
import asyncio
import copy
import types
import unittest

from qwenpaw.drivers.manager import DriverManager


class Store:
    def __init__(self, card):
        self.card = card

    async def stored_path(self, name):
        return name

    async def load_path(self, path):
        return copy.deepcopy(self.card)

    async def save(self, card):
        self.card = copy.deepcopy(card)


class Handler:
    def __init__(self, card):
        self.card = card

    def set_policy(self, policy):
        self.card.policy = policy


class ReloadPolicyTest(unittest.IsolatedAsyncioTestCase):
    async def check_update(self, before, after):
        card = types.SimpleNamespace(name="test", protocol="mcp", enabled=True, policy=before)
        manager = DriverManager.__new__(DriverManager)
        manager._lock = asyncio.Lock()
        manager._handler_scopes = {}
        manager._handlers = {"test": Handler(copy.deepcopy(card))}
        manager._card_store = Store(card)
        manager._validate_card_for_registered_protocol = lambda value: value
        manager._runtime_info_from_card = lambda value: value
        loaded, resume = asyncio.Event(), asyncio.Event()

        async def build(value):
            loaded.set()
            await resume.wait()
            return Handler(value)

        async def shutdown(handler):
            pass

        manager._build_and_init_handler = build
        manager._shutdown_handler = shutdown
        reload_task = asyncio.create_task(manager.reload_driver("test"))
        await asyncio.wait_for(loaded.wait(), 5)
        updated = copy.deepcopy(card)
        updated.policy = after
        await manager.sync_driver_policy(updated)
        self.assertEqual(manager._card_store.card.policy, after)
        resume.set()
        await asyncio.wait_for(reload_task, 5)
        self.assertEqual(manager._card_store.card.policy, after)
        self.assertEqual(manager._handlers["test"].card.policy, after)

    async def test_allow_update_survives_reload(self):
        await self.check_update("ask", "allow")

    async def test_approval_requirement_survives_reload(self):
        await self.check_update("allow", "ask")

    async def test_revocation_survives_reload(self):
        await self.check_update("allow", "deny")


if __name__ == "__main__":
    unittest.main()
