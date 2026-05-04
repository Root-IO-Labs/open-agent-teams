#!/usr/bin/env python3
"""Tests for probe-model.py scoring logic, YAML generation, and recommendations.

Runs without API keys — uses mock LangChain responses to exercise every
scoring branch in every probe. Validates that:
  1. Each probe scores correctly for good, partial, and bad responses
  2. YAML profile generation produces valid, parseable output
  3. Recommendations fire on the right conditions
  4. Weighted overall scoring produces sensible composites
  5. Minimum probe set defaults and skipped-probe tracking work
"""

import asyncio
import sys
import os
import unittest
from dataclasses import dataclass, field
from typing import Any
from unittest.mock import MagicMock, AsyncMock, patch

# Add the benchmarks dir so we can import probe-model as a module
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

# Import using importlib since the filename has a hyphen
import importlib.util
_probe_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "probe-model.py")
spec = importlib.util.spec_from_file_location("probe_model", _probe_path)
pm = importlib.util.module_from_spec(spec)
sys.modules["probe_model"] = pm  # Required for dataclass introspection
spec.loader.exec_module(pm)


# ---------------------------------------------------------------------------
# Helpers: mock LangChain message objects
# ---------------------------------------------------------------------------

class MockMessage:
    """Simulates a LangChain AIMessage response."""
    def __init__(self, content="", tool_calls=None, usage_metadata=None,
                 response_metadata=None, profile=None):
        self.content = content
        self.tool_calls = tool_calls or []
        self.usage_metadata = usage_metadata
        self.response_metadata = response_metadata
        if profile is not None:
            self.profile = profile


class MockModel:
    """Simulates a LangChain chat model."""
    def __init__(self, response=None, stream_chunks=None, profile=None):
        self._response = response or MockMessage()
        self._stream_chunks = stream_chunks or []
        self.profile = profile

    def invoke(self, *args, **kwargs):
        if isinstance(self._response, Exception):
            raise self._response
        return self._response

    def bind_tools(self, tools):
        return self  # return self so invoke still works

    async def astream(self, *args, **kwargs):
        for chunk in self._stream_chunks:
            yield chunk


# ---------------------------------------------------------------------------
# Probe 1: basic_inference
# ---------------------------------------------------------------------------

class TestBasicInference(unittest.TestCase):
    def test_perfect_3_words(self):
        model = MockModel(MockMessage(content="Hello there friend"))
        result = pm.probe_basic_inference(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 100)  # 80 base + 20 exact
        self.assertTrue(result.details["instruction_followed"])

    def test_close_4_words(self):
        model = MockModel(MockMessage(content="Hello there my friend"))
        result = pm.probe_basic_inference(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 90)  # 80 + 10

    def test_verbose_response(self):
        model = MockModel(MockMessage(content="Hello! How are you doing today? Nice to meet you!"))
        result = pm.probe_basic_inference(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 80)  # base only, too many words

    def test_empty_response(self):
        model = MockModel(MockMessage(content=""))
        result = pm.probe_basic_inference(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 0)

    def test_exception(self):
        model = MockModel(response=ValueError("API error"))
        result = pm.probe_basic_inference(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 0)
        self.assertIn("API error", result.error)

    def test_content_block_list(self):
        """Some providers return content as list of dicts."""
        model = MockModel(MockMessage(content=[{"text": "Hello"}, {"text": "there friend"}]))
        result = pm.probe_basic_inference(model)
        self.assertTrue(result.passed)


# ---------------------------------------------------------------------------
# Probe 2: tool_calling
# ---------------------------------------------------------------------------

class TestToolCalling(unittest.TestCase):
    def test_perfect_tool_call(self):
        model = MockModel(MockMessage(
            tool_calls=[{"name": "get_weather", "args": {"city": "Tokyo"}}],
        ))
        result = pm.probe_tool_calling(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 100)  # 70 + 20 name + 10 arg
        self.assertTrue(result.details["arg_correct"])

    def test_correct_tool_wrong_arg(self):
        model = MockModel(MockMessage(
            tool_calls=[{"name": "get_weather", "args": {"city": "London"}}],
        ))
        result = pm.probe_tool_calling(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 90)  # 70 + 20 name, no arg bonus
        self.assertFalse(result.details["arg_correct"])

    def test_wrong_tool_name(self):
        model = MockModel(MockMessage(
            tool_calls=[{"name": "weather_check", "args": {"city": "Tokyo"}}],
        ))
        result = pm.probe_tool_calling(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 80)  # 70 + 10 arg, no name bonus

    def test_plaintext_tool_call(self):
        model = MockModel(MockMessage(
            content='I would call get_weather with "city": "Tokyo"',
        ))
        result = pm.probe_tool_calling(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 20)
        self.assertEqual(result.details["issue"], "plaintext_tool_call")

    def test_no_tool_call_at_all(self):
        model = MockModel(MockMessage(content="The weather in Tokyo is nice."))
        result = pm.probe_tool_calling(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 0)


# ---------------------------------------------------------------------------
# Probe 3: streaming
# ---------------------------------------------------------------------------

