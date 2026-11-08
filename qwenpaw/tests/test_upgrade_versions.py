"""Offline unit tests for ``qwenpaw_worker.upgrade.versions`` (no network).

Covers the candidate-listing rules (PEP440 ordering, pre-release
exclusion, unparseable-name tolerance), the release timestamp helper, and
the ``fetch_releases`` error translation (``_http_json`` is monkeypatched).
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[1]  # .../AgentTeams-runtime-upgrade/qwenpaw
SRC = REPO_ROOT / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))

from qwenpaw_worker.upgrade import versions  # noqa: E402

RELEASES = {
    "2.2.1": [{"upload_time": "2026-09-01T00:00:00"}],
    "2.2.2": [
        {"upload_time": "2026-09-15T10:00:00"},
        {"upload_time": "2026-09-15T09:00:00"},
    ],
    "2.2.2b4": [{"upload_time": "2026-09-10T00:00:00"}],
    "2.3.0": [],
    "2.3.1.post1": [{"upload_time": "2026-10-01T00:00:00"}],
}


def test_list_candidates_ascending_excludes_prerelease():
    assert versions.list_candidate_versions("2.2.1", RELEASES) == [
        "2.2.2",
        "2.3.0",
        "2.3.1.post1",
    ]


def test_list_candidates_include_prerelease_sorted_before_release():
    assert versions.list_candidate_versions("2.2.1", RELEASES, include_prerelease=True) == [
        "2.2.2b4",
        "2.2.2",
        "2.3.0",
        "2.3.1.post1",
    ]


def test_list_candidates_unparseable_current_yields_none():
    assert versions.list_candidate_versions("unknown", RELEASES) == []


def test_list_candidates_drops_unparseable_release_names():
    releases = dict(RELEASES)
    releases["v2.4.0"] = []  # invalid: must start with a digit
    releases["2.5.0"] = []
    assert versions.list_candidate_versions("2.2.1", releases) == [
        "2.2.2",
        "2.3.0",
        "2.3.1.post1",
        "2.5.0",
    ]


def test_release_published_at_is_earliest_upload():
    assert versions.release_published_at(RELEASES["2.2.2"]) == "2026-09-15T09:00:00"
    assert versions.release_published_at([]) is None
    assert versions.release_published_at(None) is None


def test_fetch_releases_ok(monkeypatch):
    monkeypatch.setattr(
        versions, "_http_json", lambda url, timeout=20.0: {"releases": RELEASES}
    )
    assert versions.fetch_releases("https://example.invalid/qwenpaw/json") is RELEASES


def test_fetch_releases_network_error_raises(monkeypatch):
    def boom(url, timeout=20.0):
        raise OSError("net down")

    monkeypatch.setattr(versions, "_http_json", boom)
    with pytest.raises(versions.UpgradeIndexError, match="net down"):
        versions.fetch_releases()


def test_fetch_releases_bad_payload_raises(monkeypatch):
    monkeypatch.setattr(versions, "_http_json", lambda url, timeout=20.0: {"info": {}})
    with pytest.raises(versions.UpgradeIndexError, match="releases"):
        versions.fetch_releases()


def test_is_valid_version_and_is_prerelease():
    assert versions.is_valid_version("2.3.0")
    assert not versions.is_valid_version("v2.3.0")
    assert not versions.is_valid_version("")
    assert versions.is_prerelease("2.2.2b4")
    assert not versions.is_prerelease("2.3.1.post1")
