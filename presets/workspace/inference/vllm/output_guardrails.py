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

"""
Output guardrails middleware for KAITO vLLM inference.

Integrates LLM Guard output scanners to filter/block potentially harmful
LLM responses (e.g., malicious URLs, raw IPs, banned topics) before they
reach the caller. Configured via the `guardrails` section in inference_config.yaml.

See: https://protectai.github.io/llm-guard/output_scanners/
"""

import logging
from dataclasses import dataclass, field
from typing import Any

logger = logging.getLogger(__name__)

# Scanner name -> LLM Guard import path mapping
_SCANNER_REGISTRY = {
    "MaliciousURLs": "llm_guard.output_scanners.MaliciousURLs",
    "Regex": "llm_guard.output_scanners.Regex",
    "BanTopics": "llm_guard.output_scanners.BanTopics",
    "Sensitive": "llm_guard.output_scanners.Sensitive",
    "Toxicity": "llm_guard.output_scanners.Toxicity",
    "Bias": "llm_guard.output_scanners.Bias",
    "NoRefusal": "llm_guard.output_scanners.NoRefusal",
    "LanguageSame": "llm_guard.output_scanners.LanguageSame",
    "Relevance": "llm_guard.output_scanners.Relevance",
}


@dataclass
class GuardrailsConfig:
    """Configuration for output guardrails."""

    enabled: bool = False
    output_scanners: list[dict[str, Any]] = field(default_factory=list)

    @staticmethod
    def from_dict(config: dict[str, Any]) -> "GuardrailsConfig":
        return GuardrailsConfig(
            enabled=config.get("enabled", False),
            output_scanners=config.get("output_scanners", []),
        )


class OutputGuardrails:
    """
    Applies LLM Guard output scanners to filter LLM responses.

    Usage:
        guardrails = OutputGuardrails.from_config(guardrails_config)
        sanitized, is_valid, results = guardrails.scan(prompt, response_text)
    """

    def __init__(self, scanners: list[Any]):
        self.scanners = scanners

    @classmethod
    def from_config(cls, config: GuardrailsConfig) -> "OutputGuardrails":
        """Initialize guardrails from config. Returns a no-op instance if disabled."""
        if not config.enabled:
            logger.info("Output guardrails are disabled.")
            return cls(scanners=[])

        scanners = []
        for scanner_config in config.output_scanners:
            scanner_name = scanner_config.get("name")
            if not scanner_name:
                logger.warning("Skipping scanner config with no name: %s", scanner_config)
                continue

            scanner = _create_scanner(scanner_name, scanner_config)
            if scanner is not None:
                scanners.append((scanner_name, scanner))
                logger.info("Loaded output scanner: %s", scanner_name)

        logger.info("Output guardrails enabled with %d scanner(s).", len(scanners))
        return cls(scanners=scanners)

    @property
    def enabled(self) -> bool:
        return len(self.scanners) > 0

    def scan(self, prompt: str, output: str) -> tuple[str, bool, list[dict[str, Any]]]:
        """
        Scan the LLM output through all configured scanners.

        Args:
            prompt: The original user prompt.
            output: The LLM-generated output text.

        Returns:
            A tuple of (sanitized_output, is_valid, scan_results).
            - sanitized_output: The output after scanner processing (may be modified).
            - is_valid: True if the output passed all scanners.
            - scan_results: List of per-scanner results for logging/observability.
        """
        if not self.scanners:
            return output, True, []

        sanitized = output
        is_valid = True
        scan_results = []

        for scanner_name, scanner in self.scanners:
            try:
                sanitized, valid, risk_score = scanner.scan(prompt, sanitized)
                result = {
                    "scanner": scanner_name,
                    "valid": valid,
                    "risk_score": risk_score,
                }
                scan_results.append(result)

                if not valid:
                    is_valid = False
                    logger.warning(
                        "Output blocked by scanner '%s' (risk_score=%.3f)",
                        scanner_name,
                        risk_score,
                    )
            except Exception as e:
                logger.error("Scanner '%s' failed: %s", scanner_name, e)
                scan_results.append({
                    "scanner": scanner_name,
                    "valid": True,
                    "error": str(e),
                })

        return sanitized, is_valid, scan_results


def _create_scanner(name: str, config: dict[str, Any]) -> Any:
    """Create an LLM Guard output scanner instance from config."""
    import_path = _SCANNER_REGISTRY.get(name)
    if import_path is None:
        logger.warning("Unknown scanner '%s'. Available: %s", name, list(_SCANNER_REGISTRY.keys()))
        return None

    try:
        module_path, class_name = import_path.rsplit(".", 1)
        import importlib
        module = importlib.import_module(module_path)
        scanner_cls = getattr(module, class_name)
    except (ImportError, AttributeError) as e:
        logger.error(
            "Failed to import scanner '%s' from '%s': %s. "
            "Make sure llm-guard is installed: pip install llm-guard",
            name, import_path, e,
        )
        return None

    # Build kwargs from config, excluding 'name'
    kwargs = {k: v for k, v in config.items() if k != "name"}

    try:
        return scanner_cls(**kwargs)
    except Exception as e:
        logger.error("Failed to initialize scanner '%s' with config %s: %s", name, kwargs, e)
        return None