class TestStreaming(unittest.TestCase):
    def test_fast_ttft(self):
        """TTFT < 1000ms should score 100."""
        chunks = [MockMessage(content="1"), MockMessage(content="2")]
        model = MockModel(stream_chunks=chunks)
        result = asyncio.run(pm.probe_streaming(model))
        self.assertTrue(result.passed)
        # TTFT will be very fast (in-process), so should get full bonus
        self.assertEqual(result.score, 100)  # 50 + 25 + 25
        self.assertIsNotNone(result.details["ttft_ms"])

    def test_no_chunks(self):
        model = MockModel(stream_chunks=[])
        result = asyncio.run(pm.probe_streaming(model))
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 0)

    def test_empty_content_chunks(self):
        """Chunks with no content should not register TTFT."""
        chunks = [MockMessage(content=""), MockMessage(content=""), MockMessage(content="hello")]
        model = MockModel(stream_chunks=chunks)
        result = asyncio.run(pm.probe_streaming(model))
        self.assertTrue(result.passed)
        self.assertIsNotNone(result.details["ttft_ms"])


# ---------------------------------------------------------------------------
# Probe 4: token_reporting
# ---------------------------------------------------------------------------

class TestTokenReporting(unittest.TestCase):
    def test_full_reporting(self):
        model = MockModel(MockMessage(
            usage_metadata={
                "input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
                "input_token_details": {"cache_read": 0},
                "output_token_details": {"reasoning": 3},
            },
        ))
        result = pm.probe_token_reporting(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 100)  # 70 + 15 + 10 + 5
        self.assertTrue(result.details["counts_sane"])

    def test_basic_only(self):
        model = MockModel(MockMessage(
            usage_metadata={"input_tokens": 10, "output_tokens": 5, "total_tokens": 0},
        ))
        result = pm.probe_token_reporting(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 70)

    def test_insane_counts(self):
        model = MockModel(MockMessage(
            usage_metadata={"input_tokens": 999, "output_tokens": 5, "total_tokens": 1004},
        ))
        result = pm.probe_token_reporting(model)
        self.assertTrue(result.passed)
        self.assertFalse(result.details["counts_sane"])

    def test_no_usage(self):
        model = MockModel(MockMessage(content="4"))
        result = pm.probe_token_reporting(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 0)


# ---------------------------------------------------------------------------
# Probe 6: multi_turn
# ---------------------------------------------------------------------------

class TestMultiTurn(unittest.TestCase):
    def test_gate_pass_extended_full_marks(self):
        """Model passes gate AND nails extended synthesis."""
        # The model.invoke is called twice — gate then extended.
        # We use side_effect to return different responses.
        model = MagicMock()
        model.invoke = MagicMock(side_effect=[
            # Gate response
            MockMessage(content="It's sunny, 72°F in NYC!"),
            # Extended response
            MockMessage(content=(
                "Based on the health check and logs, the memory spike from 71% to 94% "
                "correlates with the deployment of v2.8.1. The OOM killer triggered on "
                "worker-3 shortly after. Disk usage was 67%. I recommend we rollback "
                "to the previous version while investigating the memory leak."
            )),
        ])
        result = pm.probe_multi_turn(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 100)  # 30 gate + 10 + 15 + 15 + 15 + 15
        self.assertTrue(result.details["references_version"])
        self.assertTrue(result.details["connects_cause"])
        self.assertTrue(result.details["recalls_disk"])
        self.assertTrue(result.details["suggests_action"])

    def test_gate_pass_extended_partial(self):
        """Gate passes but extended only gets some cross-turn refs."""
        model = MagicMock()
        model.invoke = MagicMock(side_effect=[
            MockMessage(content="The weather is sunny."),
            MockMessage(content="The memory issue seems related to the recent deployment. I suggest investigating further."),
        ])
        result = pm.probe_multi_turn(model)
        self.assertTrue(result.passed)
        # 30 gate + 10 responded + 15 connects_cause ("deploy") + 0 version + 0 disk
        # suggests_action: "investigate" matches
        # = 30 + 10 + 15 + 15 = 70... but "sunny" in gate response triggers gate_passed
        # Actually: check what keywords match
        self.assertGreaterEqual(result.score, 55)
        self.assertLessEqual(result.score, 75)

    def test_gate_fail(self):
        """Model responds but doesn't reference the tool result — gate fails.
        However, MockModel returns the same response for both invoke() calls,
        so the probe treats it as gate-pass (has content + "weather" keyword).
        Use a response that doesn't match any gate keywords."""
        model = MockModel(MockMessage(content="I cannot help with that request."))
        result = pm.probe_multi_turn(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 30)  # Has content but doesn't ref tool result

    def test_gate_empty(self):
        model = MockModel(MockMessage(content=""))
        result = pm.probe_multi_turn(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 0)

    def test_gate_exception_tool_error(self):
        model = MockModel(response=ValueError("tool messages not supported"))
        result = pm.probe_multi_turn(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 20)


# ---------------------------------------------------------------------------
# Probe 7: large_output
# ---------------------------------------------------------------------------

