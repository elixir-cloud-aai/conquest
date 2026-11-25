"""The plugin authenticates to Conquest's runner callback API with the run's token."""

import miniwdl_conquest_tes as plugin


def test_runner_token_from_environment(monkeypatch):
    monkeypatch.setenv("CONQUEST_RUNNER_TOKEN", " abc123\n")
    assert plugin._runner_token() == "abc123"


def test_runner_token_unset_or_empty(monkeypatch):
    monkeypatch.delenv("CONQUEST_RUNNER_TOKEN", raising=False)
    assert plugin._runner_token() is None
    monkeypatch.setenv("CONQUEST_RUNNER_TOKEN", "")
    assert plugin._runner_token() is None


def test_tes_client_carries_the_token(monkeypatch):
    monkeypatch.setenv("CONQUEST_RUNNER_TOKEN", "abc123")
    backend = plugin.ConquestTESContainer.__new__(plugin.ConquestTESContainer)
    backend._tes_client = None
    monkeypatch.setattr(plugin.ConquestTESContainer, "_tes_url", "http://conquest/internal/runs/r1/ga4gh/tes/v1")
    assert backend.tes_client.token == "abc123"
