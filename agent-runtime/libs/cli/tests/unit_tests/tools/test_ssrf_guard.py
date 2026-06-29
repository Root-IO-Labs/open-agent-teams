"""Tests for the narrow SSRF guard on http_request / fetch_url.

The guard blocks link-local / cloud-metadata destinations (e.g.
169.254.169.254) and non-http(s) schemes, while deliberately leaving
loopback and private LAN ranges reachable so agents can still hit
localhost dev servers and internal services.
"""

import pytest
import responses

from oat_cli.tools import (
    BlockedRequestError,
    _assert_url_allowed,
    fetch_url,
    http_request,
)


def _resolve_to(monkeypatch: pytest.MonkeyPatch, ip: str) -> None:
    """Force every hostname resolution to a single IP."""
    monkeypatch.setattr(
        "oat_cli.tools.socket.getaddrinfo",
        lambda *_a, **_k: [(0, 0, 0, "", (ip, 0))],
    )


class TestAssertUrlAllowed:
    def test_blocks_metadata_ip_literal(self) -> None:
        with pytest.raises(BlockedRequestError):
            _assert_url_allowed("http://169.254.169.254/latest/meta-data/")

    def test_blocks_ipv4_link_local_range(self) -> None:
        with pytest.raises(BlockedRequestError):
            _assert_url_allowed("http://169.254.1.1/")

    def test_blocks_ipv6_link_local(self) -> None:
        with pytest.raises(BlockedRequestError):
            _assert_url_allowed("http://[fe80::1]/")

    @pytest.mark.parametrize("scheme", ["file", "gopher", "ftp", "data"])
    def test_blocks_non_http_schemes(self, scheme: str) -> None:
        with pytest.raises(BlockedRequestError):
            _assert_url_allowed(f"{scheme}://etc/passwd")

    def test_blocks_hostname_resolving_to_metadata(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        _resolve_to(monkeypatch, "169.254.169.254")
        with pytest.raises(BlockedRequestError):
            _assert_url_allowed("http://metadata.example.com/")

    def test_allows_loopback(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Localhost stays reachable on purpose (narrow guard).
        _resolve_to(monkeypatch, "127.0.0.1")
        _assert_url_allowed("http://localhost:8080/health")

    def test_allows_private_lan(self, monkeypatch: pytest.MonkeyPatch) -> None:
        _resolve_to(monkeypatch, "10.0.0.5")
        _assert_url_allowed("http://internal.example.com/")

    def test_allows_public_host(self, monkeypatch: pytest.MonkeyPatch) -> None:
        _resolve_to(monkeypatch, "93.184.216.34")
        _assert_url_allowed("https://example.com/")


class TestHttpRequestGuard:
    def test_http_request_blocks_metadata(self) -> None:
        result = http_request("http://169.254.169.254/latest/meta-data/")
        assert result["success"] is False
        assert "blocked" in result["content"].lower()

    def test_http_request_blocks_file_scheme(self) -> None:
        result = http_request("file:///etc/passwd")
        assert result["success"] is False
        assert "blocked" in result["content"].lower()

    @responses.activate
    def test_http_request_allows_normal_url(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        _resolve_to(monkeypatch, "93.184.216.34")
        responses.add(
            responses.GET, "https://example.com/api", json={"ok": True}, status=200
        )
        result = http_request("https://example.com/api")
        assert result["success"] is True
        assert result["status_code"] == 200


class TestFetchUrlGuard:
    def test_fetch_url_blocks_metadata(self) -> None:
        result = fetch_url("http://169.254.169.254/latest/meta-data/")
        assert "error" in result
        assert "blocked" in result["error"].lower()

    @responses.activate
    def test_fetch_url_blocks_redirect_to_metadata(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # First hop resolves public; the guard must re-check the redirect
        # target, which resolves to the metadata IP.
        responses.add(
            responses.GET,
            "https://example.com/redir",
            status=302,
            headers={"Location": "http://169.254.169.254/latest/meta-data/"},
        )
        _resolve_to(monkeypatch, "93.184.216.34")
        result = fetch_url("https://example.com/redir")
        assert "error" in result
        assert "blocked" in result["error"].lower()
