"""The plugin authenticates to Conquest's runner callback API with the run's token."""

import pytest
import requests

import snakemake_executor_plugin_conquest_tes as plugin


@pytest.mark.parametrize("value, expected", [("abc123", "abc123"), (" abc123\n", "abc123"), ("", None)])
def test_runner_token_from_environment(monkeypatch, value, expected):
    monkeypatch.setenv("CONQUEST_RUNNER_TOKEN", value)
    assert plugin.runner_token() == expected


def test_runner_token_unset(monkeypatch):
    monkeypatch.delenv("CONQUEST_RUNNER_TOKEN", raising=False)
    assert plugin.runner_token() is None


def test_tes_client_sends_the_token(monkeypatch):
    """py-tes sends the token as a bearer header on every call."""
    import tes

    sent = []

    class Response:
        def __init__(self, body):
            self.body = body

        def raise_for_status(self):
            pass

        def json(self):
            return self.body

    def record(url, **kwargs):
        sent.append(kwargs["headers"].get("Authorization"))
        if url.endswith("/v1/tasks"):
            return Response({"id": "t1"})
        return Response({"id": "t1", "state": "RUNNING"})

    monkeypatch.setattr(requests, "get", record)
    monkeypatch.setattr(requests, "post", record)
    client = tes.HTTPClient(url="http://conquest/internal/runs/r1/ga4gh/tes", token="abc123")
    client.get_task("t1")
    client.cancel_task("t1")
    client.create_task(tes.Task(executors=[tes.Executor(image="alpine", command=["true"])]))
    assert sent == ["Bearer abc123"] * 3