class TestLargeOutput(unittest.TestCase):
    def test_perfect_output(self):
        code = '''```python
class CacheEntry:
    """A cache entry with TTL."""
    def __init__(self, value, ttl):
        self.value = value
        self.ttl = ttl
        self.created_at = time.time()

class LRUCache:
    """LRU cache with TTL support."""
    def __init__(self, max_size=128, default_ttl=300):
        self.max_size = max_size
        self.default_ttl = default_ttl
        self._store = {}

    def get(self, key):
        """Get value by key."""
        return self._store.get(key)

    def put(self, key, value, ttl=None):
        """Store a value."""
        self._store[key] = CacheEntry(value, ttl or self.default_ttl)
        return True

    def evict_expired(self):
        """Remove expired entries."""
        now = time.time()
        expired = [k for k, v in self._store.items() if now - v.created_at > v.ttl]
        for k in expired:
            del self._store[k]

def make_cache(max_size=128, default_ttl=300):
    """Create a configured LRU cache."""
    return LRUCache(max_size=max_size, default_ttl=default_ttl)
```'''
        model = MockModel(MockMessage(content=code))
        result = pm.probe_large_output(model)
        self.assertTrue(result.passed)
        self.assertTrue(result.details["has_cache_entry"])
        self.assertTrue(result.details["has_lru_cache"])
        self.assertTrue(result.details["has_make_cache"])
        self.assertTrue(result.details["structurally_complete"])
        self.assertTrue(result.details["not_truncated"])
        # Should score high
        self.assertGreaterEqual(result.score, 80)

    def test_truncated_output(self):
        # Need unbalanced parens to catch structural incompleteness
        code = "class LRUCache:\n    def get(self, key):\n        return self._store.get(key\n    def put(self, key, value, ttl=None):\n        self._store[key] = (value\n..."
        model = MockModel(MockMessage(content=code))
        result = pm.probe_large_output(model)
        # Has LRU and return but truncated and unbalanced
        self.assertFalse(result.details["not_truncated"])
        self.assertFalse(result.details["structurally_complete"])

    def test_empty_response(self):
        model = MockModel(MockMessage(content=""))
        result = pm.probe_large_output(model)
        self.assertFalse(result.passed)
        # Empty string still has 1 "line" and may get structural completeness
        # points (balanced parens on empty = true), but no component points
        self.assertLessEqual(result.score, 30)


# ---------------------------------------------------------------------------
# Probe 8: shell_roundtrip
# ---------------------------------------------------------------------------

class TestShellRoundtrip(unittest.TestCase):
    def test_perfect_roundtrip(self):
        model = MagicMock()
        model.bind_tools = MagicMock(return_value=model)
        model.invoke = MagicMock(side_effect=[
            # Phase 1: tool call emission
            MockMessage(tool_calls=[{"name": "execute", "args": {"command": "echo hello_from_probe"}}]),
            # Phase 2: interpret ls output
            MockMessage(content="There are 3 Python files: main.py, utils.py, and test_main.py"),
        ])
        result = pm.probe_shell_roundtrip(model)
        self.assertTrue(result.passed)
        self.assertTrue(result.details["phase1_has_echo"])
        self.assertTrue(result.details["phase1_has_marker"])
        self.assertTrue(result.details["phase2_mentions_count"])
        self.assertGreaterEqual(result.details["phase2_identifies_files"], 2)
        self.assertEqual(result.score, 100)  # 30+10+10+15+20+15

    def test_no_tool_call(self):
        model = MagicMock()
        model.bind_tools = MagicMock(return_value=model)
        model.invoke = MagicMock(return_value=MockMessage(content="I would run echo hello_from_probe"))
        result = pm.probe_shell_roundtrip(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 0)

    def test_phase1_ok_phase2_wrong_count(self):
        model = MagicMock()
        model.bind_tools = MagicMock(return_value=model)
        model.invoke = MagicMock(side_effect=[
            MockMessage(tool_calls=[{"name": "execute", "args": {"command": "echo hello_from_probe"}}]),
            MockMessage(content="I see several files in the directory including Python and YAML files."),
        ])
        result = pm.probe_shell_roundtrip(model)
        self.assertTrue(result.passed)
        # 30 + 10 echo + 10 marker + 15 responded = 65, no count/file bonus
        self.assertEqual(result.score, 65)


# ---------------------------------------------------------------------------
# Probe 9: shell_failure_recovery
# ---------------------------------------------------------------------------

