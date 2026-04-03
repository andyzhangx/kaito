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

import unittest
from unittest.mock import MagicMock, patch

from output_guardrails import GuardrailsConfig, OutputGuardrails


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


if __name__ == "__main__":
    unittest.main()
