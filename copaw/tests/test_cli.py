import inspect

import typer

from copaw_worker import cli


def test_cli_defaults_sync_interval_to_one_minute(monkeypatch):
    captured = {}
    monkeypatch.setattr(typer, "run", lambda callback: captured.setdefault("callback", callback))

    cli.main()

    option = inspect.signature(captured["callback"]).parameters["sync_interval"].default
    assert option.default == 60