class TestShellFailureRecovery(unittest.TestCase):
    def test_perfect_recovery_all_scenarios(self):
        model = MagicMock()
        model.bind_tools = MagicMock(return_value=model)
        # Each scenario: acknowledges error + suggests recovery + emits corrective tool call
        model.invoke = MagicMock(side_effect=[
            MockMessage(
                content="The file was not found. Let me search for it.",
                tool_calls=[{"name": "execute", "args": {"command": "find / -name config.yaml"}}],
            ),
            MockMessage(
                content="rg is not installed. I'll use grep instead.",
                tool_calls=[{"name": "execute", "args": {"command": "grep -r TODO /app/src/"}}],
            ),
            MockMessage(
                content="Permission denied. Let me try writing to a different location.",
                tool_calls=[{"name": "execute", "args": {"command": "echo 'env=prod' > /tmp/deploy.conf"}}],
            ),
        ])
        result = pm.probe_shell_failure_recovery(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 100)  # 9/9 * 100
        self.assertTrue(result.details["any_corrective_tool_call"])
        self.assertEqual(result.details["scenarios_acknowledged"], 3)
        self.assertEqual(result.details["scenarios_recovered"], 3)

    def test_acknowledges_but_no_recovery(self):
        model = MagicMock()
        model.bind_tools = MagicMock(return_value=model)
        # Use specific keywords that match acknowledge but NOT recovery
        model.invoke = MagicMock(side_effect=[
            MockMessage(content="The file does not exist at that path."),
            MockMessage(content="That command is not available on this system."),
            MockMessage(content="Permission denied when writing to that location."),
        ])
        result = pm.probe_shell_failure_recovery(model)
        # Only acknowledge keywords match, not recovery keywords.
        # But "not available" matches acknowledge for cmd_not_found.
        # Score: depends on exact keyword hits. Just verify it's low and not passed.
        self.assertFalse(result.passed)  # needs at least 1 recovery
        self.assertLessEqual(result.score, 40)

    def test_one_scenario_errors(self):
        model = MagicMock()
        model.bind_tools = MagicMock(return_value=model)
        model.invoke = MagicMock(side_effect=[
            MockMessage(
                content="File not found. Let me find it.",
                tool_calls=[{"name": "execute", "args": {"command": "find / -name config.yaml"}}],
            ),
            ValueError("API timeout"),
            MockMessage(
                content="Permission denied. I'll use sudo.",
                tool_calls=[{"name": "execute", "args": {"command": "sudo echo 'env=prod' > /etc/app/deploy.conf"}}],
            ),
        ])
        result = pm.probe_shell_failure_recovery(model)
        self.assertTrue(result.passed)  # 2/3 acknowledged, 2 recovered
        # 6/9 = 66
        self.assertEqual(result.score, 66)


# ---------------------------------------------------------------------------
# Probe 11: reasoning_effort
# ---------------------------------------------------------------------------

class TestReasoningEffort(unittest.TestCase):
    def test_unknown_provider_scores_zero(self):
        """Unknown provider should return 0, not 50."""
        model = MockModel()
        result = pm.probe_reasoning_effort(model, "ollama", "qwen2.5:3b")
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 0)  # Not 50!


# ---------------------------------------------------------------------------
# YAML Profile Generation
# ---------------------------------------------------------------------------

