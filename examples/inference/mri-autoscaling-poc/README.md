# MRI manual scale-up POC: KAITO-side preparation only

This is a **disposable-cluster experiment**, not supported production autoscaling or the managed-mode proposal. It implements the KAITO replica-defaulting and bootstrap changes for [section 0 of the POC plan](https://github.com/andyzhangx/demo/blob/d2828050ea9657eda90aa9157cbdb949d9b552e8/llm/kaito/mri-pd-autoscaling-poc-implementation-plan.md). It does **not** implement the required scaler changes or prove GPU serving, routing, HPA behavior, or safe downscale.

## Runtime behavior and compatibility

The MRI API `SetDefaults` and Helm CRD now preserve omitted role `replicas`, matching the existing base CRD/API contract. The MRI controller initializes a **new** child InferenceSet to one replica when its parent count is absent; for an **existing** child it leaves the count untouched. Explicit positive parent counts still reconcile the child to that fixed value. Child bootstrap never writes a default back to the parent.

This changes omitted-count semantics: omission is no longer a durable fixed count of one. It means bootstrap at one and then permit an external writer to own that child's count. It does not start or authorize an autoscaler. If you want a fixed workload, specify a positive count explicitly for every role. An existing MRI whose counts were previously defaulted and stored as one stays fixed at one after upgrade; nothing automatically clears its counts or transfers ownership. Remove counts only through the verified handoff below.

The old POC post-renderer is retired: **no overlay or new policy annotation is required**. The chart and base CRD replica schemas agree, retain `minimum: 1`, and make replicas optional. The registered MRI webhook remains validating-only; tests also exercise `SetDefaults` directly and Kubernetes structural schema defaulting to guard both paths. These local tests do not prove every additional admission/conversion component in a real cluster leaves nil intact.

## 1. Render and inspect; install only in the dedicated lab

From the repository root:

```sh
helm template mri-poc charts/kaito/workspace \
  --set featureGates.enableInferenceSetController=true \
  --set featureGates.enableMultiRoleInferenceController=true \
  > /tmp/mri-poc-rendered.yaml
go test ./api/v1alpha1 ./pkg/controllers/multiroleinference ./pkg/workspace/webhooks
```

Inspect the output and use the pinned chart/controller build with your normal lab values. Pin the chart commit, controller/scaler/KEDA/vLLM/sidecar images and configuration; the render command alone does not pin runtime images or constitute deployment. The CRD change is **cluster-scoped**, not a per-MRI opt-in. Test this branch in an isolated cluster, not a shared/production installation. Review omission semantics before any broader upgrade.

After installation, inspect `kubectl get crd multiroleinferences.kaito.sh -o yaml`: every served role replica schema must lack `default`, remain optional, and retain `minimum: 1`. Inspect all installed mutating/conversion webhooks. Do not proceed if readback reinserts a value. Rolling back to an older chart that restores the default can reintroduce competing writers; first complete the fixed-ownership handoff below.

## 2. Bootstrap positive fixed roles with autoscaling disabled

Use [the Phi-4 example](../kaito_multiroleinference_phi4_4.yaml), adapted for your lab, with **both roles explicitly set to `replicas: 1`** (or another safe positive count). Keep topology/GPU/TP fixed and suitable spare capacity available. The controller also supports initially omitted parent counts by creating children at one; this runbook deliberately starts with explicit counts so ownership transfer is an observable operator step. Never rely on an HPA to rescue an existing zero/nil child.

Record the MRI namespace/name/UID, exact prefill/decode role membership, and both child names/UIDs/controller-owner references in the lab allowlist. Wait for child InferenceSets, Workspaces, engine pods, and a working P/D route. Pod readiness alone is insufficient: confirm successful routed inference and KV transfer. Keep the scaler disabled and verify no SO/HPA or other controller currently targets either child.

## 3. Establish external replica ownership manually

1. Save MRI/child JSON. With a resource-version/UID-checked edit, remove `spec.roles[].replicas` for **both** roles after verifying their types. Do not assume array indexes identify roles. Read the stored MRI back through the API, not from a local manifest.
2. Both counts must remain absent across unrelated metadata updates, repeated MRI reconciliations, and a controller restart. Read positive child counts back each time. Save evidence. Stop if any admission/controller path restores a parent count.
3. Verify the installed InferenceSet `/scale` API and operator/HPA RBAC; inspect its current `spec.replicas`, `status.replicas`, and `status.selector`. With no active HPA, make one operator-controlled positive increase, for example:

   ```sh
   kubectl scale inferenceset <verified-prefill-child> -n <namespace> \
     --current-replicas=1 --replicas=2
   ```

   Recheck identities immediately before the command. Repeat reconciles/restart and verify the count is not reverted, newly added Workspaces become Ready, and they actually receive routed inference work. Repeat separately for decode. These are operator writes, not scaler writes. Do not overlap them with an HPA.

## 4. Manually configure child metadata, still disabled

Put existing scaler annotations on each **child's top-level `metadata.annotations`**, never the parent or `spec.template`. For prefill:

```yaml
metadata:
  annotations:
    scaledobject.kaito.sh/auto-provision: "false"
    scaledobject.kaito.sh/min-replicas: "1"
    scaledobject.kaito.sh/max-replicas: "3" # must be >= the current child count
    scaledobject.kaito.sh/metrics: |
      - name: vllm:num_requests_waiting
        type: gauge
        source: modelpod
        aggregation: service-avg
        upthreshold: 2
        downthreshold: 1
```

For decode use `vllm:num_requests_running`, with independently calibrated thresholds. These are parser examples, not recommended capacity targets. Require `0 <= downthreshold < upthreshold`, positive bounds, and max no lower than the live count. Both parser thresholds are required; they do not authorize downscale.

Check preservation across MRI reconciles/restarts. Save and compare `spec.template`, the InferenceSet spec hash, Workspace templates and model pod identities before/after **annotation-only** changes; there must be no rollout. The controller unit tests cover positive creation bootstrap, no reinitialization of existing children (including nil/zero counts), metadata/template/spec-hash preservation and newly constructed reconcilers, not a real process restart or downstream rollout. Existing nil/zero children are not automatically repaired: resolve those states with autoscaling disabled before entering this positive-only experiment.

## 5. Stop here until the separate scaler implementation is ready

Keep `auto-provision: "false"`. Stock `keda-kaito-scaler` excludes MRI children; changing that predicate alone is unsafe. This branch neither creates SOs/HPAs nor adds any scaler implementation. Before activation, the separate scaler work must provide:

- Explicit feature gate/UID allowlist; live parent/child owner, role and persisted-nil authorization during reconciliation **and metric serving**, including resync and ScaledObject-triggered paths.
- Exactly one authorized SO/HPA pair per child and no competing writers; never adopt foreign objects.
- Strict, complete, fresh role-local gauge samples with readiness/membership agreement; missing endpoints, invalid/stale data or partial coverage must mean unavailable, not zero or a partial average.
- Verified decode Service/sidecar `/metrics` routing (Service `80 -> 5000`, engine `5001` is not automatically a Service port).
- Enforced scale-up-only HPA behavior with drift repair; reject bounds below the current count and protect against concurrent writers. `scaleDown.selectPolicy: Disabled` alone does not prevent above-max clamping.
- No scale-to-zero, idle/fallback/restore-original/pause-replica bypasses. Failure injection must freeze decisions. Newly added role capacity must actually serve routed requests.

Follow the linked plan's full acceptance checklist before claiming a working autoscaling POC. No automatic downscale/drain, production readiness, GPU deployment, or performance validation is provided here.

## 6. Manual exit and restore fixed ownership

Block new activation/recreation in the lab scaler authorization gate first. Identify the exact demo-owned SO/HPA UIDs and target references, disable auto-provision, and stop the external writer. Delete only verified demo-owned objects using your approved lab procedure; **verify both SO and HPA absence** and that neither is recreated. KEDA unavailability or uncertain ownership means handoff is blocked, not successful. Do not adopt/delete foreign objects.

Only after external writers are conclusively gone, set each parent role to its **current positive child count** and verify fixed reconciliation resumes without an unintended decrease. Do not roll back to an older chart/defaulting implementation until every role has explicit fixed counts and the handoff is complete. Manual capacity reduction, if desired, requires a separate workload-safe procedure; this POC has no drain/downscale protocol.
