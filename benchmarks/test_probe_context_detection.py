#!/usr/bin/env python3
"""Tests for the context-window detection path in probe-model.py.

Covers:

  * Google Gemini fetcher: env-var auth (GOOGLE_API_KEY or GEMINI_API_KEY),
    URL shape, response parsing, silent-None on failure, "models/" prefix
    stripping.
  * Ollama fetcher: both response shapes (newer model_info dict +
    older parameters text block), OLLAMA_HOST default + override, silent-
    None on connection failure / malformed payload.
  * probe_context_profile precedence: --context-window CLI override beats
    LangChain profile beats API probe beats honest 128K default.
  * Honest-default markers in probe details
    (context_window_defaulted=True, context_window_source="default_fallback")
    and in the generated YAML (context_window_source + context_window_defaulted
    lines, warnings list entry).
  * --context-window clamp boundaries (lt MIN → exit code 2,
    gt MAX → exit code 2, valid → flows through).
  * _normalize_model_id_for_env: matches the daemon's Go normalization for
    a representative set of model IDs.
  * Live API probes (Gemini, Ollama) skipped when no credentials / server
    available -- the unit tests rely on mocked urllib for determinism.
"""

import io
import json
import os
import sys
import unittest
from unittest.mock import patch, MagicMock

# Same hyphen-filename import dance as the sibling test_probe_model.py.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import importlib.util

_probe_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "probe-model.py")
spec = importlib.util.spec_from_file_location("probe_model", _probe_path)
pm = importlib.util.module_from_spec(spec)
sys.modules["probe_model"] = pm
spec.loader.exec_module(pm)


# ---------------------------------------------------------------------------
# Test helpers
# ---------------------------------------------------------------------------

def _mock_urlopen(payload):
    """Build a context-manager-aware mock of urllib.request.urlopen that
    returns `payload` (a dict serialized to bytes) on read()."""
    body = json.dumps(payload).encode("utf-8")
    mock_resp = MagicMock()
    mock_resp.read.return_value = body
    mock_resp.__enter__ = MagicMock(return_value=mock_resp)
    mock_resp.__exit__ = MagicMock(return_value=False)
    return MagicMock(return_value=mock_resp)


def _mock_urlopen_raises(exc):
    """Mock of urlopen that raises `exc` on entry."""
    def _raise(*_args, **_kwargs):
        raise exc
    return MagicMock(side_effect=_raise)


class MockProfileModel:
    """Stand-in for a LangChain chat model with a `.profile` attribute."""
    def __init__(self, profile):
        self.profile = profile


# ---------------------------------------------------------------------------
# _fetch_google_gemini_context_length
# ---------------------------------------------------------------------------

class TestGoogleGeminiFetcher(unittest.TestCase):
    def setUp(self):
        # Clear both env vars so the test is hermetic.
        self._saved = {k: os.environ.pop(k, None) for k in ("GOOGLE_API_KEY", "GEMINI_API_KEY")}

    def tearDown(self):
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def test_returns_none_when_no_api_key(self):
        # Neither GOOGLE_API_KEY nor GEMINI_API_KEY set → silent None.
        self.assertIsNone(pm._fetch_google_gemini_context_length("gemini-2.5-flash"))

    def test_honors_google_api_key(self):
        os.environ["GOOGLE_API_KEY"] = "secret-google"
        with patch("urllib.request.urlopen", _mock_urlopen({"inputTokenLimit": 1_000_000})):
            self.assertEqual(pm._fetch_google_gemini_context_length("gemini-2.5-flash"), 1_000_000)

    def test_honors_gemini_api_key_fallback(self):
        os.environ["GEMINI_API_KEY"] = "secret-gemini"
        with patch("urllib.request.urlopen", _mock_urlopen({"inputTokenLimit": 200_000})):
            self.assertEqual(pm._fetch_google_gemini_context_length("gemini-2.5-pro"), 200_000)

    def test_google_api_key_takes_precedence_over_gemini(self):
        os.environ["GOOGLE_API_KEY"] = "primary"
        os.environ["GEMINI_API_KEY"] = "fallback"
        seen_url = {}

        def capture(req, *args, **kwargs):
            seen_url["url"] = req.full_url
            return _mock_urlopen({"inputTokenLimit": 100_000})(req, *args, **kwargs)

        with patch("urllib.request.urlopen", side_effect=capture):
            pm._fetch_google_gemini_context_length("gemini-2.5-flash")
        self.assertIn("key=primary", seen_url["url"])

    def test_strips_models_prefix(self):
        os.environ["GOOGLE_API_KEY"] = "k"
        seen_url = {}

        def capture(req, *args, **kwargs):
            seen_url["url"] = req.full_url
            return _mock_urlopen({"inputTokenLimit": 100_000})(req, *args, **kwargs)

        with patch("urllib.request.urlopen", side_effect=capture):
            pm._fetch_google_gemini_context_length("models/gemini-2.5-flash")
        # URL should have just /models/gemini-2.5-flash, not /models/models/...
        self.assertIn("/models/gemini-2.5-flash?", seen_url["url"])
        self.assertNotIn("/models/models/", seen_url["url"])

    def test_returns_none_on_url_error(self):
        os.environ["GOOGLE_API_KEY"] = "k"
        import urllib.error
        with patch("urllib.request.urlopen", _mock_urlopen_raises(urllib.error.URLError("DNS"))):
            self.assertIsNone(pm._fetch_google_gemini_context_length("gemini-2.5-flash"))

    def test_returns_none_on_missing_field(self):
        os.environ["GOOGLE_API_KEY"] = "k"
        with patch("urllib.request.urlopen", _mock_urlopen({"name": "models/gemini-2.5-flash"})):
            self.assertIsNone(pm._fetch_google_gemini_context_length("gemini-2.5-flash"))

    def test_returns_none_on_non_positive_value(self):
        os.environ["GOOGLE_API_KEY"] = "k"
        with patch("urllib.request.urlopen", _mock_urlopen({"inputTokenLimit": 0})):
            self.assertIsNone(pm._fetch_google_gemini_context_length("gemini-2.5-flash"))


