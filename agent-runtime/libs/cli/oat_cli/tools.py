"""Custom tools for the CLI agent."""

from __future__ import annotations

import ipaddress
import socket
from typing import TYPE_CHECKING, Any, Literal
from urllib.parse import urlsplit

if TYPE_CHECKING:
    import requests
    from tavily import TavilyClient

_UNSET = object()
_tavily_client: TavilyClient | object | None = _UNSET


def _get_tavily_client() -> TavilyClient | None:
    """Get or initialize the lazy Tavily client singleton.

    Returns:
        TavilyClient instance, or None if API key is not configured.
    """
    global _tavily_client  # noqa: PLW0603  # Module-level cache requires global statement
    if _tavily_client is not _UNSET:
        return _tavily_client  # type: ignore[return-value]  # narrowed by sentinel check

    from oat_cli.config import settings

    if settings.has_tavily:
        from tavily import TavilyClient as _TavilyClient

        _tavily_client = _TavilyClient(api_key=settings.tavily_api_key)
    else:
        _tavily_client = None
    return _tavily_client


class BlockedRequestError(Exception):
    """Raised when an outbound HTTP request targets a disallowed address.

    This is a narrow SSRF guard. The agent chooses the URL for
    ``http_request`` / ``fetch_url`` and processes untrusted input, so a
    successful prompt injection could otherwise coax it into hitting an
    internal endpoint. We block the highest-impact escalation path only:
    link-local addresses (IPv4 ``169.254.0.0/16`` — which includes the
    cloud metadata endpoint ``169.254.169.254`` — and IPv6 ``fe80::/10``)
    and any non-http(s) URL scheme. Loopback and private LAN ranges are
    intentionally still reachable so agents can fetch localhost dev
    servers and internal services.
    """


_ALLOWED_SCHEMES = frozenset({"http", "https"})


def _is_blocked_ip(value: str) -> bool:
    """Return True if ``value`` is a link-local IP literal.

    Non-IP strings (hostnames) return False; callers resolve hostnames
    separately and pass each resolved IP here.
    """
    try:
        addr = ipaddress.ip_address(value)
    except ValueError:
        return False
    return addr.is_link_local


def _resolved_ips(host: str) -> list[str]:
    """Resolve ``host`` to its A/AAAA records.

    A transient DNS failure (no network, NXDOMAIN) must not become a hard
    block, and unit tests stay hermetic, so resolution failures yield an
    empty list and the request proceeds (failing later at connect time).

    Args:
        host: Hostname or IP literal to resolve.

    Returns:
        The list of resolved IP address strings, or ``[]`` on failure.
    """
    try:
        infos = socket.getaddrinfo(host, None)
    except socket.gaierror:
        return []
    return [info[4][0] for info in infos]


def _assert_url_allowed(url: str) -> None:
    """Reject non-http(s) schemes and link-local/metadata destinations.

    Args:
        url: The URL about to be requested.

    Raises:
        BlockedRequestError: if the scheme is not http/https, the URL has
            no host, or the host is (or resolves to) a link-local address.
    """
    parts = urlsplit(url)
    scheme = parts.scheme.lower()
    if scheme not in _ALLOWED_SCHEMES:
        msg = f"refusing non-http(s) URL scheme: {scheme or '(none)'!r}"
        raise BlockedRequestError(msg)
    host = parts.hostname
    if not host:
        msg = "refusing URL with no host"
        raise BlockedRequestError(msg)
    if _is_blocked_ip(host):
        msg = f"refusing link-local/metadata address: {host}"
        raise BlockedRequestError(msg)
    for ip in _resolved_ips(host):
        if _is_blocked_ip(ip):
            msg = f"refusing {host!r}: resolves to link-local/metadata address {ip}"
            raise BlockedRequestError(msg)


def _guarded_session() -> requests.Session:
    """Build a ``requests.Session`` that validates every request hop.

    The guard is mounted as a transport adapter, so it runs on the
    initial request *and* on each redirect hop (requests re-enters the
    adapter for every ``3xx`` Location), closing the redirect-to-metadata
    bypass that a one-shot pre-flight check would miss.

    Returns:
        A session whose http/https adapters reject link-local/metadata
        targets before connecting.
    """
    import requests
    from requests.adapters import HTTPAdapter

    class _GuardedAdapter(HTTPAdapter):
        def send(
            self,
            request: requests.PreparedRequest,
            *args: Any,
            **kwargs: Any,
        ) -> requests.Response:
            _assert_url_allowed(request.url or "")
            return super().send(request, *args, **kwargs)

    session = requests.Session()
    adapter = _GuardedAdapter()
    session.mount("http://", adapter)
    session.mount("https://", adapter)
    return session


