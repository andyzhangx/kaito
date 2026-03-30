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

// Package llmd provides integration with the llm-d inference scheduler
// (https://github.com/llm-d/llm-d-inference-scheduler) for Kaito workspaces.
//
// The llm-d scheduler uses a pluggable Filter → Scorer → Picker pipeline
// with real-time KV cache events from vLLM engines to make near-optimal
// routing decisions. This package generates the Kubernetes manifests
// (EndpointPickerConfig ConfigMap, scheduler Deployment, RBAC, etc.)
// needed to deploy the llm-d EPP alongside Kaito inference workloads.
package llmd
