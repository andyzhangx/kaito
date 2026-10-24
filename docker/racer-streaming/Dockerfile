# Build from the repository root; the base must contain the audited
# runai-model-streamer{,-azure}==0.16.0 wheel pair. No wheel replacement here.
ARG KAITO_STREAMING_BASE_IMAGE
FROM golang:1.26.10-bookworm AS go-toolchain
# Use a glibc 2.35 baseline compatible with the current bookworm preset.
FROM ubuntu:22.04 AS provider-builder
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY tools/racer-cache-provider/ ./
RUN CGO_ENABLED=1 go test ./... \
    && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o /out/libkaito_racer_cache.so .

FROM ${KAITO_STREAMING_BASE_IMAGE}
COPY --from=provider-builder /out/libkaito_racer_cache.so /opt/kaito/racer/libkaito_racer_cache.so
COPY tools/racer-cache-provider/check_runtime.py /opt/kaito/racer/check_runtime.py
RUN python3 /opt/kaito/racer/check_runtime.py