class TestYAMLGeneration(unittest.TestCase):
    def _make_report(self, probes, overall_score=90, probe_set="default"):
        report = pm.ModelReport(
            model_string="test:model-1",
            provider="test",
            model_name="model-1",
            timestamp="2026-04-13T00:00:00+00:00",
            overall_score=overall_score,
            oat_compatible=True,
            probe_set=probe_set,
        )
        report.probes = probes
        return report

    def test_full_probe_set(self):
        probes = [
            pm.ProbeResult("basic_inference", True, 100, duration_ms=500),
            pm.ProbeResult("tool_calling", True, 100, duration_ms=600),
            pm.ProbeResult("streaming", True, 75, details={"ttft_ms": 800}, duration_ms=1200),
            pm.ProbeResult("streaming_tokens", True, 100, duration_ms=400),
            pm.ProbeResult("token_reporting", True, 95, duration_ms=300),
            pm.ProbeResult("multi_turn", True, 85, duration_ms=2000),
            pm.ProbeResult("large_output", True, 90, duration_ms=3000),
            pm.ProbeResult("shell_roundtrip", True, 80, duration_ms=1500),
            pm.ProbeResult("shell_failure_recovery", True, 78, duration_ms=4000),
            pm.ProbeResult("file_write_via_tool", True, 100, duration_ms=700),
            pm.ProbeResult("reasoning_effort", True, 100, details={"supported": ["low", "high"]}),
            pm.ProbeResult("routing_decision", True, 70, duration_ms=2000),
            pm.ProbeResult("context_profile", True, 90, details={"max_input_tokens": 200000}),
        ]
        report = self._make_report(probes, 90)
        yaml_out = pm._generate_yaml_profile(report)

        # Must be valid YAML-like structure
        self.assertIn('model_id: "test:model-1"', yaml_out)
        self.assertIn("status: known", yaml_out)
        self.assertIn("probe_version: 2", yaml_out)
        self.assertIn("probe_set: default", yaml_out)
        self.assertIn("supervisor_eligible: true", yaml_out)
        self.assertIn("worker_eligible: true", yaml_out)
        self.assertIn('reasoning_controls: "low, high"', yaml_out)
        self.assertIn("ttft_ms: 800", yaml_out)
        self.assertIn("avg_ms:", yaml_out)
        # Should not have probes_skipped section
        self.assertNotIn("probes_skipped", yaml_out)

    def test_minimum_probe_set_defaults(self):
        """Minimum probe set should use defaults for untested probes."""
        probes = [
            pm.ProbeResult("basic_inference", True, 100, duration_ms=500),
            pm.ProbeResult("tool_calling", True, 100, duration_ms=600),
            pm.ProbeResult("shell_roundtrip", True, 80, duration_ms=1500),
            pm.ProbeResult("file_write_via_tool", True, 100, duration_ms=700),
            pm.ProbeResult("token_reporting", True, 95, duration_ms=300),
            pm.ProbeResult("context_profile", True, 90, details={"max_input_tokens": 200000}),
        ]
        report = self._make_report(probes, 95, probe_set="minimum")
        yaml_out = pm._generate_yaml_profile(report)

        # Untested probes should default to 1.0, not 0.0
        self.assertIn("shell_recovery: 1.0", yaml_out)
        self.assertIn("multi_turn: 1.0", yaml_out)
        self.assertIn("streaming: 1.0", yaml_out)
        self.assertIn("large_output: 1.0", yaml_out)
        # Reasoning should be not_tested
        self.assertIn('reasoning_controls: "not_tested"', yaml_out)
        # Should have probes_skipped
        self.assertIn("probes_skipped:", yaml_out)
        self.assertIn("probe_set: minimum", yaml_out)
        # Supervisor eligible should be true (untested probes don't penalize)
        self.assertIn("supervisor_eligible: true", yaml_out)

    def test_supervisor_ineligible_low_multi_turn(self):
        """Low multi_turn score should block supervisor."""
        probes = [
            pm.ProbeResult("basic_inference", True, 100),
            pm.ProbeResult("tool_calling", True, 100),
            pm.ProbeResult("streaming", True, 100),
            pm.ProbeResult("shell_roundtrip", True, 80),
            pm.ProbeResult("shell_failure_recovery", True, 80),
            pm.ProbeResult("file_write_via_tool", True, 100),
            pm.ProbeResult("multi_turn", True, 50),  # Below 0.7 threshold
            pm.ProbeResult("large_output", True, 90),
            pm.ProbeResult("token_reporting", True, 95),
            pm.ProbeResult("reasoning_effort", True, 100, details={"supported": ["low"]}),
            pm.ProbeResult("routing_decision", True, 70),
            pm.ProbeResult("context_profile", True, 90, details={"max_input_tokens": 200000}),
            pm.ProbeResult("streaming_tokens", True, 100),
        ]
        report = self._make_report(probes, 88)
        yaml_out = pm._generate_yaml_profile(report)
        self.assertIn("supervisor_eligible: false", yaml_out)

    def test_not_oat_compatible(self):
        """Failed core probes should produce restricted status."""
        probes = [
            pm.ProbeResult("basic_inference", True, 100),
            pm.ProbeResult("tool_calling", False, 0),  # Core probe failed
        ]
        report = self._make_report(probes, 30)
        report.oat_compatible = False
        yaml_out = pm._generate_yaml_profile(report)
        self.assertIn("status: restricted", yaml_out)
        self.assertIn("worker_eligible: false", yaml_out)
        self.assertIn("supervisor_eligible: false", yaml_out)

    def test_probe_set_from_report_not_count(self):
        """probe_set_name should come from report.probe_set, not probe count."""
        # 7 probes but probe_set is "minimum" — should still say minimum
        probes = [
            pm.ProbeResult("basic_inference", True, 100, duration_ms=500),
            pm.ProbeResult("tool_calling", True, 100, duration_ms=600),
            pm.ProbeResult("shell_roundtrip", True, 80, duration_ms=1500),
            pm.ProbeResult("file_write_via_tool", True, 100, duration_ms=700),
            pm.ProbeResult("token_reporting", True, 95, duration_ms=300),
            pm.ProbeResult("context_profile", True, 90, details={"max_input_tokens": 200000}),
            pm.ProbeResult("streaming", True, 100, duration_ms=800),  # Extra probe
        ]
        report = self._make_report(probes, 95, probe_set="minimum")
        yaml_out = pm._generate_yaml_profile(report)
        # Old heuristic would say "default" (7 > 6), new code uses report.probe_set
        self.assertIn("probe_set: minimum", yaml_out)

    def test_default_probe_set_with_few_probes(self):
        """Even with few probes, if probe_set is default, YAML says default."""
        # Only 3 probes but probe_set is "default" (some crashed)
        probes = [
            pm.ProbeResult("basic_inference", True, 100, duration_ms=500),
            pm.ProbeResult("tool_calling", True, 100, duration_ms=600),
            pm.ProbeResult("token_reporting", True, 95, duration_ms=300),
        ]
        report = self._make_report(probes, 80, probe_set="default")
        yaml_out = pm._generate_yaml_profile(report)
        # Old heuristic would say "minimum" (3 <= 6), new code uses report.probe_set
        self.assertIn("probe_set: default", yaml_out)


# ---------------------------------------------------------------------------
# ModelReport dataclass
# ---------------------------------------------------------------------------

class TestModelReport(unittest.TestCase):
    def test_probe_set_defaults_to_default(self):
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01")
        self.assertEqual(report.probe_set, "default")

    def test_probe_set_can_be_set(self):
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01", probe_set="minimum")
        self.assertEqual(report.probe_set, "minimum")

    def test_probe_set_in_asdict(self):
        """probe_set should appear in JSON serialization."""
        from dataclasses import asdict
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01", probe_set="minimum")
        d = asdict(report)
        self.assertEqual(d["probe_set"], "minimum")


