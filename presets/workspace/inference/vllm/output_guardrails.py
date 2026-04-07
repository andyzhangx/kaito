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

For non-streaming requests, the full response is scanned before returning.
For streaming (SSE) requests, tokens are buffered and scanned incrementally;
if a scanner rejects the output, the stream is terminated with an error event.

See: https://protectai.github.io/llm-guard/output_scanners/
"""

import json
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

# Default: scan every N characters accumulated (balances latency vs safety)
_DEFAULT_STREAM_SCAN_INTERVAL = 200


@dataclass
class GuardrailsConfig:
    """Configuration for output guardrails."""

    enabled: bool = False
    output_scanners: list[dict[str, Any]] = field(default_factory=list)
    # How often to scan during streaming (in characters accumulated)
    stream_scan_interval: int = _DEFAULT_STREAM_SCAN_INTERVAL

    @staticmethod
    def from_dict(config: dict[str, Any]) -> "GuardrailsConfig":
        return GuardrailsConfig(
            enabled=config.get("enabled", False),
            output_scanners=config.get("output_scanners", []),
            stream_scan_interval=config.get(
                "stream_scan_interval", _DEFAULT_STREAM_SCAN_INTERVAL
            ),
        )


class OutputGuardrails:
    """
    Applies LLM Guard output scanners to filter LLM responses.

    Usage:
        guardrails = OutputGuardrails.from_config(guardrails_config)
        # Non-streaming
        sanitized, is_valid, results = guardrails.scan(prompt, response_text)
        # Streaming
        async for event in guardrails.scan_streaming(prompt, original_sse_generator):
            yield event
    """

    def __init__(self, scanners: list[Any], stream_scan_interval: int = _DEFAULT_STREAM_SCAN_INTERVAL):
        self.scanners = scanners
        self.stream_scan_interval = stream_scan_interval

    @classmethod
    def from_config(cls, config: GuardrailsConfig) -> "OutputGuardrails":
        """Initialize guardrails from config. Returns a no-op instance if disabled."""
        if not config.enabled:
            logger.info("Output guardrails are disabled.")
            return cls(scanners=[], stream_scan_interval=config.stream_scan_interval)

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
        return cls(scanners=scanners, stream_scan_interval=config.stream_scan_interval)

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

    async def scan_streaming(self, prompt: str, sse_generator):
        """
        Wrap a vLLM SSE streaming generator with guardrails scanning.

        Strategy: buffer SSE chunks and accumulate the generated text.
        Periodically (every stream_scan_interval chars), run scanners on
        the accumulated text. If blocked, yield an error SSE event and stop.
        At stream end, do a final full-text scan.

        Buffered chunks are held until the next scan checkpoint passes,
        then flushed to the client. This adds latency equal to
        ~stream_scan_interval characters of generation, but ensures no
        harmful content is sent before scanning.

        Args:
            prompt: The original user prompt (for scanner context).
            sse_generator: The original async generator of SSE byte lines
                           from vLLM (each item is a bytes line like
                           b'data: {...}\\n\\n').

        Yields:
            SSE byte lines — either the original chunks (if clean) or an
            error event followed by stream termination.
        """
        if not self.scanners:
            # No scanners — pass through untouched
            async for chunk in sse_generator:
                yield chunk
            return

        accumulated_text = ""
        last_scanned_len = 0
        buffered_chunks: list[bytes] = []
        blocked = False

        async for chunk in sse_generator:
            if blocked:
                break

            buffered_chunks.append(chunk)

            # Extract text delta from SSE data line
            delta = _extract_text_delta(chunk)
            if delta:
                accumulated_text += delta

            # Check if we should scan (enough new text accumulated)
            new_text_len = len(accumulated_text) - last_scanned_len
            if new_text_len >= self.stream_scan_interval:
                _, is_valid, results = self.scan(prompt, accumulated_text)
                last_scanned_len = len(accumulated_text)

                if not is_valid:
                    blocked = True
                    logger.warning(
                        "Streaming output blocked by guardrails at %d chars. "
                        "Results: %s",
                        len(accumulated_text),
                        results,
                    )
                    # Yield error event to client
                    yield _make_error_sse_event(
                        "Response blocked by output guardrails."
                    )
                    yield b"data: [DONE]\n\n"
                    return

                # Scan passed — flush buffered chunks to client
                for buffered in buffered_chunks:
                    yield buffered
                buffered_chunks.clear()

        if blocked:
            return

        # Final full-text scan on complete response
        if accumulated_text:
            _, is_valid, results = self.scan(prompt, accumulated_text)
            if not is_valid:
                logger.warning(
                    "Streaming output blocked by guardrails at final scan. "
                    "Results: %s",
                    results,
                )
                yield _make_error_sse_event(
                    "Response blocked by output guardrails."
                )
                yield b"data: [DONE]\n\n"
                return

        # All clean — flush remaining buffered chunks
        for buffered in buffered_chunks:
            yield buffered


def _extract_text_delta(sse_chunk: bytes) -> str:
    """
    Extract the text content delta from an SSE data line.

    vLLM SSE format:
      data: {"id":"...","choices":[{"delta":{"content":"hello"},...}],...}

    For /v1/completions:
      data: {"id":"...","choices":[{"text":"hello",...}],...}

    Returns the extracted text or empty string.
    """
    try:
        line = sse_chunk.decode("utf-8", errors="replace").strip()
        if not line.startswith("data: "):
            return ""
        data_str = line[6:]  # Strip "data: " prefix
        if data_str == "[DONE]":
            return ""
        data = json.loads(data_str)
        choices = data.get("choices", [])
        if not choices:
            return ""
        choice = choices[0]
        # Chat completions streaming format
        delta = choice.get("delta", {})
        if "content" in delta and delta["content"]:
            return delta["content"]
        # Completions streaming format
        if "text" in choice and choice["text"]:
            return choice["text"]
    except (json.JSONDecodeError, KeyError, IndexError, UnicodeDecodeError):
        pass
    return ""


def _make_error_sse_event(message: str) -> bytes:
    """Create an SSE error event in OpenAI-compatible format."""
    error_data = {
        "error": {
            "message": message,
            "type": "guardrails_violation",
            "code": "content_blocked",
        }
    }
    return f"data: {json.dumps(error_data)}\n\n".encode("utf-8")


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
