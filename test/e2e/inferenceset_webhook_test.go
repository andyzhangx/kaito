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

package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kaito-project/kaito/test/e2e/utils"
)

var _ = Describe("InferenceSet Validation Webhook", utils.GinkgoLabelFastCheck, func() {
	var inferenceSet *kaitov1beta1.InferenceSet

	// Use an explicit API endpoint so the request really exercises the served
	// version, rather than a client's preferred version. Dry-run still invokes
	// admission but never persists an InferenceSet or provisions GPU resources.
	dryRunCreate := func(version string, obj *kaitov1beta1.InferenceSet) error {
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return err
		}
		request := &unstructured.Unstructured{Object: content}
		request.SetAPIVersion("kaito.sh/" + version)
		request.SetKind("InferenceSet")
		_, err = utils.TestingCluster.DynamicClient.Resource(schema.GroupVersionResource{
			Group: "kaito.sh", Version: version, Resource: "inferencesets",
		}).Namespace(namespaceName).Create(ctx, request, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		return err
	}

	BeforeEach(func() {
		inferenceSet = &kaitov1beta1.InferenceSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "admission-" + string(uuid.NewUUID()),
				Namespace: namespaceName,
			},
			Spec: kaitov1beta1.InferenceSetSpec{
				Replicas: ptr.To(int32(0)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "admission-test"}},
				Template: kaitov1beta1.InferenceSetTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "admission-test"},
						Annotations: map[string]string{kaitov1beta1.AnnotationWorkspaceRuntime: "vllm"},
					},
					Resource: kaitov1beta1.InferenceSetResourceSpec{InstanceType: "Standard_NC6s_v3"},
					// A custom template avoids model downloads, GPU-memory sizing,
					// model credentials, and BYO node discovery during validation.
					Inference: kaitov1beta1.InferenceSpec{Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{
							Name: "inference", Image: "registry.k8s.io/pause:3.10",
						}}},
					}},
				},
			},
		}
		if nodeProvisionerName == "byo" {
			inferenceSet.Spec.Template.Resource.InstanceType = ""
		}
		inferenceSet.Spec.Template.Annotations = utils.DisableModelStreaming(inferenceSet.Spec.Template.Annotations)
		// Establish a valid baseline and wait for admission readiness. Negative
		// cases below must fail for their exact validation error, not an outage.
		Eventually(func() error {
			return dryRunCreate("v1beta1", inferenceSet)
		}, utils.PollTimeout, utils.PollInterval).Should(Succeed())
	})

	DescribeTable("accepts a valid InferenceSet through each served version", func(version string) {
		Expect(dryRunCreate(version, inferenceSet)).To(Succeed())
	},
		Entry("v1alpha1", "v1alpha1"),
		Entry("v1beta1", "v1beta1"),
	)

	// These checks are specific to the beta validator. matchPolicy: Equivalent
	// can convert beta requests to alpha when beta is missing from the webhook
	// rule; a generic failure or an alpha-shared check would not detect that.
	DescribeTable("rejects invalid v1beta1 input at admission", func(mutate func(*kaitov1beta1.InferenceSet), message, field string) {
		mutate(inferenceSet)
		err := dryRunCreate("v1beta1", inferenceSet)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`admission webhook "validation.inferenceset.kaito.sh" denied the request`))
		Expect(err.Error()).To(ContainSubstring(message))
		Expect(err.Error()).To(ContainSubstring(field))
	},
		Entry("empty selector", func(obj *kaitov1beta1.InferenceSet) {
			obj.Spec.Selector = &metav1.LabelSelector{}
		}, "labelSelector must not be empty", "spec.labelSelector"),
		Entry("conflicting template labels", func(obj *kaitov1beta1.InferenceSet) {
			obj.Spec.Template.Labels["app"] = "conflicting"
		}, "must equal labelSelector.matchLabels", "spec.template.metadata.labels[app]"),
		Entry("missing child Workspace inference", func(obj *kaitov1beta1.InferenceSet) {
			obj.Spec.Template.Inference.Template = nil
		}, "Preset or Template must be specified", "spec.template.inference"),
		Entry("invalid child Workspace annotation", func(obj *kaitov1beta1.InferenceSet) {
			obj.Spec.Template.Annotations[kaitov1beta1.AnnotationPerformanceMode] = "invalid"
		}, "is not a valid performance mode", "spec.template.metadata.annotations["+kaitov1beta1.AnnotationPerformanceMode+"]"),
		Entry("missing child Workspace ConfigMap", func(obj *kaitov1beta1.InferenceSet) {
			obj.Spec.Template.Inference.Config = fmt.Sprintf("missing-%s", uuid.NewUUID())
		}, "specified in 'config' not found in namespace", "spec.template.config"),
	)
})