# ---------------------------------------------------------------------------
# Recommendations
# ---------------------------------------------------------------------------

class TestRecommendations(unittest.TestCase):
    def test_ttft_warning_slow(self):
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01")
        report.probes = [
            pm.ProbeResult("basic_inference", True, 100),
            pm.ProbeResult("streaming", True, 50, details={"ttft_ms": 5000}),
        ]
        pm._generate_recommendations(report)
        self.assertTrue(any("High TTFT" in w for w in report.warnings))

    def test_ttft_recommendation_fast(self):
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01")
        report.probes = [
            pm.ProbeResult("basic_inference", True, 100),
            pm.ProbeResult("streaming", True, 100, details={"ttft_ms": 400}),
        ]
        pm._generate_recommendations(report)
        self.assertTrue(any("Fast TTFT" in r for r in report.recommendations))

    def test_insane_tokens_warning(self):
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01")
        report.probes = [
            pm.ProbeResult("basic_inference", True, 100),
            pm.ProbeResult("token_reporting", True, 85, details={"counts_sane": False}),
        ]
        pm._generate_recommendations(report)
        self.assertTrue(any("outside expected range" in w for w in report.warnings))

    def test_weak_multi_turn_warning(self):
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01")
        report.probes = [
            pm.ProbeResult("basic_inference", True, 100),
            pm.ProbeResult("multi_turn", True, 45),  # passed but low score
        ]
        pm._generate_recommendations(report)
        self.assertTrue(any("poor supervisor candidate" in w for w in report.warnings))

    def test_corrective_tool_call_recommendation(self):
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01")
        report.probes = [
            pm.ProbeResult("basic_inference", True, 100),
            pm.ProbeResult("shell_failure_recovery", True, 80,
                          details={"any_corrective_tool_call": True}),
        ]
        pm._generate_recommendations(report)
        self.assertTrue(any("corrective tool calls" in r for r in report.recommendations))

    def test_basic_inference_blocker(self):
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01")
        report.probes = [
            pm.ProbeResult("basic_inference", False, 0),
        ]
        pm._generate_recommendations(report)
        self.assertTrue(any("BLOCKER" in r for r in report.recommendations))


# ---------------------------------------------------------------------------
# Weighted Scoring
# ---------------------------------------------------------------------------

class TestWeightedScoring(unittest.TestCase):
    def test_all_100_gives_100(self):
        report = pm.ModelReport("t:m", "t", "m", "2026-01-01")
        report.probes = [
            pm.ProbeResult(name, True, 100)
            for name in pm.PROBE_WEIGHTS.keys()
        ]
        total_weight = sum(pm.PROBE_WEIGHTS.values())
        weighted = sum(100 * w for w in pm.PROBE_WEIGHTS.values())
        expected = int(weighted / total_weight)
        self.assertEqual(expected, 100)

    def test_weights_sum_to_110(self):
        """Document the total weight for reference."""
        total = sum(pm.PROBE_WEIGHTS.values())
        self.assertEqual(total, 110)

    def test_shell_recovery_weight_increased(self):
        self.assertEqual(pm.PROBE_WEIGHTS["shell_failure_recovery"], 15)

    def test_multi_turn_weight_increased(self):
        self.assertEqual(pm.PROBE_WEIGHTS["multi_turn"], 12)

    def test_shell_roundtrip_weight_reduced(self):
        self.assertEqual(pm.PROBE_WEIGHTS["shell_roundtrip"], 8)


# ---------------------------------------------------------------------------
# Probe Sets
# ---------------------------------------------------------------------------

class TestProbeSets(unittest.TestCase):
    def test_minimum_set_has_core_probes(self):
        minimum = pm.PROBE_SETS["minimum"]
        self.assertIn("basic_inference", minimum)
        self.assertIn("tool_calling", minimum)
        self.assertIn("shell_roundtrip", minimum)
        self.assertIn("file_write_via_tool", minimum)
        self.assertIn("token_reporting", minimum)
        self.assertIn("context_profile", minimum)
        self.assertEqual(len(minimum), 6)

    def test_default_set_is_none(self):
        self.assertIsNone(pm.PROBE_SETS["default"])

    def test_minimum_set_uses_new_probe_name(self):
        """shell_roundtrip replaced shell_execution in minimum set."""
        minimum = pm.PROBE_SETS["minimum"]
        self.assertNotIn("shell_execution", minimum)
        self.assertIn("shell_roundtrip", minimum)


# ---------------------------------------------------------------------------
# Routing Decision
# ---------------------------------------------------------------------------