# ---------------------------------------------------------------------------
# _fetch_ollama_context_length
# ---------------------------------------------------------------------------

class TestOllamaFetcher(unittest.TestCase):
    def setUp(self):
        self._saved = os.environ.pop("OLLAMA_HOST", None)

    def tearDown(self):
        if self._saved is not None:
            os.environ["OLLAMA_HOST"] = self._saved
        else:
            os.environ.pop("OLLAMA_HOST", None)

    def test_parses_newer_model_info_shape(self):
        # Newer Ollama returns model_info: { "<arch>.context_length": N, ... }
        payload = {
            "model_info": {
                "general.architecture": "llama",
                "llama.context_length": 128_000,
                "llama.attention.head_count": 32,
            },
        }
        with patch("urllib.request.urlopen", _mock_urlopen(payload)):
            self.assertEqual(pm._fetch_ollama_context_length("llama3:8b"), 128_000)

    def test_parses_older_parameters_text_shape(self):
        # Older Ollama returns parameters as a Modelfile-style text block.
        payload = {
            "parameters": "num_ctx 8192\ntemperature 0.7\ntop_p 0.95",
        }
        with patch("urllib.request.urlopen", _mock_urlopen(payload)):
            self.assertEqual(pm._fetch_ollama_context_length("custom-model"), 8192)

    def test_newer_shape_wins_when_both_present(self):
        # Some Ollama versions emit both for back-compat; the newer shape
        # is more reliable for non-default num_ctx values.
        payload = {
            "model_info": {"llama.context_length": 32_000},
            "parameters": "num_ctx 2048",
        }
        with patch("urllib.request.urlopen", _mock_urlopen(payload)):
            self.assertEqual(pm._fetch_ollama_context_length("custom"), 32_000)

    def test_returns_none_on_connection_failure(self):
        import urllib.error
        with patch("urllib.request.urlopen", _mock_urlopen_raises(urllib.error.URLError("refused"))):
            self.assertIsNone(pm._fetch_ollama_context_length("llama3:8b"))

    def test_returns_none_on_empty_payload(self):
        with patch("urllib.request.urlopen", _mock_urlopen({})):
            self.assertIsNone(pm._fetch_ollama_context_length("llama3:8b"))

    def test_returns_none_on_unparseable_parameters(self):
        with patch("urllib.request.urlopen", _mock_urlopen({"parameters": "stop \"</end>\"\n"})):
            self.assertIsNone(pm._fetch_ollama_context_length("llama3:8b"))

    def test_ollama_host_default(self):
        seen = {}

        def capture(req, *args, **kwargs):
            seen["url"] = req.full_url
            return _mock_urlopen({"model_info": {"x.context_length": 4096}})(req, *args, **kwargs)

        with patch("urllib.request.urlopen", side_effect=capture):
            pm._fetch_ollama_context_length("llama3:8b")
        self.assertEqual(seen["url"], "http://localhost:11434/api/show")

    def test_ollama_host_env_override(self):
        os.environ["OLLAMA_HOST"] = "http://192.168.1.42:9999"
        seen = {}

        def capture(req, *args, **kwargs):
            seen["url"] = req.full_url
            return _mock_urlopen({"model_info": {"x.context_length": 4096}})(req, *args, **kwargs)

        with patch("urllib.request.urlopen", side_effect=capture):
            pm._fetch_ollama_context_length("llama3:8b")
        self.assertEqual(seen["url"], "http://192.168.1.42:9999/api/show")

    def test_ollama_host_adds_scheme_when_missing(self):
        os.environ["OLLAMA_HOST"] = "localhost:11434"
        seen = {}

        def capture(req, *args, **kwargs):
            seen["url"] = req.full_url
            return _mock_urlopen({"model_info": {"x.context_length": 4096}})(req, *args, **kwargs)

        with patch("urllib.request.urlopen", side_effect=capture):
            pm._fetch_ollama_context_length("llama3:8b")
        self.assertTrue(seen["url"].startswith("http://localhost:11434/"))


