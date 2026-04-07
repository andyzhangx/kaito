# Copyright (c) KAITO authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import asyncio
import json
import unittest
from unittest.mock import MagicMock, patch

from output_guardrails import (
    GuardrailsConfig,
    OutputGuardrails,
    _extract_text_delta,
    _make_error_sse_event,
)


class TestGuardrailsConfig(unittest.TestCase):
    def test_from_dict_disabled(self):
        config = GuardrailsConfig.from_dict({})
        self.assertFalse(config.enabled)
        self.assertEqual(config.output_scanners, [])

    def test_from_dict_enabled(self):
        config = GuardrailsConfig.from_dict({
            "enabled": True,
            "output_scanners": [
                {"name": "MaliciousURLs", "threshold": 0.5},
                {"name": "Regex", "patterns": [r"\d+\.\d+\.\d+\.\d+"]},
            ],
        })
        self.assertTrue(config.enabled)
        self.assertEqual(len(config.output_scanners), 2)

    def test_from_dict_custom_stream_scan_interval(self):
        config = GuardrailsConfig.from_dict({
            "enabled": True,
            "stream_scan_interval": 500,
            "output_scanners": [],
        })
        self.assertEqual(config.stream_scan_interval, 500)

    def test_from_dict_default_stream_scan_interval(self):
        config = GuardrailsConfig.from_dict({"enabled": True})
        self.assertEqual(config.stream_scan_interval, 200)


class TestOutputGuardrails(unittest.TestCase):
    def test_disabled_returns_noop(self):
        config = GuardrailsConfig(enabled=False, output_scanners=[])
        guardrails = OutputGuardrails.from_config(config)
        self.assertFalse(guardrails.enabled)

        output, is_valid, results = guardrails.scan("prompt", "response text")
        self.assertEqual(output, "response text")
        self.assertTrue(is_valid)
        self.assertEqual(results, [])

    def test_scan_with_mock_scanner(self):
        mock_scanner = MagicMock()
        mock_scanner.scan.return_value = ("sanitized output", True, 0.1)

        guardrails = OutputGuardrails(scanners=[("MockScanner", mock_scanner)])
        self.assertTrue(guardrails.enabled)

        output, is_valid, results = guardrails.scan("prompt", "response text")
        self.assertEqual(output, "sanitized output")
        self.assertTrue(is_valid)
        self.assertEqual(len(results), 1)
        self.assertEqual(results[0]["scanner"], "MockScanner")
        self.assertTrue(results[0]["valid"])
        mock_scanner.scan.assert_called_once_with("prompt", "response text")

    def test_scan_blocks_invalid_output(self):
        mock_scanner = MagicMock()
        mock_scanner.scan.return_value = ("", False, 0.9)

        guardrails = OutputGuardrails(scanners=[("BlockingScanner", mock_scanner)])

        output, is_valid, results = guardrails.scan("prompt", "visit http://evil.com")
        self.assertFalse(is_valid)
        self.assertEqual(results[0]["risk_score"], 0.9)

    def test_scan_multiple_scanners_short_circuits(self):
        """If one scanner blocks, is_valid is False even if others pass."""
        passing_scanner = MagicMock()
        passing_scanner.scan.return_value = ("clean text", True, 0.1)

        blocking_scanner = MagicMock()
        blocking_scanner.scan.return_value = ("", False, 0.8)

        guardrails = OutputGuardrails(scanners=[
            ("PassingScanner", passing_scanner),
            ("BlockingScanner", blocking_scanner),
        ])

        output, is_valid, results = guardrails.scan("prompt", "some text")
        self.assertFalse(is_valid)
        self.assertEqual(len(results), 2)
        self.assertTrue(results[0]["valid"])
        self.assertFalse(results[1]["valid"])

    def test_scan_handles_scanner_exception(self):
        failing_scanner = MagicMock()
        failing_scanner.scan.side_effect = RuntimeError("scanner crashed")

        guardrails = OutputGuardrails(scanners=[("FailingScanner", failing_scanner)])

        output, is_valid, results = guardrails.scan("prompt", "some text")
        # Scanner failure should not block the response
        self.assertEqual(output, "some text")
        self.assertTrue(is_valid)
        self.assertEqual(len(results), 1)
        self.assertIn("error", results[0])

    def test_unknown_scanner_skipped(self):
        config = GuardrailsConfig(
            enabled=True,
            output_scanners=[{"name": "NonExistentScanner"}],
        )
        guardrails = OutputGuardrails.from_config(config)
        self.assertFalse(guardrails.enabled)  # No valid scanners loaded