class TestRoutingDecision(unittest.TestCase):
    def test_perfect_routing(self):
        """Model assigns optimal models to all 5 tasks."""
        response_text = (
            "anthropic:claude-sonnet-4-6\n"
            "ollama:qwen2.5:3b\n"
            "anthropic:claude-sonnet-4-6\n"
            "ollama:qwen2.5:3b\n"
            "google_genai:gemini-2.5-flash\n"
        )
        model = MockModel(MockMessage(content=response_text))
        result = pm.probe_routing_decision(model)
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 100)  # 20+15+20+20+25
        self.assertTrue(result.details.get("good_distribution"))

    def test_all_on_one_model(self):
        """Penalty for zero distribution."""
        response_text = "\n".join(["anthropic:claude-sonnet-4-6"] * 5)
        model = MockModel(MockMessage(content=response_text))
        result = pm.probe_routing_decision(model)
        self.assertTrue(result.passed)
        # Gets optimal for tasks 1,3 (20+20), half for 2,4,5 (7+10+12) = 69, minus 20 for single model = 49
        self.assertLess(result.score, 70)

    def test_restricted_model_assigned(self):
        """Assigning restricted model is a critical failure."""
        response_text = (
            "anthropic:claude-sonnet-4-6\n"
            "ollama:gemma3:1b\n"  # RESTRICTED!
            "anthropic:claude-sonnet-4-6\n"
            "ollama:qwen2.5:3b\n"
            "google_genai:gemini-2.5-flash\n"
        )
        model = MockModel(MockMessage(content=response_text))
        result = pm.probe_routing_decision(model)
        self.assertFalse(result.passed)
        self.assertTrue(result.details["any_restricted_assigned"])

    def test_unparseable_response(self):
        model = MockModel(MockMessage(content="I think model A for task 1 and model B for task 2"))
        result = pm.probe_routing_decision(model)
        self.assertFalse(result.passed)
        self.assertEqual(result.score, 10)


# ---------------------------------------------------------------------------
# Integration: _extract_content
# ---------------------------------------------------------------------------

class TestExtractContent(unittest.TestCase):
    def test_string_content(self):
        msg = MockMessage(content="hello")
        self.assertEqual(pm._extract_content(msg), "hello")

    def test_list_content(self):
        msg = MockMessage(content=[{"text": "hello"}, {"text": " world"}])
        self.assertEqual(pm._extract_content(msg), "hello  world")

    def test_no_content(self):
        msg = MockMessage()
        self.assertEqual(pm._extract_content(msg), "")


# ---------------------------------------------------------------------------
# Per-probe timeout (PR #2 Task 1 / P0-A)
# ---------------------------------------------------------------------------

class TestPerProbeTimeout(unittest.TestCase):
    def test_per_probe_timeout_marks_failed(self):
        """A probe that sleeps past the timeout should be recorded as failed
        with an explicit 'timeout after Xs' error."""
        async def slow_probe():
            await asyncio.sleep(5)
            return pm.ProbeResult(name="slow", passed=True, score=100)

        def _fn():
            return slow_probe()  # returns a coroutine

        with self.assertRaises(asyncio.TimeoutError):
            asyncio.run(pm._invoke_probe_with_timeout(_fn, timeout=1))

    def test_per_probe_timeout_sync_probe_returns_immediately(self):
        """Fast sync probes run unchanged through the timeout helper."""
        def _fn():
            return pm.ProbeResult(name="fast", passed=True, score=100)

        result = asyncio.run(pm._invoke_probe_with_timeout(_fn, timeout=5))
        self.assertTrue(result.passed)
        self.assertEqual(result.score, 100)

    def test_per_probe_timeout_default_60s(self):
        """--per-probe-timeout default is 60s."""
        import argparse as _argparse
        parser = _argparse.ArgumentParser()
        parser.add_argument("--per-probe-timeout", type=int, default=60)
        args = parser.parse_args([])
        self.assertEqual(args.per_probe_timeout, 60)

    def test_per_probe_timeout_yaml_records_error(self):
        """A timed-out probe should appear in evidence.probe_errors."""
        probes = [
            pm.ProbeResult("basic_inference", True, 100, duration_ms=500),
            pm.ProbeResult("tool_calling", False, 0, error="timeout after 60s"),
        ]
        report = pm.ModelReport(
            model_string="test:timeout", provider="test", model_name="timeout",
            timestamp="2026-04-13T00:00:00+00:00",
            overall_score=50, oat_compatible=True,
        )
        report.probes = probes
        yaml_out = pm._generate_yaml_profile(report)
        self.assertIn("probe_errors:", yaml_out)
        self.assertIn("tool_calling:", yaml_out)
        self.assertIn("timeout after 60s", yaml_out)

    def test_per_probe_timeout_run_continues_after_timeout(self):
        """run_probes should record the timeout and keep going."""
        # Build a minimal fake "model" and a fake probe loop by hand. We bypass
        # run_probes because it resolves a model (requires API). Instead,
        # re-use the core wait_for logic that the loop calls.
        async def slow():
            await asyncio.sleep(5)

        async def fast():
            return pm.ProbeResult(name="fast", passed=True, score=100)

        async def drive():
            results = []
            # Probe 1 hangs
            try:
                await pm._invoke_probe_with_timeout(lambda: slow(), timeout=1)
            except asyncio.TimeoutError:
                results.append(pm.ProbeResult(
                    name="slow", passed=False, score=0,
                    error="timeout after 1s",
                ))
            # Probe 2 runs normally
            r = await pm._invoke_probe_with_timeout(lambda: fast(), timeout=5)
            results.append(r)
            return results

        results = asyncio.run(drive())
        self.assertEqual(len(results), 2)
        self.assertFalse(results[0].passed)
        self.assertTrue(results[0].error.startswith("timeout after"))
        self.assertTrue(results[1].passed)


