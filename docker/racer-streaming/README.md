# Racer streaming POC runtime image

See [deployment and validation guide](../../docs/racer-model-streaming-poc.md).

The existing preset runtime pins `runai-model-streamer[azure]==0.16.0`. The
published Linux x86_64 core/Azure 0.16.0 wheel pair supports this cache ABI;
no source-built streamer or Python loader monkey-patch is required. The build
checks both installed versions, native cache-loader markers, and dlopens the
provider to verify `cache_abi_version() == 1`, `blob_read`, and `shutdown`.
These checks do **not** establish end-to-end GPU/cache correctness.

```bash
export REGISTRY=andyzhangx OUTPUT_TYPE=type=docker ARCH=amd64
export PLATFORMS=linux/amd64 VERSION=0.13.0-racer
export KAITO_STREAMING_BASE_IMAGE="$REGISTRY/kaito-streaming-base:$VERSION"
export RACER_STREAMING_IMAGE="$REGISTRY/kaito-vllm-racer:$VERSION"
make docker-build-kaito-streaming-base ARCH=amd64
docker push "$KAITO_STREAMING_BASE_IMAGE"
make docker-build-racer-streaming ARCH=amd64
```

The base target builds the existing vLLM/Streamer preset Dockerfile. If you
already have a compatible registry image, set `KAITO_STREAMING_BASE_IMAGE` to
its version tag/digest and skip the base build/push. With `OUTPUT_TYPE=type=docker`,
the image is loaded into the local daemon, **not** pushed; the docker-container
builder cannot generally consume that daemon-local base. Publish it before the
Racer build. `OUTPUT_TYPE=type=registry` instead explicitly exports to the registry.
The final output defaults to `$REGISTRY/kaito-vllm-racer:$VERSION` and can be
overridden with `RACER_STREAMING_IMAGE`. No images are built or pushed by tests.

Use Linux amd64 and a digest-pinned, matching KAITO vLLM preset base image.
The provider targets Ubuntu 22.04 glibc 2.35, compatible with the newer
Debian bookworm preset; the final-image dlopen check verifies linkage.
All environment configuration must be injected before process startup (Go
c-shared snapshots the process environment); restart rather than changing it
from Python after loading. Provider account/container config is process-wide.
