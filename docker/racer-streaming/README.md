# Racer streaming POC runtime image

See [deployment and validation guide](../../docs/racer-model-streaming-poc.md).

The existing preset runtime pins `runai-model-streamer[azure]==0.16.0`. The
published Linux x86_64 core/Azure 0.16.0 wheel pair supports this cache ABI;
no source-built streamer or Python loader monkey-patch is required. The build
checks both installed versions, native cache-loader markers, and dlopens the
provider to verify `cache_abi_version() == 1`, `blob_read`, and `shutdown`.
These checks do **not** establish end-to-end GPU/cache correctness.

```bash
docker build -f docker/racer-streaming/Dockerfile \
  --build-arg KAITO_STREAMING_BASE_IMAGE="$KAITO_STREAMING_BASE_IMAGE" \
  -t "$RACER_RUNTIME_IMAGE" .
```

Use Linux amd64 and a digest-pinned, matching KAITO vLLM preset base image.
The provider targets Ubuntu 22.04 glibc 2.35, compatible with the newer
Debian bookworm preset; the final-image dlopen check verifies linkage.
All environment configuration must be injected before process startup (Go
c-shared snapshots the process environment); restart rather than changing it
from Python after loading. Provider account/container config is process-wide.