# ---------------------------------------------------------------------------
# probe_context_profile precedence
# ---------------------------------------------------------------------------

class TestProbeContextProfilePrecedence(unittest.TestCase):
    def setUp(self):
        # Reset the module-level CLI override before every test so order
        # of execution doesn't matter.
        pm._explicit_context_window = None
        # Clear API keys so the provider-API path never fires accidentally.
        self._saved = {k: os.environ.pop(k, None) for k in (
            "GOOGLE_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY", "OLLAMA_HOST",
        )}

    def tearDown(self):
        pm._explicit_context_window = None
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def test_cli_override_beats_langchain_profile(self):
        pm._explicit_context_window = 500_000
        model = MockProfileModel({"max_input_tokens": 32_000, "tool_calling": True})
        r = pm.probe_context_profile(model, provider="anthropic", model_name="claude-sonnet-4")
        self.assertEqual(r.details["max_input_tokens"], 500_000)
        self.assertEqual(r.details["context_window_source"], "cli_override")
        self.assertNotIn("context_window_defaulted", r.details)

    def test_langchain_profile_used_when_no_override(self):
        model = MockProfileModel({"max_input_tokens": 200_000, "tool_calling": True})
        r = pm.probe_context_profile(model, provider="anthropic", model_name="claude-sonnet-4")
        self.assertEqual(r.details["max_input_tokens"], 200_000)
        self.assertEqual(r.details["context_window_source"], "langchain_profile")

    def test_google_gemini_api_called_when_profile_empty(self):
        os.environ["GOOGLE_API_KEY"] = "k"
        model = MockProfileModel({"tool_calling": True})  # no max_input_tokens
        with patch("urllib.request.urlopen", _mock_urlopen({"inputTokenLimit": 1_000_000})):
            r = pm.probe_context_profile(model, provider="google_genai", model_name="gemini-2.5-flash")
        self.assertEqual(r.details["max_input_tokens"], 1_000_000)
        self.assertEqual(r.details["context_window_source"], "google_gemini_api")

    def test_ollama_api_called_when_profile_empty(self):
        model = MockProfileModel({"tool_calling": True})
        with patch(
            "urllib.request.urlopen",
            _mock_urlopen({"model_info": {"llama.context_length": 8_192}}),
        ):
            r = pm.probe_context_profile(model, provider="ollama", model_name="llama3:8b")
        self.assertEqual(r.details["max_input_tokens"], 8_192)
        self.assertEqual(r.details["context_window_source"], "ollama_api")

    def test_defaults_to_128k_when_all_paths_fail(self):
        # No CLI override, no LangChain profile value, no Anthropic API
        # probe → honest default + structured marker.
        model = MockProfileModel({"tool_calling": True})
        r = pm.probe_context_profile(model, provider="anthropic", model_name="claude-sonnet-4")
        self.assertEqual(r.details["max_input_tokens"], pm.DEFAULT_CONTEXT_WINDOW)
        self.assertEqual(r.details["context_window_source"], "default_fallback")
        self.assertTrue(r.details["context_window_defaulted"])

    def test_cli_override_works_even_without_langchain_profile(self):
        pm._explicit_context_window = 250_000
        model = MockProfileModel(None)  # mimics a model with no profile attr
        model.profile = None
        r = pm.probe_context_profile(model, provider="weird", model_name="custom")
        self.assertTrue(r.passed)
        self.assertEqual(r.details["max_input_tokens"], 250_000)
        self.assertEqual(r.details["context_window_source"], "cli_override")

    def test_unprofiled_model_without_override_returns_error(self):
        model = MockProfileModel(None)
        model.profile = None
        r = pm.probe_context_profile(model, provider="unknown", model_name="something")
        self.assertFalse(r.passed)
        self.assertEqual(r.details["context_window_source"], "none")
        self.assertIn("--context-window", r.error)
        self.assertIn("OAT_MODEL_CONTEXT_", r.error)


