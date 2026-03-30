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
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kaito-project/kaito/pkg/utils/consts"
)

func TestGenerateSchedulerDeployment(t *testing.T) {
	spec := &kaitov1beta1.DisaggregatedServingSpec{
		Enabled: true,
	}
	ownerRef := metav1.OwnerReference{
		APIVersion: "kaito.sh/v1beta1",
		Kind:       "Workspace",
		Name:       "test-ws",
	}

	deploy := GenerateSchedulerDeployment("test-ws", "default", "test-model", "test-pool", spec, ownerRef)

	assert.Equal(t, "test-ws"+consts.LlmdSchedulerSuffix, deploy.Name)
	assert.Equal(t, "default", deploy.Namespace)
	assert.Len(t, deploy.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "epp", deploy.Spec.Template.Spec.Containers[0].Name)
	assert.Equal(t, consts.LlmdSchedulerImage, deploy.Spec.Template.Spec.Containers[0].Image)
	assert.NotNil(t, deploy.Spec.Template.Spec.Containers[0].ReadinessProbe)
	assert.NotNil(t, deploy.Spec.Template.Spec.Containers[0].LivenessProbe)
}

func TestGenerateSchedulerDeployment_CustomImage(t *testing.T) {
	spec := &kaitov1beta1.DisaggregatedServingSpec{
		Enabled: true,
		Scheduler: &kaitov1beta1.LlmdSchedulerConfig{
			Image: "my-registry/llmd:v1.0",
		},
	}
	ownerRef := metav1.OwnerReference{}

	deploy := GenerateSchedulerDeployment("test-ws", "default", "test-model", "test-pool", spec, ownerRef)

	assert.Equal(t, "my-registry/llmd:v1.0", deploy.Spec.Template.Spec.Containers[0].Image)
}

func TestGenerateSchedulerService(t *testing.T) {
	ownerRef := metav1.OwnerReference{}
	svc := GenerateSchedulerService("test-ws", "default", ownerRef)

	assert.Equal(t, "test-ws"+consts.LlmdSchedulerSuffix, svc.Name)
	assert.Len(t, svc.Spec.Ports, 2)
	assert.Equal(t, consts.LlmdSchedulerPort, svc.Spec.Ports[0].Port)
}

func TestGenerateConfigMap(t *testing.T) {
	ownerRef := metav1.OwnerReference{}
	cm := GenerateConfigMap("test-ws", "default", "test-yaml-content", ownerRef)

	assert.Equal(t, "test-ws"+consts.LlmdConfigMapSuffix, cm.Name)
	assert.Equal(t, "test-yaml-content", cm.Data["config.yaml"])
}

func TestGenerateServiceAccount(t *testing.T) {
	ownerRef := metav1.OwnerReference{}
	sa := GenerateServiceAccount("test-ws", "default", ownerRef)

	assert.Equal(t, "test-ws"+consts.LlmdSchedulerSuffix, sa.Name)
}

func TestGenerateRole(t *testing.T) {
	ownerRef := metav1.OwnerReference{}
	role := GenerateRole("test-ws", "default", ownerRef)

	assert.Equal(t, "test-ws"+consts.LlmdSchedulerSuffix, role.Name)
	assert.True(t, len(role.Rules) >= 3, "should have rules for pods, services, and inference resources")

	// Verify pods rule exists
	hasPodRule := false
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if res == "pods" {
				hasPodRule = true
			}
		}
	}
	assert.True(t, hasPodRule)
}

func TestGenerateRoleBinding(t *testing.T) {
	ownerRef := metav1.OwnerReference{}
	rb := GenerateRoleBinding("test-ws", "default", ownerRef)

	assert.Equal(t, "test-ws"+consts.LlmdSchedulerSuffix, rb.Name)
	assert.Len(t, rb.Subjects, 1)
	assert.Equal(t, "test-ws"+consts.LlmdSchedulerSuffix, rb.Subjects[0].Name)
}
