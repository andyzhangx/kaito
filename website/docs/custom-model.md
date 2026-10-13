---
title: Custom Model Integration
---

## Using the KAITO base image

The KAITO base image includes both HuggingFace and vLLM runtime libraries along with corresponding FastAPI server scripts. This provides a convenient way to run **any** HuggingFace model using the **HuggingFace** runtime.

Note that the vLLM runtime is not generally productized for arbitrary custom model deployment. A narrow CPU-only vLLM proof of concept is documented below for experimentation.

Here is a **[sample deployment YAML](https://github.com/kaito-project/kaito/tree/main/examples/custom-model-integration/custom-model-deployment.yaml)**. To use it:
1. Specify the HuggingFace model ID in the container command.
2. For models that require a HuggingFace token to download, users need to add the token to the specified secret.

The script downloads model weights during server bootstrap, eliminating the need to pre-bake them into the container image.

## CPU-only POC

For a minimal CPU-only proof of concept, KAITO can run a **custom template Workspace** on **BYO CPU nodes** using a dedicated CPU image and the HuggingFace runtime. This path intentionally avoids presets, GPU estimation, and node auto-provisioning.

Artifacts in this repo:

- Transformers CPU image Dockerfile: `docker/presets/models/tfs-cpu/Dockerfile`
- Transformers CPU requirements: `presets/workspace/dependencies/requirements-cpu.txt`
- Transformers CPU example Workspace: [`examples/custom-model-integration/custom-model-deployment-cpu.yaml`](https://github.com/kaito-project/kaito/tree/main/examples/custom-model-integration/custom-model-deployment-cpu.yaml)
- vLLM CPU POC image Dockerfile: `docker/presets/models/vllm-cpu/Dockerfile`
- vLLM CPU POC example Workspace: [`examples/custom-model-integration/custom-model-deployment-vllm-cpu.yaml`](https://github.com/kaito-project/kaito/tree/main/examples/custom-model-integration/custom-model-deployment-vllm-cpu.yaml)

Build the CPU images:

```bash
make docker-build-kaito-base-cpu REGISTRY=<your-registry> OUTPUT_TYPE=type=docker
make docker-build-kaito-vllm-cpu REGISTRY=<your-registry> OUTPUT_TYPE=type=docker
```

Key requirements for the POC:

1. Run KAITO in **BYO mode** (`disableNodeAutoProvisioning=true`).
2. Label at least one existing CPU node so the Workspace can target it, for example:

   ```bash
   kubectl label node <cpu-node-name> kaito.sh/cpu-inference=true
   ```

3. Use `metadata.annotations.kaito.sh/runtime: transformers` so KAITO treats the Workspace as a non-vLLM custom model path.

This path is intentionally narrow: it is only meant to prove KAITO can launch a CPU-served model, not to provide full CPU preset parity.

## Experimental vLLM CPU POC

A second narrow proof of concept can use a template-based Workspace together with a CPU-oriented vLLM image.

This POC is intentionally constrained:

- it still requires **BYO CPU nodes**
- it still uses **custom template Workspace** only
- it is intended for **single-node experimentation**
- it should be treated as an **experimental runtime path**, not the default CPU recommendation

The example manifest is:

- [`examples/custom-model-integration/custom-model-deployment-vllm-cpu.yaml`](https://github.com/kaito-project/kaito/tree/main/examples/custom-model-integration/custom-model-deployment-vllm-cpu.yaml)

The corresponding image build target is:

```bash
make docker-build-kaito-vllm-cpu REGISTRY=<your-registry> OUTPUT_TYPE=type=docker
```

The example uses `kaito.sh/runtime: vllm`, a CPU-oriented vLLM image, and `VLLM_TARGET_DEVICE=cpu`. This is best viewed as a runtime experiment to validate the minimal KAITO template path with the upstream vLLM CPU backend.

## Limitations

- **Stable custom-model path is still Hugging Face-first**: the normal/productized custom-model path remains HuggingFace-oriented. The vLLM CPU path documented above is an experimental POC rather than a general custom-model guarantee.
- **No multi-node inference**: Distributed inference across multiple nodes is not supported for custom model.
- **No autoscaling**: This POC does not attempt to integrate custom-model autoscaling, regardless of whether the runtime is Hugging Face or experimental vLLM CPU.
- **No presets**: Users must manually modify the command line in the pod template for parameter changes.
