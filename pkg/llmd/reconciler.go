// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package llmd

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kaito-project/kaito/pkg/utils"
	"github.com/kaito-project/kaito/pkg/utils/resources"
)

// EnsureLlmdScheduler reconciles all llm-d inference scheduler resources
// for a Workspace with disaggregated serving enabled.
//
// This creates or updates:
// - ConfigMap with EndpointPickerConfig
// - ServiceAccount + Role + RoleBinding for pod/InferencePool access
// - Deployment for the llm-d EPP
// - Service exposing the EPP gRPC endpoint
//
// All resources are owned by the Workspace, so they are garbage collected
// when the Workspace is deleted.
func EnsureLlmdScheduler(
	ctx context.Context,
	c client.Client,
	wObj *kaitov1beta1.Workspace,
	modelName string,
) error {
	if wObj.Inference == nil ||
		wObj.Inference.DisaggregatedServing == nil ||
		!wObj.Inference.DisaggregatedServing.Enabled {
		return nil
	}

	spec := wObj.Inference.DisaggregatedServing
	poolName := utils.InferencePoolName(wObj.Name)

	ownerRef := *metav1.NewControllerRef(wObj, kaitov1beta1.GroupVersion.WithKind("Workspace"))

	// 1. Generate and apply EndpointPickerConfig ConfigMap
	configYAML, err := GenerateEndpointPickerConfig(modelName, spec)
	if err != nil {
		return fmt.Errorf("failed to generate EndpointPickerConfig: %w", err)
	}

	configMap := GenerateConfigMap(wObj.Name, wObj.Namespace, configYAML, ownerRef)
	if err := createOrUpdate(ctx, c, configMap, func(existing *corev1.ConfigMap) {
		existing.Data = configMap.Data
	}); err != nil {
		return fmt.Errorf("failed to ensure ConfigMap: %w", err)
	}

	// 2. ServiceAccount
	sa := GenerateServiceAccount(wObj.Name, wObj.Namespace, ownerRef)
	if err := createOrUpdate(ctx, c, sa, func(existing *corev1.ServiceAccount) {
		// ServiceAccount has no spec to update
	}); err != nil {
		return fmt.Errorf("failed to ensure ServiceAccount: %w", err)
	}

	// 3. Role
	role := GenerateRole(wObj.Name, wObj.Namespace, ownerRef)
	if err := createOrUpdate(ctx, c, role, func(existing *rbacv1.Role) {
		existing.Rules = role.Rules
	}); err != nil {
		return fmt.Errorf("failed to ensure Role: %w", err)
	}

	// 4. RoleBinding
	rb := GenerateRoleBinding(wObj.Name, wObj.Namespace, ownerRef)
	if err := createOrUpdate(ctx, c, rb, func(existing *rbacv1.RoleBinding) {
		existing.Subjects = rb.Subjects
		existing.RoleRef = rb.RoleRef
	}); err != nil {
		return fmt.Errorf("failed to ensure RoleBinding: %w", err)
	}

	// 5. Scheduler Deployment
	deploy := GenerateSchedulerDeployment(wObj.Name, wObj.Namespace, modelName, poolName, spec, ownerRef)
	if err := createOrUpdate(ctx, c, deploy, func(existing *appsv1.Deployment) {
		existing.Spec = deploy.Spec
	}); err != nil {
		return fmt.Errorf("failed to ensure Deployment: %w", err)
	}

	// 6. Scheduler Service
	svc := GenerateSchedulerService(wObj.Name, wObj.Namespace, ownerRef)
	if err := createOrUpdate(ctx, c, svc, func(existing *corev1.Service) {
		existing.Spec.Ports = svc.Spec.Ports
		existing.Spec.Selector = svc.Spec.Selector
	}); err != nil {
		return fmt.Errorf("failed to ensure Service: %w", err)
	}

	klog.InfoS("llm-d scheduler resources reconciled",
		"workspace", klog.KObj(wObj),
		"model", modelName,
		"pdDisagg", spec.PrefillReplicas > 0 || spec.DecodeReplicas > 0,
	)

	return nil
}

// createOrUpdate is a generic helper that creates a resource if it doesn't exist,
// or updates it if it does.
func createOrUpdate[T client.Object](ctx context.Context, c client.Client, desired T, updateFn func(existing T)) error {
	existing := desired.DeepCopyObject().(T)
	err := c.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.InfoS("Creating llm-d resource", "kind", fmt.Sprintf("%T", desired), "name", desired.GetName())
			return resources.CreateResource(ctx, desired, c)
		}
		return err
	}

	// Update existing
	updateFn(existing)
	return c.Update(ctx, existing)
}
