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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kaito-project/kaito/pkg/utils/consts"
)

// GenerateSchedulerDeployment creates the llm-d inference scheduler Deployment manifest.
func GenerateSchedulerDeployment(
	workspaceName, namespace, modelName, poolName string,
	spec *kaitov1beta1.DisaggregatedServingSpec,
	ownerRef metav1.OwnerReference,
) *appsv1.Deployment {
	schedulerName := workspaceName + consts.LlmdSchedulerSuffix
	configMapName := workspaceName + consts.LlmdConfigMapSuffix

	image := consts.LlmdSchedulerImage
	if spec.Scheduler != nil && spec.Scheduler.Image != "" {
		image = spec.Scheduler.Image
	}

	labels := map[string]string{
		"app":                          schedulerName,
		"kaito.sh/workspace":          workspaceName,
		"kaito.sh/component":          "llmd-scheduler",
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            schedulerName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels:          labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: schedulerName,
					Containers: []corev1.Container{
						{
							Name:  "epp",
							Image: image,
							Args: []string{
								"--configFile=/etc/epp/config.yaml",
								fmt.Sprintf("--poolName=%s", poolName),
								fmt.Sprintf("--poolNamespace=%s", namespace),
								"--grpcPort=9002",
								"--metricsPort=9090",
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "grpc",
									ContainerPort: consts.LlmdSchedulerPort,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "metrics",
									ContainerPort: consts.LlmdSchedulerMetricsPort,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "health",
									ContainerPort: consts.LlmdSchedulerHealthPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "HF_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: workspaceName + "-hf-token",
											},
											Key:      "token",
											Optional: ptr.To(true),
										},
									},
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt32(consts.LlmdSchedulerHealthPort),
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       10,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt32(consts.LlmdSchedulerHealthPort),
									},
								},
								InitialDelaySeconds: 15,
								PeriodSeconds:       20,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "epp-config",
									MountPath: "/etc/epp",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "epp-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMapName,
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// GenerateSchedulerService creates the Service for the llm-d scheduler.
func GenerateSchedulerService(
	workspaceName, namespace string,
	ownerRef metav1.OwnerReference,
) *corev1.Service {
	schedulerName := workspaceName + consts.LlmdSchedulerSuffix
	labels := map[string]string{
		"app":                 schedulerName,
		"kaito.sh/workspace": workspaceName,
		"kaito.sh/component": "llmd-scheduler",
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            schedulerName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels:          labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "grpc",
					Port:       consts.LlmdSchedulerPort,
					TargetPort: intstr.FromString("grpc"),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "metrics",
					Port:       consts.LlmdSchedulerMetricsPort,
					TargetPort: intstr.FromString("metrics"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// GenerateConfigMap creates the ConfigMap containing the EndpointPickerConfig YAML.
func GenerateConfigMap(
	workspaceName, namespace, configYAML string,
	ownerRef metav1.OwnerReference,
) *corev1.ConfigMap {
	configMapName := workspaceName + consts.LlmdConfigMapSuffix
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            configMapName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels: map[string]string{
				"kaito.sh/workspace": workspaceName,
				"kaito.sh/component": "llmd-scheduler",
			},
		},
		Data: map[string]string{
			"config.yaml": configYAML,
		},
	}
}

// GenerateServiceAccount creates the ServiceAccount for the llm-d scheduler.
func GenerateServiceAccount(
	workspaceName, namespace string,
	ownerRef metav1.OwnerReference,
) *corev1.ServiceAccount {
	schedulerName := workspaceName + consts.LlmdSchedulerSuffix
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            schedulerName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels: map[string]string{
				"kaito.sh/workspace": workspaceName,
				"kaito.sh/component": "llmd-scheduler",
			},
		},
	}
}

// GenerateRole creates the Role granting the llm-d scheduler permissions
// to watch pods, InferencePool, and InferenceModel resources.
func GenerateRole(
	workspaceName, namespace string,
	ownerRef metav1.OwnerReference,
) *rbacv1.Role {
	schedulerName := workspaceName + consts.LlmdSchedulerSuffix
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:            schedulerName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels: map[string]string{
				"kaito.sh/workspace": workspaceName,
				"kaito.sh/component": "llmd-scheduler",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"endpoints", "services"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"inference.networking.x-k8s.io"},
				Resources: []string{"inferencepools", "inferencemodels"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"inference.networking.x-k8s.io"},
				Resources: []string{"inferencepools/status"},
				Verbs:     []string{"update", "patch"},
			},
		},
	}
}

// GenerateRoleBinding creates the RoleBinding for the llm-d scheduler.
func GenerateRoleBinding(
	workspaceName, namespace string,
	ownerRef metav1.OwnerReference,
) *rbacv1.RoleBinding {
	schedulerName := workspaceName + consts.LlmdSchedulerSuffix
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            schedulerName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels: map[string]string{
				"kaito.sh/workspace": workspaceName,
				"kaito.sh/component": "llmd-scheduler",
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      schedulerName,
				Namespace: namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     schedulerName,
		},
	}
}
