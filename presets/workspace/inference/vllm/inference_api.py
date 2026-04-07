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

import argparse
import json
import logging
import os
from dataclasses import dataclass
from typing import Any

import psutil
import pynvml
import uvloop
import vllm.entrypoints.openai.api_server as api_server
import yaml
from vllm.entrypoints.openai.models.protocol import LoRAModulePath
from vllm.utils.argparse_utils import FlexibleArgumentParser

from starlette.requests import Request
from starlette.responses import JSONResponse, StreamingResponse

from output_guardrails import GuardrailsConfig, OutputGuardrails

# Initialize logger
logger = logging.getLogger(__name__)
debug_mode = os.environ.get("DEBUG_MODE", "false").lower() == "true"
logging.basicConfig(
    level=logging.DEBUG if debug_mode else logging.INFO,
    format="%(levelname)s %(asctime)s %(filename)s:%(lineno)d] %(message)s",
    datefmt="%m-%d %H:%M:%S",
)


class KAITOArgumentParser(argparse.ArgumentParser):
    vllm_parser = FlexibleArgumentParser(description="vLLM serving server")

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)

        # Initialize vllm parser
        self.vllm_parser = api_server.make_arg_parser(self.vllm_parser)
        self._reset_vllm_defaults()

        # KAITO only args
        # They should start with "kaito-" prefix to avoid conflict with vllm args
        self.add_argument(
            "--kaito-adapters-dir",
            type=str,
            default="/mnt/adapter",
            help="Directory where adapters are stored in KAITO preset.",
        )
        self.add_argument(
            "--kaito-config-file",
            type=str,
            default="",
            help="Additional args for KAITO preset.",
        )
        self.add_argument(
            "--kaito-max-probe-steps",
            type=int,
            help="Maximum number of steps to find the max available seq len fitting in the GPU memory.",
        )
        self.add_argument(
            "--kaito-kv-cache-cpu-memory-utilization",
            type=float,
            help="KV cache CPU memory utilization.",
        )

    def _reset_vllm_defaults(self):
        local_rank = int(os.environ.get("LOCAL_RANK", 0))  # Default to 0 if not set
        port = 5000 + local_rank  # Adjust port based on local rank

        server_default_args = {
            "disable_frontend_multiprocessing": False,
            "port": port,
        }
        self.vllm_parser.set_defaults(**server_default_args)

        # See https://docs.vllm.ai/en/stable/serving/engine_args.html for more args
        engine_default_args = {
            "model": "/workspace/vllm/weights",
            "cpu_offload_gb": 0,
            "gpu_memory_utilization": get_max_gpu_memory_utilization(),
            "swap_space": 4,
            "disable_log_stats": False,
            "uvicorn_log_level": "error",
        }
        self.vllm_parser.set_defaults(**engine_default_args)

    def parse_args(self, *args, **kwargs):
        args = super().parse_known_args(*args, **kwargs)
        kaito_args = args[0]
        runtime_args = args[1]  # Remaining args

        # Load KAITO config
        if kaito_args.kaito_config_file:
            file_config = KaitoConfig.from_yaml(kaito_args.kaito_config_file)
            if kaito_args.kaito_max_probe_steps is None:
                kaito_args.kaito_max_probe_steps = file_config.max_probe_steps
            if kaito_args.kaito_kv_cache_cpu_memory_utilization is None:
                kaito_args.kaito_kv_cache_cpu_memory_utilization = (
                    file_config.kv_cache_cpu_memory_utilization
                )
            # Store guardrails config for later initialization
            kaito_args._kaito_guardrails_config = file_config.guardrails

            for key, value in file_config.vllm.items():
                runtime_args.append(f"--{key}")
                runtime_args.append(str(value))

        vllm_args = self.vllm_parser.parse_args(runtime_args, **kwargs)
        # Merge KAITO and vLLM args
        return argparse.Namespace(**vars(kaito_args), **vars(vllm_args))

    def print_help(self, file=None):
        super().print_help(file)
        print("\norignal vLLM server arguments:\n")
        self.vllm_parser.print_help(file)