def http_request(
    url: str,
    method: str = "GET",
    headers: dict[str, str] | None = None,
    data: str | dict | None = None,
    params: dict[str, str] | None = None,
    timeout: int = 30,
) -> dict[str, Any]:
    """Make HTTP requests to APIs and web services.

    Args:
        url: Target URL
        method: HTTP method (GET, POST, PUT, DELETE, etc.)
        headers: HTTP headers to include
        data: Request body data (string or dict)
        params: URL query parameters
        timeout: Request timeout in seconds

    Returns:
        Dictionary with response data including status, headers, and content
    """
    import requests

    session = _guarded_session()
    try:
        # Validate the initial URL up front (scheme + host). The mounted
        # adapter re-checks every redirect hop, but a non-http(s) scheme
        # never reaches an adapter, so it must be caught here.
        _assert_url_allowed(url)

        kwargs: dict[str, Any] = {}

        if headers:
            kwargs["headers"] = headers
        if params:
            kwargs["params"] = params
        if data:
            if isinstance(data, dict):
                kwargs["json"] = data
            else:
                kwargs["data"] = data

        response = session.request(method.upper(), url, timeout=timeout, **kwargs)

        try:
            content = response.json()
        except (ValueError, requests.exceptions.JSONDecodeError):
            content = response.text

        return {
            "success": response.status_code < 400,  # noqa: PLR2004  # HTTP status code threshold
            "status_code": response.status_code,
            "headers": dict(response.headers),
            "content": content,
            "url": response.url,
        }

    except BlockedRequestError as e:
        return {
            "success": False,
            "status_code": 0,
            "headers": {},
            "content": f"Request blocked: {e!s}",
            "url": url,
        }
    except requests.exceptions.Timeout:
        return {
            "success": False,
            "status_code": 0,
            "headers": {},
            "content": f"Request timed out after {timeout} seconds",
            "url": url,
        }
    except requests.exceptions.RequestException as e:
        return {
            "success": False,
            "status_code": 0,
            "headers": {},
            "content": f"Request error: {e!s}",
            "url": url,
        }
    finally:
        session.close()


def web_search(  # noqa: ANN201  # Return type depends on dynamic tool configuration
    query: str,
    max_results: int = 5,
    topic: Literal["general", "news", "finance"] = "general",
    include_raw_content: bool = False,
):
    """Search the web using Tavily for current information and documentation.

    This tool searches the web and returns relevant results. After receiving results,
    you MUST synthesize the information into a natural, helpful response for the user.

    Args:
        query: The search query (be specific and detailed)
        max_results: Number of results to return (default: 5)
        topic: Search topic type - "general" for most queries, "news" for current events
        include_raw_content: Include full page content (warning: uses more tokens)

    Returns:
        Dictionary containing:
        - results: List of search results, each with:
            - title: Page title
            - url: Page URL
            - content: Relevant excerpt from the page
            - score: Relevance score (0-1)
        - query: The original search query

    IMPORTANT: After using this tool:
    1. Read through the 'content' field of each result
    2. Extract relevant information that answers the user's question
    3. Synthesize this into a clear, natural language response
    4. Cite sources by mentioning the page titles or URLs
    5. NEVER show the raw JSON to the user - always provide a formatted response
    """
    try:
        import requests
        from tavily import (
            BadRequestError,
            InvalidAPIKeyError,
            MissingAPIKeyError,
            UsageLimitExceededError,
        )
        from tavily.errors import ForbiddenError, TimeoutError as TavilyTimeoutError
    except ImportError as exc:
        return {
            "error": f"Required package not installed: {exc.name}. "
            "Install with: pip install 'oat_sdk[cli]'",
            "query": query,
        }

    client = _get_tavily_client()
    if client is None:
        return {
            "error": "Tavily API key not configured. "
            "Please set TAVILY_API_KEY environment variable.",
            "query": query,
        }

    try:
        return client.search(
            query,
            max_results=max_results,
            include_raw_content=include_raw_content,
            topic=topic,
        )
    except (
        requests.exceptions.RequestException,
        ValueError,
        TypeError,
        # Tavily-specific exceptions
        BadRequestError,
        ForbiddenError,
        InvalidAPIKeyError,
        MissingAPIKeyError,
        TavilyTimeoutError,
        UsageLimitExceededError,
    ) as e:
        return {"error": f"Web search error: {e!s}", "query": query}


def fetch_url(url: str, timeout: int = 30) -> dict[str, Any]:
    """Fetch content from a URL and convert HTML to markdown format.

    This tool fetches web page content and converts it to clean markdown text,
    making it easy to read and process HTML content. After receiving the markdown,
    you MUST synthesize the information into a natural, helpful response for the user.

    Args:
        url: The URL to fetch (must be a valid HTTP/HTTPS URL)
        timeout: Request timeout in seconds (default: 30)

    Returns:
        Dictionary containing:
        - success: Whether the request succeeded
        - url: The final URL after redirects
        - markdown_content: The page content converted to markdown
        - status_code: HTTP status code
        - content_length: Length of the markdown content in characters

    IMPORTANT: After using this tool:
    1. Read through the markdown content
    2. Extract relevant information that answers the user's question
    3. Synthesize this into a clear, natural language response
    4. NEVER show the raw markdown to the user unless specifically requested
    """
    try:
        import requests
        from markdownify import markdownify
    except ImportError as exc:
        return {
            "error": f"Required package not installed: {exc.name}. "
            "Install with: pip install 'oat_sdk[cli]'",
            "url": url,
        }

    session = _guarded_session()
    try:
        # Validate up front so non-http(s) schemes (which never reach the
        # mounted adapter) are rejected; the adapter covers redirect hops.
        _assert_url_allowed(url)

        response = session.get(
            url,
            timeout=timeout,
            headers={"User-Agent": "Mozilla/5.0 (compatible; OatSdks/1.0)"},
        )
        response.raise_for_status()

        # Convert HTML content to markdown
        markdown_content = markdownify(response.text)

        return {
            "url": str(response.url),
            "markdown_content": markdown_content,
            "status_code": response.status_code,
            "content_length": len(markdown_content),
        }
    except BlockedRequestError as e:
        return {"error": f"Fetch URL blocked: {e!s}", "url": url}
    except requests.exceptions.RequestException as e:
        return {"error": f"Fetch URL error: {e!s}", "url": url}
    finally:
        session.close()