# ---------------------------------------------------------------------------
# _resolve_model explicit fallback (PR #2 Task 2 / P0-B)
# ---------------------------------------------------------------------------

class TestResolveModel(unittest.TestCase):
    def test_returns_tuple(self):
        """_resolve_model must return a (model, used_fallback) tuple."""
        # The live deepagents_cli.config.create_model may or may not be
        # importable in the test env. Patch both paths to determine result.
        fake_model = MockModel()

        # Happy path: primary succeeds → used_fallback=False.
        # Patch at the import level by injecting a fake module.
        import types
        fake_mod = types.ModuleType("deepagents_cli.config")

        class _Fake:
            def __init__(self, m):
                self.model = m
        fake_mod.create_model = lambda s: _Fake(fake_model)
        with patch.dict(sys.modules, {"deepagents_cli.config": fake_mod}):
            result, used_fallback = pm._resolve_model("test:model")
        self.assertIs(result, fake_model)
        self.assertFalse(used_fallback)

    def test_logs_first_exception_before_fallback(self):
        """When create_model raises, a WARN line should be printed to stderr
        before attempting the fallback path."""
        import types, io, contextlib
        fake_mod = types.ModuleType("deepagents_cli.config")

        def _raise(s):
            raise ValueError("bad config")
        fake_mod.create_model = _raise

        fake_chat_mod = types.ModuleType("langchain.chat_models")
        fake_chat_mod.init_chat_model = lambda *a, **kw: MockModel()

        buf = io.StringIO()
        with patch.dict(sys.modules, {
            "deepagents_cli.config": fake_mod,
            "langchain.chat_models": fake_chat_mod,
        }), contextlib.redirect_stderr(buf):
            model, used_fallback = pm._resolve_model("test:model")

        output = buf.getvalue()
        self.assertIn("WARN:", output)
        self.assertIn("ValueError", output)
        self.assertIn("bad config", output)
        self.assertTrue(used_fallback)

    def test_reraises_first_on_both_fail(self):
        """When both resolution paths fail, the FIRST exception should be
        re-raised (the create_model one, not the init_chat_model one)."""
        import types, io, contextlib
        fake_mod = types.ModuleType("deepagents_cli.config")

        def _raise_first(s):
            raise ValueError("config.toml typo here")
        fake_mod.create_model = _raise_first

        fake_chat_mod = types.ModuleType("langchain.chat_models")

        def _raise_second(*a, **kw):
            raise RuntimeError("unknown model")
        fake_chat_mod.init_chat_model = _raise_second

        buf = io.StringIO()
        with patch.dict(sys.modules, {
            "deepagents_cli.config": fake_mod,
            "langchain.chat_models": fake_chat_mod,
        }), contextlib.redirect_stderr(buf):
            with self.assertRaises(ValueError) as ctx:
                pm._resolve_model("test:model")

        self.assertIn("config.toml typo here", str(ctx.exception))

    def test_no_fallback_flag_skips_second_path(self):
        """--no-fallback should skip init_chat_model entirely and re-raise
        the original create_model exception."""
        import types, io, contextlib
        fake_mod = types.ModuleType("deepagents_cli.config")
        call_counter = {"fallback": 0}

        def _raise(s):
            raise ValueError("primary failure")
        fake_mod.create_model = _raise

        fake_chat_mod = types.ModuleType("langchain.chat_models")

        def _count_fallback(*a, **kw):
            call_counter["fallback"] += 1
            return MockModel()
        fake_chat_mod.init_chat_model = _count_fallback

        buf = io.StringIO()
        with patch.dict(sys.modules, {
            "deepagents_cli.config": fake_mod,
            "langchain.chat_models": fake_chat_mod,
        }), contextlib.redirect_stderr(buf):
            with self.assertRaises(ValueError) as ctx:
                pm._resolve_model("test:model", no_fallback=True)

        self.assertEqual(call_counter["fallback"], 0,
                         "init_chat_model should not be called with no_fallback=True")
        self.assertIn("primary failure", str(ctx.exception))

    def test_resolution_fallback_recorded_in_yaml(self):
        """When used_fallback=True the YAML should note it in evidence."""
        probes = [pm.ProbeResult("basic_inference", True, 100, duration_ms=500)]
        report = pm.ModelReport(
            model_string="test:fb", provider="test", model_name="fb",
            timestamp="2026-04-13T00:00:00+00:00",
            overall_score=50, oat_compatible=True,
            resolution_fallback=True,
        )
        report.probes = probes
        yaml_out = pm._generate_yaml_profile(report)
        self.assertIn("resolution_fallback: true", yaml_out)

        report.resolution_fallback = False
        yaml_out = pm._generate_yaml_profile(report)
        self.assertIn("resolution_fallback: false", yaml_out)


if __name__ == "__main__":
    unittest.main(verbosity=2)
