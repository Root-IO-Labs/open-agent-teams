"""Unit tests for the Langfuse telemetry middleware + client.

These tests stand up an in-process HTTP stub and confirm the client sends
the right wire payload. The middleware itself is exercised by running its
``wrap_model_call`` against a fake handler and checking that exceptions
in instrumentation never propagate up.
"""
from __future__ import annotations

import base64
import http.server
import importlib
import json
import os
import threading
import time
from collections.abc import Iterator
from typing import Any
from unittest import mock

import pytest

from oat_sdk.middleware import _langfuse_client
from oat_sdk.middleware.telemetry import LangfuseMiddleware


@pytest.fixture(autouse=True)
def _reset_singleton(monkeypatch: pytest.MonkeyPatch) -> Iterator[None]:
    """Reset the module-level client/disabled flags between tests."""
    monkeypatch.setattr(_langfuse_client, "_client", None, raising=False)
    monkeypatch.setattr(_langfuse_client, "_disabled", False, raising=False)
    for k in ("LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY", "LANGFUSE_HOST", "OAT_TRACE_ID"):
        monkeypatch.delenv(k, raising=False)
    yield


def test_get_client_returns_none_without_env() -> None:
    assert _langfuse_client.get_client() is None


def test_get_client_returns_none_with_partial_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("LANGFUSE_PUBLIC_KEY", "pk")
    # secret + trace missing
    assert _langfuse_client.get_client() is None


def test_get_client_caches_singleton(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("LANGFUSE_PUBLIC_KEY", "pk")
    monkeypatch.setenv("LANGFUSE_SECRET_KEY", "sk")
    monkeypatch.setenv("OAT_TRACE_ID", "trace-x")
    monkeypatch.setenv("LANGFUSE_HOST", "http://127.0.0.1:1")  # refused, but client builds fine
    a = _langfuse_client.get_client()
    b = _langfuse_client.get_client()
    assert a is b
    a.close(timeout=0.5)


class _StubHandler(http.server.BaseHTTPRequestHandler):
    received: list[dict[str, Any]] = []

    def do_POST(self) -> None:
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n).decode("utf-8")
        auth = self.headers.get("Authorization", "")
        decoded = ""
        if auth.startswith("Basic "):
            try:
                decoded = base64.b64decode(auth[6:]).decode("utf-8")
            except Exception:
                pass
        _StubHandler.received.append({"path": self.path, "auth": decoded, "body": body})
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')

    def log_message(self, *args: Any) -> None:  # noqa: D401, ARG002
        return


@pytest.fixture
def stub_server() -> Iterator[tuple[str, list[dict[str, Any]]]]:
    """Run a stub Langfuse server on an ephemeral port."""
    _StubHandler.received = []
    srv = http.server.HTTPServer(("127.0.0.1", 0), _StubHandler)
    port = srv.server_address[1]
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    try:
        yield f"http://127.0.0.1:{port}", _StubHandler.received
    finally:
        srv.shutdown()
        srv.server_close()


def test_emit_generation_round_trip(
    monkeypatch: pytest.MonkeyPatch, stub_server: tuple[str, list[dict[str, Any]]]
) -> None:
    host, received = stub_server
    monkeypatch.setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
    monkeypatch.setenv("LANGFUSE_SECRET_KEY", "sk-test")
    monkeypatch.setenv("OAT_TRACE_ID", "trace-unit-1")
    monkeypatch.setenv("LANGFUSE_HOST", host)

    client = _langfuse_client.get_client()
    assert client is not None

    client.emit_generation(
        name="llm_call",
        model="anthropic:claude-haiku-4-5",
        input_tokens=100,
        output_tokens=50,
        start_time="2026-05-18T16:00:00.000Z",
        end_time="2026-05-18T16:00:01.000Z",
        metadata={"wall_ms": 1000},
    )
    client.emit_tool_span(
        name="edit_file",
        start_time="2026-05-18T16:00:01.000Z",
        end_time="2026-05-18T16:00:01.005Z",
        metadata={"tool_call_id": "tc-1", "arg_count": 2},
    )
    client.close(timeout=3.0)

    assert received, "stub server received no events"
    # Decode all events from all batches.
    events: list[dict[str, Any]] = []
    for r in received:
        env = json.loads(r["body"])
        events.extend(env.get("batch", []))

    types = {e["type"] for e in events}
    assert "generation-create" in types
    assert "span-create" in types

    gen = next(e for e in events if e["type"] == "generation-create")
    assert gen["body"]["traceId"] == "trace-unit-1"
    assert gen["body"]["model"] == "anthropic:claude-haiku-4-5"
    assert gen["body"]["usage"]["input"] == 100
    assert gen["body"]["usage"]["output"] == 50

    span = next(e for e in events if e["type"] == "span-create")
    assert span["body"]["name"] == "tool:edit_file"

    # Basic-auth header was decoded correctly.
    assert any(r["auth"] == "pk-test:sk-test" for r in received)


def test_emit_to_refused_host_is_silent(monkeypatch: pytest.MonkeyPatch) -> None:
    """Emitting against an unreachable host must not raise."""
    monkeypatch.setenv("LANGFUSE_PUBLIC_KEY", "pk")
    monkeypatch.setenv("LANGFUSE_SECRET_KEY", "sk")
    monkeypatch.setenv("OAT_TRACE_ID", "trace-doomed")
    monkeypatch.setenv("LANGFUSE_HOST", "http://127.0.0.1:1")  # refused

    client = _langfuse_client.get_client()
    assert client is not None
    for _ in range(30):
        client.emit_generation(
            name="t",
            model="m",
            input_tokens=1,
            output_tokens=1,
            start_time="2026-05-18T16:00:00.000Z",
            end_time="2026-05-18T16:00:00.001Z",
        )
    client.close(timeout=2.0)
    # If we got here, no exception escaped.


def test_middleware_with_no_telemetry_is_pass_through() -> None:
    """LangfuseMiddleware must not interfere when telemetry env is unset."""
    mw = LangfuseMiddleware()

    sentinel = object()

    def handler(_req: Any) -> Any:
        return sentinel

    result = mw.wrap_model_call({"messages": []}, handler)
    assert result is sentinel


def test_middleware_swallows_emit_errors(monkeypatch: pytest.MonkeyPatch) -> None:
    """If the singleton client's emit raises, the agent path must still complete."""
    monkeypatch.setenv("LANGFUSE_PUBLIC_KEY", "pk")
    monkeypatch.setenv("LANGFUSE_SECRET_KEY", "sk")
    monkeypatch.setenv("OAT_TRACE_ID", "trace-bad")
    monkeypatch.setenv("LANGFUSE_HOST", "http://127.0.0.1:1")

    mw = LangfuseMiddleware()

    class FakeResp:
        result = []

    # Force emit_generation to blow up — the middleware must swallow it.
    real_client = _langfuse_client.get_client()
    assert real_client is not None
    real_client.emit_generation = mock.Mock(side_effect=RuntimeError("boom"))  # type: ignore[method-assign]

    def handler(_req: Any) -> Any:
        return FakeResp()

    result = mw.wrap_model_call({"messages": []}, handler)
    assert isinstance(result, FakeResp)
    real_client.close(timeout=0.5)
