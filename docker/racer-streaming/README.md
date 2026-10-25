# Racer streaming POC runtime image

See [deployment and validation guide](../../docs/racer-model-streaming-poc.md).

Reuse the existing **kaito-base** image. There is no separate streaming-base
image or build target. The Racer Make target derives its input from the existing
`REGISTRY`, `KAITO_BASE_IMG_NAME`, and `KAITO_BASE_IMG_TAG` settings and passes it
to the Dockerfile's internal `KAITO_BASE_IMAGE` build argument.

The current preset pins the core/Azure Model Streamer wheel pair to 0.16.0.
The published Linux x86_64 wheels support the required cache ABI; no
source-built streamer or Python loader monkey-patch is required. The runtime
build checks both versions, native cache-loader markers, and dlopens the
provider to verify `cache_abi_version() == 1`, `blob_read`, and `shutdown`.
These checks do **not** establish end-to-end GPU/cache correctness.

```bash
export REGISTRY=andyzhangx OUTPUT_TYPE=type=docker ARCH=amd64
export PLATFORMS=linux/amd64 VERSION=0.13.0-racer
export KAITO_BASE_IMG_NAME=kaito-base
export KAITO_BASE_IMG_TAG="$VERSION"
export RACER_STREAMING_IMAGE="$REGISTRY/kaito-vllm-racer:$VERSION"

# Optional: skip these two lines if that compatible kaito-base already exists.
make docker-build-kaito-base ARCH=amd64
docker push "$REGISTRY/$KAITO_BASE_IMG_NAME:$KAITO_BASE_IMG_TAG"

make docker-build-racer-streaming ARCH=amd64
docker push "$RACER_STREAMING_IMAGE"
```

The existing base target uses `docker/presets/models/tfs/Dockerfile`. Its default
name/tag remain `kaito-base:v0.0.1`; `KAITO_BASE_IMG_TAG` is independent of `VERSION`,
so set it explicitly as above or select an existing compatible tag. No extra
full-base-image environment variable is required. The runtime target does not
build or push the base implicitly, and its output must not overwrite the base.

With `OUTPUT_TYPE=type=docker`, images are loaded into the local daemon, **not**
pushed. The docker-container builder cannot generally consume a daemon-local
base: make it registry-accessible before the Racer build, which uses `--pull`.
`OUTPUT_TYPE=type=registry` explicitly exports to the registry instead; do not
then depend on local images or repeat the `docker push` commands. The final
output defaults to `$REGISTRY/kaito-vllm-racer:$VERSION` and can be overridden
with `RACER_STREAMING_IMAGE`. No images are built or pushed by tests.

Use Linux amd64 and a compatible, versioned KAITO preset base. The provider
targets Ubuntu 22.04 glibc 2.35, compatible with the newer Debian bookworm
preset; the final-image dlopen check verifies linkage. An arbitrary old
kaito-base tag may lack the required preset code or wheel/ABI versions.
All environment configuration must be injected before process startup (Go
c-shared snapshots the process environment); restart rather than changing it
from Python after loading. Provider account/container config is process-wide.