class TestExtractTextDelta(unittest.TestCase):
    def test_chat_completion_delta(self):
        data = {"choices": [{"delta": {"content": "hello"}}]}
        chunk = f"data: {json.dumps(data)}\n\n".encode()
        self.assertEqual(_extract_text_delta(chunk), "hello")

    def test_completion_text(self):
        data = {"choices": [{"text": "world"}]}
        chunk = f"data: {json.dumps(data)}\n\n".encode()
        self.assertEqual(_extract_text_delta(chunk), "world")

    def test_done_event(self):
        chunk = b"data: [DONE]\n\n"
        self.assertEqual(_extract_text_delta(chunk), "")

    def test_empty_delta(self):
        data = {"choices": [{"delta": {}}]}
        chunk = f"data: {json.dumps(data)}\n\n".encode()
        self.assertEqual(_extract_text_delta(chunk), "")

    def test_non_data_line(self):
        chunk = b": keep-alive\n\n"
        self.assertEqual(_extract_text_delta(chunk), "")

    def test_malformed_json(self):
        chunk = b"data: not-json\n\n"
        self.assertEqual(_extract_text_delta(chunk), "")


class TestMakeErrorSSEEvent(unittest.TestCase):
    def test_error_event_format(self):
        event = _make_error_sse_event("blocked")
        decoded = event.decode("utf-8")
        self.assertTrue(decoded.startswith("data: "))
        data = json.loads(decoded.strip()[6:])
        self.assertEqual(data["error"]["message"], "blocked")
        self.assertEqual(data["error"]["type"], "guardrails_violation")
        self.assertEqual(data["error"]["code"], "content_blocked")


class TestStreamingGuardrails(unittest.TestCase):
    """Test the scan_streaming method."""

    def _run_async(self, coro):
        return asyncio.get_event_loop().run_until_complete(coro)

    def _make_sse_chunk(self, text, is_chat=True):
        if is_chat:
            data = {"choices": [{"delta": {"content": text}}]}
        else:
            data = {"choices": [{"text": text}]}
        return f"data: {json.dumps(data)}\n\n".encode()

    async def _collect_stream(self, guardrails, prompt, chunks):
        async def gen():
            for c in chunks:
                yield c

        result = []
        async for event in guardrails.scan_streaming(prompt, gen()):
            result.append(event)
        return result

    def test_no_scanners_passthrough(self):
        guardrails = OutputGuardrails(scanners=[])
        chunks = [self._make_sse_chunk("hello"), self._make_sse_chunk(" world")]

        result = self._run_async(self._collect_stream(guardrails, "prompt", chunks))
        self.assertEqual(result, chunks)

    def test_streaming_clean_output(self):
        mock_scanner = MagicMock()
        mock_scanner.scan.return_value = ("accumulated text", True, 0.05)

        guardrails = OutputGuardrails(
            scanners=[("Clean", mock_scanner)],
            stream_scan_interval=5,  # Scan every 5 chars
        )

        chunks = [
            self._make_sse_chunk("hello"),      # 5 chars -> triggers scan
            self._make_sse_chunk(" world"),      # 6 more -> triggers scan
            b"data: [DONE]\n\n",
        ]

        result = self._run_async(self._collect_stream(guardrails, "prompt", chunks))
        # All chunks should be yielded (clean output)
        self.assertEqual(len(result), 3)
        self.assertTrue(mock_scanner.scan.called)

    def test_streaming_blocked_output(self):
        mock_scanner = MagicMock()
        # First scan passes, second scan blocks
        mock_scanner.scan.side_effect = [
            ("hello", True, 0.1),
            ("", False, 0.9),
        ]

        guardrails = OutputGuardrails(
            scanners=[("Blocker", mock_scanner)],
            stream_scan_interval=5,
        )

        chunks = [
            self._make_sse_chunk("hello"),      # triggers scan -> pass
            self._make_sse_chunk(" evil"),       # triggers scan -> block
            self._make_sse_chunk(" more"),       # should not be yielded
        ]

        result = self._run_async(self._collect_stream(guardrails, "prompt", chunks))

        # Should have: first chunk (flushed after pass) + error event + [DONE]
        result_decoded = [r.decode("utf-8") for r in result]

        # Check that error event is present
        has_error = any("guardrails_violation" in r for r in result_decoded)
        self.assertTrue(has_error)

        # Check that [DONE] is present
        has_done = any("[DONE]" in r for r in result_decoded)
        self.assertTrue(has_done)

    def test_streaming_final_scan_blocks(self):
        """Content passes incremental scans but fails the final full-text scan."""
        mock_scanner = MagicMock()
        # Incremental scans pass, final scan blocks
        mock_scanner.scan.side_effect = [
            ("partial", True, 0.1),  # incremental
            ("", False, 0.9),         # final
        ]

        guardrails = OutputGuardrails(
            scanners=[("FinalBlocker", mock_scanner)],
            stream_scan_interval=5,
        )

        chunks = [
            self._make_sse_chunk("hello"),      # triggers scan -> pass
            self._make_sse_chunk("!"),           # not enough for scan
            b"data: [DONE]\n\n",
        ]

        result = self._run_async(self._collect_stream(guardrails, "prompt", chunks))
        result_decoded = [r.decode("utf-8") for r in result]

        # First chunk was flushed, then final scan blocks
        has_error = any("guardrails_violation" in r for r in result_decoded)
        self.assertTrue(has_error)


if __name__ == "__main__":
    unittest.main()