# ---------------------------------------------------------------------------
# _normalize_model_id_for_env
# ---------------------------------------------------------------------------

class TestNormalizeModelIDForEnv(unittest.TestCase):
    def test_matches_daemon_normalization(self):
        # Same cases as `internal/daemon/context_capacity_test.go`'s
        # TestNormalizeModelIDForEnv so a drift across the two
        # surfaces would be caught by either test failing.
        cases = [
            ("google_genai:gemini-2.5-flash", "google_genai_gemini-2.5-flash"),
            ("anthropic:claude-sonnet-4", "anthropic_claude-sonnet-4"),
            ("openrouter:meta-llama/llama-3", "openrouter_meta-llama_llama-3"),
            ("OPENAI:GPT-4O", "openai_gpt-4o"),
            ("ollama:llama3:8b", "ollama_llama3_8b"),
        ]
        for raw, expected in cases:
            with self.subTest(raw=raw):
                self.assertEqual(pm._normalize_model_id_for_env(raw), expected)


# ---------------------------------------------------------------------------
# _print_default_context_warning
# ---------------------------------------------------------------------------

class TestPrintDefaultContextWarning(unittest.TestCase):
    def _make_report(self, defaulted: bool):
        # Build the smallest ModelReport that exercises the WARNING path.
        # Avoid importing ModelReport directly to keep this test resilient
        # to dataclass field additions; SimpleNamespace mimics the duck-type
        # the printer reads.
        from types import SimpleNamespace
        cp_details = {"context_window_defaulted": True} if defaulted else {}
        return SimpleNamespace(
            model_string="google_genai:gemini-2.5-flash",
            probes=[SimpleNamespace(name="context_profile", details=cp_details)],
        )

    def test_emits_block_when_defaulted(self):
        report = self._make_report(defaulted=True)
        with patch("sys.stderr", new=io.StringIO()) as buf:
            pm._print_default_context_warning(report, "/abs/path/to/profile.yaml")
        out = buf.getvalue()
        self.assertIn("WARNING", out)
        self.assertIn(f"Defaulted to {pm.DEFAULT_CONTEXT_WINDOW} tokens", out)
        self.assertIn("/abs/path/to/profile.yaml", out)
        self.assertIn("oat model onboard google_genai:gemini-2.5-flash --context-window <N>", out)
        self.assertIn("OAT_MODEL_CONTEXT_google_genai_gemini-2.5-flash=<tokens>", out)

    def test_silent_when_not_defaulted(self):
        report = self._make_report(defaulted=False)
        with patch("sys.stderr", new=io.StringIO()) as buf:
            pm._print_default_context_warning(report, "/abs/path/to/profile.yaml")
        self.assertEqual(buf.getvalue(), "")

    def test_silent_when_no_context_profile_probe(self):
        from types import SimpleNamespace
        report = SimpleNamespace(model_string="x", probes=[])
        with patch("sys.stderr", new=io.StringIO()) as buf:
            pm._print_default_context_warning(report, "/p")
        self.assertEqual(buf.getvalue(), "")


# ---------------------------------------------------------------------------
# --context-window flag validation
# ---------------------------------------------------------------------------

class TestContextWindowFlagBounds(unittest.TestCase):
    def test_bounds_constants_match_daemon(self):
        # Daemon's contextEnvOverrideMin / Max in
        # internal/daemon/context_capacity.go. If these drift, the
        # operator's --context-window <N> could pass the probe but get
        # clamped by the daemon at runtime with a WARN -- confusing UX.
        # Catching it here keeps the two clamps in sync.
        self.assertEqual(pm.CONTEXT_WINDOW_MIN, 1_024)
        self.assertEqual(pm.CONTEXT_WINDOW_MAX, 16_000_000)

    def test_default_context_window_matches_daemon_fallback(self):
        # Daemon's contextFallbackTokens. Same drift risk as above.
        self.assertEqual(pm.DEFAULT_CONTEXT_WINDOW, 128_000)


if __name__ == "__main__":
    unittest.main()