@dataclass
class KaitoConfig:
    # Extra arguments for the vllm serving server, will be forwarded to the vllm server.
    # This should be in key-value format.
    vllm: dict[str, Any]

    # Maximum number of steps to find the max available seq len fitting in the GPU memory.
    max_probe_steps: int

    # Optional: CPU memory utilization for the vllm engine in kv cache offload mode. (default: 0.5, set to 0 to disable)
    kv_cache_cpu_memory_utilization: float

    # Output guardrails configuration
    guardrails: GuardrailsConfig

    @staticmethod
    def from_yaml(yaml_file: str) -> "KaitoConfig":
        with open(yaml_file) as file:
            config_data = yaml.safe_load(file)
        guardrails_data = config_data.get("guardrails", {})
        return KaitoConfig(
            vllm=config_data.get("vllm", {}),
            max_probe_steps=config_data.get("max_probe_steps", 6),
            kv_cache_cpu_memory_utilization=config_data.get(
                "kv_cache_cpu_memory_utilization", 0.5
            ),
            guardrails=GuardrailsConfig.from_dict(guardrails_data),
        )

    def to_yaml(self) -> str:
        return yaml.dump(self.__dict__)


def load_lora_adapters(adapters_dir: str) -> LoRAModulePath | None:
    lora_list: list[LoRAModulePath] = []

    if not os.path.exists(adapters_dir):
        return lora_list

    logger.info(f"Loading LoRA adapters from {adapters_dir}")
    for adapter in os.listdir(adapters_dir):
        adapter_path = os.path.join(adapters_dir, adapter)
        if os.path.isdir(adapter_path):
            lora_list.append(LoRAModulePath(adapter, adapter_path))

    return lora_list


