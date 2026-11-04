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

package multiroleinference

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	inferencesetutil "github.com/kaito-project/kaito/pkg/utils/inferenceset"
)

func TestReconcileInferenceSetPropagatesAnnotations(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kaitov1alpha1.AddToScheme(scheme))
	require.NoError(t, kaitov1beta1.AddToScheme(scheme))

	mri := &kaitov1alpha1.MultiRoleInference{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mri-test",
			Namespace: "default",
			Annotations: map[string]string{
				"kaito.sh/model-streaming":          "disabled",
				"kaito.sh/disable-benchmark":        "true",
				kaitov1beta1.AnnotationCapacityType: "spot",
			},
		},
		Spec: kaitov1alpha1.MultiRoleInferenceSpec{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "mri-test"}},
			Model:         kaitov1alpha1.MultiRoleInferenceModelSpec{Name: "google/gemma-4-E2B-it"},
		},
	}
	role := kaitov1alpha1.MultiRoleInferenceRoleSpec{
		Type:         kaitov1alpha1.MultiRoleInferenceRolePrefill,
		InstanceType: "Standard_NV36ads_A10_v5",
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mri).Build()
	r := &MultiRoleInferenceReconciler{Client: cl, Scheme: scheme}

	require.NoError(t, r.reconcileInferenceSet(context.Background(), mri, role))

	got := &kaitov1beta1.InferenceSet{}
	require.NoError(t, cl.Get(context.Background(),
		client.ObjectKey{Name: "mri-test-prefill", Namespace: "default"}, got))

	assert.Equal(t, "disabled", got.Spec.Template.Annotations["kaito.sh/model-streaming"])
	assert.Equal(t, "true", got.Spec.Template.Annotations["kaito.sh/disable-benchmark"])
	assert.Equal(t, "spot", got.Spec.Template.Annotations[kaitov1beta1.AnnotationCapacityType])
}

func TestReconcileInferenceSetClearsAnnotationsWhenMRIAnnotationsRemoved(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kaitov1alpha1.AddToScheme(scheme))
	require.NoError(t, kaitov1beta1.AddToScheme(scheme))

	mri := &kaitov1alpha1.MultiRoleInference{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mri-test",
			Namespace: "default",
		},
		Spec: kaitov1alpha1.MultiRoleInferenceSpec{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "mri-test"}},
			Model:         kaitov1alpha1.MultiRoleInferenceModelSpec{Name: "gemma-3-4b-instruct"},
		},
	}
	role := kaitov1alpha1.MultiRoleInferenceRoleSpec{
		Type:         kaitov1alpha1.MultiRoleInferenceRolePrefill,
		InstanceType: "Standard_NV36ads_A10_v5",
	}
	is := &kaitov1beta1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mri-test-prefill",
			Namespace: "default",
		},
		Spec: kaitov1beta1.InferenceSetSpec{
			Template: kaitov1beta1.InferenceSetTemplate{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					kaitov1alpha1.AnnotationEnableSpeculativeDecoding: "true",
				}},
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mri, is).Build()
	r := &MultiRoleInferenceReconciler{Client: cl, Scheme: scheme}

	require.NoError(t, r.reconcileInferenceSet(context.Background(), mri, role))

	got := &kaitov1beta1.InferenceSet{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Name: "mri-test-prefill", Namespace: "default"}, got))
	assert.Nil(t, got.Spec.Template.Annotations)
}

// Exercise the manual handoff used by the lab POC, not HPA behavior or API-server defaulting.
func TestReconcileInferenceSetExternalReplicaOwnership(t *testing.T) {
	ctx := context.Background()
	for _, roleType := range []kaitov1alpha1.MultiRoleInferenceRoleType{
		kaitov1alpha1.MultiRoleInferenceRolePrefill, kaitov1alpha1.MultiRoleInferenceRoleDecode,
	} {
		t.Run(string(roleType), func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, kaitov1alpha1.AddToScheme(scheme))
			require.NoError(t, kaitov1beta1.AddToScheme(scheme))
			mri := &kaitov1alpha1.MultiRoleInference{
				ObjectMeta: metav1.ObjectMeta{Name: "external-replicas", Namespace: "default", UID: types.UID("mri-uid")},
				Spec: kaitov1alpha1.MultiRoleInferenceSpec{
					Model:         kaitov1alpha1.MultiRoleInferenceModelSpec{Name: "microsoft/Phi-4-mini-instruct"},
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "poc"}},
				},
			}
			role := kaitov1alpha1.MultiRoleInferenceRoleSpec{Type: roleType, Replicas: ptr.To[int32](1), InstanceType: "Standard_NC24ads_A100_v4"}
			c := fake.NewClientBuilder().WithScheme(scheme).Build()
			r := &MultiRoleInferenceReconciler{Client: c, Scheme: scheme}
			require.NoError(t, r.reconcileInferenceSet(ctx, mri, role))
			key := types.NamespacedName{Name: mri.Name + "-" + string(roleType), Namespace: mri.Namespace}
			child := &kaitov1beta1.InferenceSet{}
			require.NoError(t, c.Get(ctx, key, child))
			require.Equal(t, int32(1), *child.Spec.Replicas)

			// Operator removes the parent count, then a separate writer scales the existing child.
			role.Replicas = nil
			child.Spec.Replicas = ptr.To[int32](3)
			require.NoError(t, c.Update(ctx, child))
			originalTemplate := child.Spec.Template.DeepCopy()
			originalHash := inferencesetutil.ComputeInferenceSetHash(child)
			annotations := map[string]string{
				"scaledobject.kaito.sh/auto-provision": "false",
				"scaledobject.kaito.sh/min-replicas":   "1",
				"scaledobject.kaito.sh/max-replicas":   "3",
				"scaledobject.kaito.sh/metrics":        "- name: vllm:num_requests_waiting\n  type: gauge\n  source: modelpod\n  upthreshold: 2\n  downthreshold: 1\n",
				"example.com/operator-note":            "preserve",
			}
			child.Annotations = annotations
			child.Labels["example.com/operator-label"] = "preserve"
			require.NoError(t, c.Update(ctx, child))

			for i := 0; i < 3; i++ {
				// A fresh reconciler has no memory of the bootstrap or external scale write.
				r = &MultiRoleInferenceReconciler{Client: c, Scheme: scheme}
				require.NoError(t, r.reconcileInferenceSet(ctx, mri, role))
				require.NoError(t, c.Get(ctx, key, child))
				assert.Equal(t, int32(3), *child.Spec.Replicas)
				assert.Equal(t, annotations, child.Annotations)
				assert.Equal(t, "preserve", child.Labels["example.com/operator-label"])
				assert.Equal(t, *originalTemplate, child.Spec.Template)
				assert.Equal(t, originalHash, inferencesetutil.ComputeInferenceSetHash(child))
				require.Len(t, child.OwnerReferences, 1)
				assert.Equal(t, mri.UID, child.OwnerReferences[0].UID)
			}

			// Once the external writer is stopped, explicit fixed counts still reconcile.
			role.Replicas = ptr.To[int32](2)
			require.NoError(t, r.reconcileInferenceSet(ctx, mri, role))
			require.NoError(t, c.Get(ctx, key, child))
			assert.Equal(t, int32(2), *child.Spec.Replicas)
			assert.Equal(t, annotations, child.Annotations)
			assert.Equal(t, *originalTemplate, child.Spec.Template)
		})
	}
}
