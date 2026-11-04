# MRI autoscaling POC: KAITO setup

## Purpose

Verify that prefill and decode child InferenceSets can be scaled independently
without the parent MultiRoleInference (MRI) resetting their replica counts.
Use an isolated test cluster: this branch changes cluster-wide CRD defaults.

## Replica behavior

- Explicit positive role `replicas`: MRI maintains that fixed child count.
- Omitted role `replicas`: MRI starts a **new** child at one replica, then leaves
  its count to an external writer. This does not enable an autoscaler by itself.
- Existing children are not reinitialized, including children at zero.
- Previously stored parent `replicas: 1` values remain until explicitly removed.

## Minimal verification

1. Install the chart and controller built from this branch, with both controllers
   enabled. Preview the chart with your lab values:

   ```sh
   helm template mri-poc charts/kaito/workspace \
     --set featureGates.enableInferenceSetController=true \
     --set featureGates.enableMultiRoleInferenceController=true \
     > /tmp/mri-poc-rendered.yaml
   ```

   This renders only; install using your normal lab procedure and pinned images.
   Check the installed MRI CRD has no role replica default. No post-renderer is needed.

2. Adapt [the Phi-4 example](../kaito_multiroleinference_phi4_4.yaml), initially
   setting both roles to `replicas: 1`. Wait for successful P/D inference and keep
   autoscaling disabled. Ensure spare GPU capacity is available.

3. Edit the MRI and remove `replicas` from **both** roles. Verify role types rather
   than assuming array order, then read the object back:

   ```sh
   kubectl edit multiroleinference <mri-name> -n <namespace>
   kubectl get multiroleinference <mri-name> -n <namespace> -o yaml
   ```

   Both fields must remain absent; stop if admission restores them.

4. With no HPA or other scaler targeting the children, identify the child owned by
   this MRI for each role. Test a positive increase on one child at a time:

   ```sh
   kubectl scale inferenceset <verified-child-name> -n <namespace> \
     --current-replicas=1 --replicas=2
   kubectl get inferenceset <verified-child-name> -n <namespace> -o yaml
   ```

   Verify the count survives repeated MRI reconciles and a controller restart,
   and that the added Workspaces become ready and serve requests. Repeat for the other role.

## Enabling autoscaling

Automatic scaling requires the matching `keda-kaito-scaler` POC; stock scaler
behavior does not support MRI children. Configure scaler annotations on each
child's **top-level metadata**, not on the MRI or Workspace template.

This is a scale-up-only test POC, not production support or live GPU/HPA validation.
For scaler configuration, authorization, metrics checks and shutdown, follow the
[implementation plan](https://github.com/andyzhangx/demo/blob/d2828050ea9657eda90aa9157cbdb949d9b552e8/llm/kaito/mri-pd-autoscaling-poc-implementation-plan.md).
Before restoring fixed parent counts or rolling back the chart, stop and verify
removal of the external SO/HPA writers; set each parent count to its current positive child count.
