# Istio Gateway Output Guardrail PoC — Implementation Overview

## Goal

Evaluate whether KAITO can enforce **output guardrails at the Istio Gateway layer** by using Envoy `ext_proc`, while reusing the existing RAGEngine output guardrail logic where possible.

The first PoC focuses on **non-streaming OpenAI-compatible responses**.

## High-Level Architecture

```
KAITO / vLLM
    ↓
    | non-streaming model response
    ↓
Istio Gateway / Envoy
    ↓
    | response body via ext_proc
    ↓
Gateway Guard Processor
    ↓
    | parse OpenAI response
    | extract assistant content
    ↓
Existing KAITO OutputGuardrails
    ↓
    | allow / redact / block
    ↓
Modified response
    ↓
Istio / Envoy
    ↓
Client
```

The existing KAITO inference routing path remains unchanged. The PoC only adds a response-side processing stage at the Gateway.

## Phase 1 Scope

**Will support:**
- Output guardrail only
- Non-streaming responses only
- `/v1/chat/completions`
- OpenAI-compatible JSON responses
- Envoy `ext_proc` with `response_body = BUFFERED`
- Reuse existing KAITO `OutputGuardrails`
- Response-only scanners: `ban_substrings`, `secrets`, PII
- `allow`, `redact`, and `block` actions
- Fail-closed behavior
- Basic latency and resource overhead measurement

**Will NOT include:**
- Input guardrails
- SSE streaming
- Holdback-window logic
- Model-based scanners requiring request context
- KAITO CRD/API changes
- InferenceSet controller changes
- BBR or EPP scheduling changes

## Implementation Plan

### PR1 — Minimal `ext_proc` Response Mutation

**Goal**: Prove that Istio Gateway can invoke an additional Envoy external processor on the response path.

**Implementation**:
- Minimal `ext_proc` gRPC service
- Process `response_body` only
- Use `BUFFERED` response mode
- Deterministic response mutation (e.g., `hello world` → `hello world [GATEWAY_TEST]`)

**Success criteria**:
- ✓ Existing KAITO inference request still succeeds
- ✓ Gateway invokes the new processor
- ✓ Client receives modified response
- ✓ Existing GWIE/EPP routing unaffected

**Target**: < 200 LOC

---

### PR2 — OpenAI Response Adapter

**Goal**: Correctly process non-streaming OpenAI-compatible chat completion responses.

**Implementation**:
- Parse response JSON
- Extract and modify: `choices[*].message.content`
- Reconstruct valid JSON
- Handle unsupported or malformed responses gracefully

**Success criteria**:
- ✓ Clean response remains valid
- ✓ Assistant content can be modified
- ✓ Other response fields unchanged
- ✓ Client can deserialize result

**Target**: < 200 LOC

---

### PR3 — KAITO OutputGuardrails Integration

**Goal**: Reuse existing RAGEngine guardrail implementation instead of building a second scanner framework.

**Implementation**:
```
response body
    ↓
OpenAI adapter
    ↓
ChatCompletionResponse
    ↓
OutputGuardrails.guard_response(...)
    ↓
allow / redact / block
    ↓
serialized response
```

Focus on scanners that don't require request context initially.

**Success criteria**:
- ✓ Clean output passes unchanged
- ✓ Unsafe output can be redacted
- ✓ Unsafe output can be blocked
- ✓ Scanner failures follow fail-closed policy

**Target**: < 200 LOC

---

### PR4 — E2E Validation, Benchmark, and Documentation

**Goal**: Validate the PoC works in real KAITO + Istio Gateway environment.

**Validation cases**:
1. Clean response → unchanged
2. Sensitive response → redacted
3. Blocked response → block message
4. Guard processor unavailable → fail-closed
5. Existing EPP routing still works
6. Response framing valid after body mutation

**Measurements**:
- P50, P95, P99 latency
- CPU and memory overhead

**Mostly**: tests, manifests, benchmarks, and documentation.

## Timeline

- **PR1**: This week (Minimal ext_proc)
- **PR2**: +3-5 days (JSON adapter)
- **PR3**: +1-2 days (OutputGuardrails integration)
- **PR4**: +2-3 days (E2E + docs)

**Total: 2-3 weeks**

## Key Technical Risks

### 1. Istio/Envoy Filter Integration
The additional `ext_proc` must coexist with existing GWIE config and must not interfere with EPP. Validate using `istioctl proxy-config` or Envoy `config_dump`.

### 2. Response Body Mutation
Body changes may affect HTTP framing (Content-Length, transfer encoding). Initial PoC supports uncompressed `application/json` only.

### 3. Request Context
Existing `OutputGuardrails.guard_response()` accepts model response and original request. Phase 1 uses scanners that don't depend on prompt context. Context-aware scanners evaluated in later phase.

### 4. Failure Semantics
Must distinguish:
- Scanner failure → fail-closed
- ext_proc service unavailable → fail-closed

Both cases must be deterministic.

## Streaming Follow-Up (Out of Scope)

Streaming support excluded from initial PoC. Future phase can investigate `STREAMED` / `FULL_DUPLEX_STREAMED` response processing.

RAGEngine already provides building blocks: SSE parsing, ordered buffering, holdback window, leakage prevention. Future implementation can evaluate reuse.

## PoC Success Criteria (Six ✓)

1. ✓ Istio Gateway invokes output `ext_proc`
2. ✓ Non-streaming model responses inspected
3. ✓ Clean output passes through unchanged
4. ✓ Unsafe output redacted
5. ✓ Unsafe output blocked
6. ✓ Existing KAITO GWIE/EPP routing continues to work

**All six ✓ = PoC successful = question answered: Yes, we can do it.**

## Next Steps After PoC Success

- **Option A**: Quick release L1 (non-streaming only)
- **Option B**: Add L2 (streaming support, +1-2 weeks)
- **Option C**: Add L3 (model-based scanners, needs cost evaluation first)

## Key Files

- `processor/server.py` - gRPC server implementation
- `envoyfilter.yaml` - Istio filter configuration
- `deployment.yaml` - Kubernetes deployment
- `README.md` - Deployment and testing guide
