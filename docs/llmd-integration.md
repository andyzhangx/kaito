# llm-d Inference Scheduler Integration

This document describes how Kaito integrates with the [llm-d inference scheduler](https://github.com/llm-d/llm-d-inference-scheduler) to enable KV-cache-aware routing and Prefill/Decode (P/D) disaggregated inference.

## Overview

The llm-d inference scheduler is an Endpoint Picker (EPP) that extends the [Kubernetes Gateway API Inference Extension (GIE)](https://gateway-api-inference-extension.sigs.k8s.io/) with advanced scheduling algorithms. When integrated with Kaito, it provides:

- **Precise prefix-cache-aware routing** using real-time KV cache events from vLLM
- **P/D disaggregation bypass** — skip the prefill stage when the decode worker already has enough cached blocks
- **Speculative indexing** — prevent duplicate prefill work for overlapping prompts
- **Pluggable Filter → Scorer → Picker pipeline** with weighted scoring

## Architecture

```
                                    ┌──────────────────────────────────┐
                                    │        Kaito Controller          │
                                    │                                  │
                                    │  Workspace CR                    │
                                    │  ├── inference.disaggregatedServing │
                                    │  │   ├── enabled: true           │
                                    │  │   ├── prefillReplicas: 2      │
                                    │  │   └── decodeReplicas: 3       │
                                    │  │                               │
                                    │  Reconciles:                     │
                                    │  ├── vLLM StatefulSet (prefill)  │
                                    │  ├── vLLM StatefulSet (decode)   │
                                    │  ├── llm-d EPP Deployment        │
                                    │  ├── InferencePool (via GIE)     │
                                    │  └── EndpointPickerConfig CM     │
                                    └──────────────────────────────────┘

┌─────────────┐    HTTP     ┌──────────┐   gRPC    ┌──────────────────┐
│   Client     │──────────→│  Envoy   │──────────→│  llm-d EPP       │
│              │            │  Gateway │           │                  │
└─────────────┘            └──────────┘           │  ┌────────────┐  │
                                                   │  │ Filters    │  │
   ┌──────────────┐                                │  │ • prefill  │  │
   │ vLLM Prefill │─── ZMQ KV Events ────────────→│  │ • decode   │  │
   │   Pod 1..N   │                                │  ├────────────┤  │
   └──────────────┘                                │  │ Scorers    │  │
                                                   │  │ • prefix   │  │
   ┌──────────────┐                                │  │   cache    │  │
   │ vLLM Decode  │─── ZMQ KV Events ────────────→│  │ • load     │  │
   │   Pod 1..N   │                                │  │   aware    │  │
   └──────────────┘                                │  ├────────────┤  │
                                                   │  │ Picker     │  │
                                                   │  │ • max-score│  │
                                                   │  └────────────┘  │
                                                   └──────────────────┘
```

## Usage

### Basic: KV-Cache-Aware Routing (No P/D Disaggregation)

```yaml
apiVersion: kaito.sh/v1beta1
kind: Workspace
metadata:
  name: my-llm
resource:
  instanceType: Standard_NC24ads_A100_v4
  labelSelector:
    matchLabels:
      app: my-llm
inference:
  preset:
    name: "Qwen/Qwen3-32B"
  disaggregatedServing:
    enabled: true
    scheduler:
      blockSize: 16
      hashSeed: "12345"
```

This deploys:
- vLLM pods with KV cache events enabled on port 5557
- An llm-d EPP with `precise-prefix-cache-scorer` + `load-aware-scorer`
- All pods labeled as `llm-d.ai/role: prefill-decode`

### Advanced: P/D Disaggregated Inference

```yaml
apiVersion: kaito.sh/v1beta1
kind: Workspace
metadata:
  name: my-llm-pd
resource:
  instanceType: Standard_NC24ads_A100_v4
  labelSelector:
    matchLabels:
      app: my-llm-pd
inference:
  preset:
    name: "meta-llama/Llama-3.1-70B-Instruct"
  disaggregatedServing:
    enabled: true
    prefillReplicas: 2
    decodeReplicas: 3
    scheduler:
      blockSize: 64
      hashSeed: "12345"
      nonCachedTokenThreshold: 256
      enableSpeculativeIndexing: true
      speculativeTTL: "2s"
      scorers:
        prefixCacheWeight: 50
        loadAwareWeight: 10
```

This deploys:
- 2 vLLM pods labeled `llm-d.ai/role: prefill`
- 3 vLLM pods labeled `llm-d.ai/role: decode`
- An llm-d EPP with separate prefill/decode scheduling profiles
- `prefix-based-pd-decider` that bypasses P/D when uncached tokens < 256

## Configuration Reference

### DisaggregatedServingSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | false | Enable llm-d scheduler integration |
| `prefillReplicas` | int | 0 | Dedicated prefill pods (0 = all pods serve both roles) |
| `decodeReplicas` | int | 0 | Dedicated decode pods (0 = all pods serve both roles) |
| `scheduler` | LlmdSchedulerConfig | - | Scheduler configuration |

### LlmdSchedulerConfig

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `image` | string | ghcr.io/llm-d/llm-d-inference-scheduler:latest | EPP image |
| `blockSize` | int | 16 | Tokens per KV block (must match vLLM `--block-size`) |
| `hashSeed` | string | "0" | Hash seed (must match `PYTHONHASHSEED`) |
| `nonCachedTokenThreshold` | int | 256 | Uncached-token threshold for P/D bypass |
| `kvEventsPort` | int | 5557 | ZMQ port for KV cache events |
| `topicFilter` | string | "kv@" | ZMQ topic prefix filter |
| `enableSpeculativeIndexing` | *bool | true | Enable speculative index entries |
| `speculativeTTL` | string | "2s" | TTL for speculative entries |
| `scorers` | ScorerWeights | - | Scorer weights |

### ScorerWeights

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `prefixCacheWeight` | int | 50 | Weight for precise-prefix-cache-scorer |
| `loadAwareWeight` | int | 10 | Weight for load-aware-scorer (0 to disable) |
| `sessionAffinityWeight` | int | 0 | Weight for session-affinity-scorer (0 to disable) |

## Prerequisites

1. **Feature Gate**: Enable the `llmdScheduler` feature gate:
   ```
   --feature-gates=llmdScheduler=true
   ```

2. **Gateway API Inference Extension**: The GIE CRDs must be installed in the cluster.

3. **Envoy Gateway**: An Envoy-based gateway must be deployed to route traffic through the EPP.

## How It Works

### What Kaito Creates

When `disaggregatedServing.enabled: true`, the workspace controller creates:

1. **ConfigMap** (`{workspace}-llmd-epp-config`) — Contains the `EndpointPickerConfig` YAML
2. **ServiceAccount** — For the EPP pod
3. **Role + RoleBinding** — Grants access to pods, InferencePool, InferenceModel
4. **Deployment** (`{workspace}-llmd-scheduler`) — The llm-d EPP pod
5. **Service** — Exposes gRPC (9002) and metrics (9090) ports

All resources are owned by the Workspace and garbage collected on deletion.

### vLLM Pod Configuration

The controller automatically:
- Enables KV cache events (`--kv-events-config`)
- Sets `PYTHONHASHSEED` to match the scheduler's `hashSeed`
- Adds the `llm-d.ai/role` label to pods
- Adds the `llm-d.ai/inferenceServing: "true"` label for pod auto-discovery
- Exposes port 5557 for ZMQ KV event publishing

### Generated EndpointPickerConfig

For P/D disaggregated mode, the generated config looks like:

```yaml
apiVersion: inference.networking.x-k8s.io/v1alpha1
kind: EndpointPickerConfig
featureGates:
- prepareDataPlugins
plugins:
- type: precise-prefix-cache-scorer
  parameters:
    tokenProcessorConfig:
      blockSize: 16
      hashSeed: "12345"
    indexerConfig:
      kvBlockIndexConfig:
        inMemoryConfig:
          size: 100000000
          podCacheSize: 10
        enableMetrics: true
      tokenizersPoolConfig:
        modelName: Qwen/Qwen3-32B
    kvEventsConfig:
      topicFilter: "kv@"
      concurrency: 4
      discoverPods: true
      podDiscoveryConfig:
        socketPort: 5557
    speculativeIndexing: true
    speculativeTTL: "2s"
- type: load-aware-scorer
  parameters:
    threshold: 100
- type: max-score-picker
- type: prefill-filter
- type: decode-filter
- name: prefix-based-pd-decider
  type: prefix-based-pd-decider
  parameters:
    nonCachedTokens: 256
- type: disagg-headers-handler
- type: disagg-profile-handler
  parameters:
    deciders:
      prefill: prefix-based-pd-decider
schedulingProfiles:
- name: prefill
  plugins:
  - pluginRef: prefill-filter
  - pluginRef: max-score-picker
  - pluginRef: precise-prefix-cache-scorer
    weight: 50
  - pluginRef: load-aware-scorer
    weight: 10
- name: decode
  plugins:
  - pluginRef: decode-filter
  - pluginRef: max-score-picker
  - pluginRef: precise-prefix-cache-scorer
    weight: 50
  - pluginRef: load-aware-scorer
    weight: 10
```

## Comparison with vllm-router kv_aware

| Aspect | llm-d (this integration) | vllm-router kv_aware |
|--------|--------------------------|---------------------|
| Architecture | K8s-native EPP + Envoy | Standalone proxy |
| Scorer composition | ✅ Multiple weighted scorers | ❌ Single policy |
| KV Index backend | InMemory / Redis / Valkey | DashMap only |
| Multi-replica HA | ✅ Shared index via Redis | ❌ Per-instance |
| Pod auto-discovery | ✅ K8s reconciler | ❌ Service discovery |
| E/P/D disagg | ✅ Experimental | ❌ Not supported |
| Multi-engine | vLLM + SGLang | vLLM only |

## Related

- [llm-d architecture](https://github.com/llm-d/llm-d-inference-scheduler/blob/main/docs/architecture.md)
- [llm-d KV cache library](https://github.com/llm-d/llm-d-kv-cache)
- [vLLM KV Events](https://docs.vllm.ai/en/stable/api/vllm/config/kv_events/)
- [Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/)
