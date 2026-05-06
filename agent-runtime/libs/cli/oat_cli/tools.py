"""Custom tools for the CLI agent."""

from __future__ import annotations

import ipaddress
from typing import TYPE_CHECKING, Any, Literal
from urllib.parse import urlparse

if TYPE_CHECKING:
    from tavily import TavilyClient

_UNSET = object()
_tavily_client: TavilyClient | object | None = _UNSET

# Hosts/IPs that an LLM tool call must not reach. Cloud-instance metadata
# services are the most common SSRF target — exfiltrating them yields IAM
# credentials. We only block the well-known metadata IPs and link-local
# range; deliberately do NOT block 127.0.0.1 / RFC1918 because dev workflows
# legitimately call localhost APIs.
_BLOCKED_HOSTS = frozenset(
    {
        "169.254.169.254",  # AWS / GCP / Azure / DigitalOcean / Oracle metadata
        "metadata.google.internal",
        "metadata.goog",
        "fd00:ec2::254",  # AWS IPv6 metadata
    }
)


def _ssrf_check(url: str) -> str | None:
    """Return a reason string if the URL is unsafe to fetch, else None.

    Blocks non-http(s) schemes (file://, gopher://, etc.) and well-known
    cloud-metadata endpoints. Does not block private/loopback ranges since
    dev tooling routinely calls them.
    """
    parsed = urlparse(url)
    if parsed.scheme not in ("http", "https"):
        return f"scheme '{parsed.scheme}' is not allowed (only http/https)"
    host = (parsed.hostname or "").lower()
    if not host:
        return "URL has no host"
    if host in _BLOCKED_HOSTS:
        return f"host '{host}' is a cloud-metadata endpoint"
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        return None  # hostname, not an IP — DNS resolution happens in requests
    if ip.is_link_local:
        return f"link-local address '{host}' is not allowed"
    return None


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

    if reason := _ssrf_check(url):
        return {
            "success": False,
            "status_code": 0,
            "headers": {},
            "content": f"Refused to fetch URL: {reason}",
            "url": url,
        }

    try:
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

        response = requests.request(method.upper(), url, timeout=timeout, **kwargs)

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

    try:
        response = requests.get(
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
    except requests.exceptions.RequestException as e:
        return {"error": f"Fetch URL error: {e!s}", "url": url}