def get_max_gpu_memory_utilization(device_index: int = 0) -> float:
    # Calculate gpu_memory_utilization based on available GPU memory.
    # This ensures vLLM only uses currently free memory to avoid OOM errors.
    # See https://github.com/kaito-project/kaito/issues/1374.
    pynvml.nvmlInit()
    handle = pynvml.nvmlDeviceGetHandleByIndex(device_index)
    info = pynvml.nvmlDeviceGetMemoryInfo(handle)
    pynvml.nvmlShutdown()

    # Reserve an additional 600MiB for pytorch memory fragments, calculated based on profiling
    free_memory = info.free - 600 * 1024**2

    # Floor to 2 decimal places
    gpu_memory_utilization = (free_memory * 100 // info.total) / 100

    # The value is capped at 0.95 to maintain compatibility with previous behavior
    gpu_memory_utilization = min(0.95, gpu_memory_utilization)

    logger.info(f"Set default gpu_memory_utilization to {gpu_memory_utilization}")
    return gpu_memory_utilization


def set_kv_cache_offloading_if_appliable(args: argparse.Namespace) -> None:
    """
    Set KV cache offloading to CPU RAM if applicable.
    This is only applicable when kaito_kv_cache_cpu_memory_utilization is set.
    """
    if (
        args.kaito_kv_cache_cpu_memory_utilization is None
        or args.kaito_kv_cache_cpu_memory_utilization <= 0
    ):
        logger.info(
            "kv_cache_cpu_memory_utilization is not set, do not use KV cache offload to CPU RAM."
        )
        return

    os.environ["LMCACHE_CHUNK_SIZE"] = "256"
    os.environ["LMCACHE_LOCAL_CPU"] = "True"
    available_memory_gb = (
        psutil.virtual_memory().total - psutil.virtual_memory().used
    ) / (1024**3)
    logger.info(
        f"Offload KV cache to CPU RAM, size limit: {available_memory_gb} * {args.kaito_kv_cache_cpu_memory_utilization} GB split among {args.tensor_parallel_size} GPUs"
    )

    # When using tensor parallelism, the KV cache CPU memory allocation must be divided evenly
    # across all GPUs. Each GPU should only allocate its portion (1/tensor_parallel_size) of the
    # total available CPU memory to prevent OOM.
    os.environ["LMCACHE_MAX_LOCAL_CPU_SIZE"] = (
        f"{available_memory_gb * args.kaito_kv_cache_cpu_memory_utilization / args.tensor_parallel_size}"
    )

    if args.kv_transfer_config is None:
        args.kv_transfer_config = {
            "kv_connector": "LMCacheConnectorV1",
            "kv_role": "kv_both",
        }


if __name__ == "__main__":
    parser = KAITOArgumentParser(description="KAITO wrapper of vLLM serving server")
    args = parser.parse_args()

    # set LoRA adapters
    if args.lora_modules is None:
        args.lora_modules = load_lora_adapters(args.kaito_adapters_dir)

    set_kv_cache_offloading_if_appliable(args)

    # Initialize output guardrails if configured
    guardrails = None
    if hasattr(args, "_kaito_guardrails_config"):
        guardrails = OutputGuardrails.from_config(args._kaito_guardrails_config)
        if guardrails.enabled:
            logger.info("Output guardrails initialized and active.")

    # Run the serving server
    logger.info(f"Starting server on port {args.port}")
    # See https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html for more
    # details about serving server.

    async def _run_with_guardrails():
        """Start vLLM server and inject guardrails middleware if enabled."""
        async with api_server.build_async_engine_client(args) as engine_client:
            app = api_server.build_app(args)

            if guardrails and guardrails.enabled:
                app = _add_guardrails_middleware(app, guardrails)
                logger.info("Guardrails middleware injected into vLLM app.")

            await api_server.run_server(args, app=app)

    uvloop.run(_run_with_guardrails())


def _add_guardrails_middleware(app, guardrails: OutputGuardrails):
    """
    Add guardrails scanning middleware to the FastAPI app.

    Intercepts /v1/chat/completions and /v1/completions responses:
    - Non-streaming: scans complete response, returns 400 if blocked.
    - Streaming: wraps SSE generator with incremental scanning,
      terminates stream with error event if blocked.
    """
    from starlette.middleware.base import BaseHTTPMiddleware

    GUARDED_PATHS = {"/v1/chat/completions", "/v1/completions"}

    class GuardrailsMiddleware(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next):
            if request.url.path not in GUARDED_PATHS:
                return await call_next(request)

            # Parse the request body to extract prompt for scanner context
            try:
                body = await request.body()
                request_data = json.loads(body)
            except (json.JSONDecodeError, Exception):
                request_data = {}

            prompt = _extract_prompt(request_data)
            is_streaming = request_data.get("stream", False)

            response = await call_next(request)

            # Only process successful responses
            if response.status_code != 200:
                return response

            if is_streaming:
                return StreamingResponse(
                    guardrails.scan_streaming(prompt, response.body_iterator),
                    media_type="text/event-stream",
                    headers=dict(response.headers),
                )
            else:
                # Non-streaming: read full body, scan, return
                body_bytes = b""
                async for chunk in response.body_iterator:
                    if isinstance(chunk, str):
                        body_bytes += chunk.encode("utf-8")
                    else:
                        body_bytes += chunk

                try:
                    response_data = json.loads(body_bytes)
                    output_text = _extract_output_text(response_data)
                except (json.JSONDecodeError, Exception):
                    # Can't parse — pass through
                    return JSONResponse(
                        content=json.loads(body_bytes) if body_bytes else {},
                        status_code=response.status_code,
                        headers=dict(response.headers),
                    )

                if output_text:
                    sanitized, is_valid, results = guardrails.scan(prompt, output_text)

                    if not is_valid:
                        logger.warning(
                            "Non-streaming output blocked by guardrails. Results: %s",
                            results,
                        )
                        return JSONResponse(
                            status_code=400,
                            content={
                                "error": {
                                    "message": "Response blocked by output guardrails.",
                                    "type": "guardrails_violation",
                                    "code": "content_blocked",
                                    "scan_results": results,
                                }
                            },
                        )

                    # If sanitized text differs, update response
                    if sanitized != output_text:
                        _replace_output_text(response_data, sanitized)
                        body_bytes = json.dumps(response_data).encode("utf-8")

                return JSONResponse(
                    content=response_data if isinstance(response_data, dict) else json.loads(body_bytes),
                    status_code=response.status_code,
                )

    app.add_middleware(GuardrailsMiddleware)
    return app


def _extract_prompt(request_data: dict) -> str:
    """Extract the user prompt from the request for scanner context."""
    # Chat completions format
    messages = request_data.get("messages", [])
    if messages:
        # Use the last user message as the prompt
        for msg in reversed(messages):
            if msg.get("role") == "user":
                content = msg.get("content", "")
                if isinstance(content, str):
                    return content
                # Handle list content (multimodal)
                if isinstance(content, list):
                    return " ".join(
                        item.get("text", "")
                        for item in content
                        if item.get("type") == "text"
                    )
        return ""
    # Completions format
    return request_data.get("prompt", "")


def _extract_output_text(response_data: dict) -> str:
    """Extract the generated text from a non-streaming response."""
    choices = response_data.get("choices", [])
    if not choices:
        return ""
    choice = choices[0]
    # Chat completions
    message = choice.get("message", {})
    if "content" in message and message["content"]:
        return message["content"]
    # Completions
    if "text" in choice and choice["text"]:
        return choice["text"]
    return ""


def _replace_output_text(response_data: dict, new_text: str) -> None:
    """Replace the generated text in a non-streaming response with sanitized text."""
    choices = response_data.get("choices", [])
    if not choices:
        return
    choice = choices[0]
    if "message" in choice and "content" in choice["message"]:
        choice["message"]["content"] = new_text
    elif "text" in choice:
        choice["text"] = new_text
