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

package webhooks

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	_ "knative.dev/pkg/client/injection/kube/client/fake"
	_ "knative.dev/pkg/client/injection/kube/informers/admissionregistration/v1/validatingwebhookconfiguration/fake"
	_ "knative.dev/pkg/injection/clients/namespacedkube/informers/core/v1/secret/fake"
	knativetesting "knative.dev/pkg/reconciler/testing"
	"knative.dev/pkg/webhook"

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
)

// This covers the registered validating webhook, not a live API server's CRD defaults.
func TestMultiRoleInferenceAdmissionPreservesOmittedReplicas(t *testing.T) {
	t.Setenv("SYSTEM_NAMESPACE", "kaito-system")
	ctx, _ := knativetesting.SetupFakeContext(t)
	ctx = webhook.WithOptions(ctx, webhook.Options{SecretName: "webhook-secret"})
	controller := NewMultiRoleInferenceCRDValidationWebhook(ctx, nil)
	admitter, ok := controller.Reconciler.(webhook.AdmissionController)
	require.True(t, ok)
	mri := &kaitov1alpha1.MultiRoleInference{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kaito.sh/v1alpha1", Kind: "MultiRoleInference"},
		ObjectMeta: metav1.ObjectMeta{Name: "poc", Namespace: "default"},
		Spec: kaitov1alpha1.MultiRoleInferenceSpec{
			Model:         kaitov1alpha1.MultiRoleInferenceModelSpec{Name: "microsoft/Phi-4-mini-instruct"},
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "poc"}},
			Roles: []kaitov1alpha1.MultiRoleInferenceRoleSpec{
				{Type: kaitov1alpha1.MultiRoleInferenceRolePrefill, InstanceType: "Standard_NC24ads_A100_v4"},
				{Type: kaitov1alpha1.MultiRoleInferenceRoleDecode, InstanceType: "Standard_NC24ads_A100_v4"},
			},
		},
	}
	raw, err := json.Marshal(mri)
	require.NoError(t, err)
	old := mri.DeepCopy()
	one := int32(1)
	for i := range old.Spec.Roles {
		old.Spec.Roles[i].Replicas = &one
	}
	oldRaw, err := json.Marshal(old)
	require.NoError(t, err)
	for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
		t.Run(string(operation), func(t *testing.T) {
			request := &admissionv1.AdmissionRequest{
				Kind:      metav1.GroupVersionKind{Group: "kaito.sh", Version: "v1alpha1", Kind: "MultiRoleInference"},
				Operation: operation,
				Object:    runtime.RawExtension{Raw: raw},
			}
			if operation == admissionv1.Update {
				request.OldObject = runtime.RawExtension{Raw: oldRaw}
			}
			response := admitter.Admit(ctx, request)
			require.True(t, response.Allowed, "%+v", response.Result)
			require.Empty(t, response.Patch)
			require.Nil(t, response.PatchType)
			// The persisted object is unchanged by a validating admission response.
			decoded := &kaitov1alpha1.MultiRoleInference{}
			require.NoError(t, json.Unmarshal(request.Object.Raw, decoded))
			for _, role := range decoded.Spec.Roles {
				require.Nil(t, role.Replicas)
			}
		})
	}
}
