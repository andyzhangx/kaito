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

package consts

const (
	// Feature flag for llm-d inference scheduler integration.
	FeatureFlagLlmdScheduler = "llmdScheduler"

	// LlmdSchedulerImage is the default image for the llm-d inference scheduler EPP.
	LlmdSchedulerImage = "ghcr.io/llm-d/llm-d-inference-scheduler:latest"

	// LlmdSchedulerPort is the gRPC port for the EPP endpoint picker.
	LlmdSchedulerPort = int32(9002)

	// LlmdSchedulerMetricsPort is the metrics port for the llm-d scheduler.
	LlmdSchedulerMetricsPort = int32(9090)

	// LlmdSchedulerHealthPort is the health check port for the llm-d scheduler.
	LlmdSchedulerHealthPort = int32(8081)

	// LlmdRoleLabel is the pod label used by llm-d filters to identify pod roles.
	LlmdRoleLabel = "llm-d.ai/role"

	// LlmdInferenceServingLabel is the pod label used by llm-d for pod discovery.
	LlmdInferenceServingLabel = "llm-d.ai/inferenceServing"

	// LlmdRoleDecode indicates a pod that can serve decode requests.
	LlmdRoleDecode = "decode"

	// LlmdRolePrefill indicates a pod that can serve prefill requests.
	LlmdRolePrefill = "prefill"

	// LlmdRolePrefillDecode indicates a pod that can serve both prefill and decode requests.
	LlmdRolePrefillDecode = "prefill-decode"

	// LlmdConfigMapSuffix is the suffix for the llm-d EndpointPickerConfig ConfigMap.
	LlmdConfigMapSuffix = "-llmd-epp-config"

	// LlmdSchedulerSuffix is the suffix for the llm-d scheduler Deployment.
	LlmdSchedulerSuffix = "-llmd-scheduler"
)
