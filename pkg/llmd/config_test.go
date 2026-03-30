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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
)

func TestGenerateEndpointPickerConfig_Disabled(t *testing.T) {
	_, err := GenerateEndpointPickerConfig("test-model", nil)
	assert.Error(t, err)

	_, err = GenerateEndpointPickerConfig("test-model", &kaitov1beta1.DisaggregatedServingSpec{Enabled: false})
	assert.Error(t, err)
}

func TestGenerateEndpointPickerConfig_SingleProfile(t *testing.T) {
	spec := &kaitov1beta1.DisaggregatedServingSpec{
		Enabled: true,
	}

	config, err := GenerateEndpointPickerConfig("Qwen/Qwen3-32B", spec)
	require.NoError(t, err)

	// Should have single-profile-handler (non-disagg mode)
	assert.Contains(t, config, "single-profile-handler")
	assert.Contains(t, config, "precise-prefix-cache-scorer")
	assert.Contains(t, config, "max-score-picker")
	assert.Contains(t, config, "load-aware-scorer")
	assert.Contains(t, config, "Qwen/Qwen3-32B")
	assert.Contains(t, config, "name: default")

	// Should NOT have P/D disagg components
	assert.NotContains(t, config, "prefill-filter")
	assert.NotContains(t, config, "decode-filter")
	assert.NotContains(t, config, "disagg-profile-handler")
}

func TestGenerateEndpointPickerConfig_PDDisagg(t *testing.T) {
	spec := &kaitov1beta1.DisaggregatedServingSpec{
		Enabled:         true,
		PrefillReplicas: 2,
		DecodeReplicas:  3,
		Scheduler: &kaitov1beta1.LlmdSchedulerConfig{
			BlockSize:               64,
			HashSeed:                "12345",
			NonCachedTokenThreshold: 128,
		},
	}

	config, err := GenerateEndpointPickerConfig("meta-llama/Llama-3.1-8B-Instruct", spec)
	require.NoError(t, err)

	// Should have P/D disagg components
	assert.Contains(t, config, "prefill-filter")
	assert.Contains(t, config, "decode-filter")
	assert.Contains(t, config, "disagg-profile-handler")
	assert.Contains(t, config, "disagg-headers-handler")
	assert.Contains(t, config, "prefix-based-pd-decider")
	assert.Contains(t, config, "name: prefill")
	assert.Contains(t, config, "name: decode")

	// Should have correct parameters
	assert.Contains(t, config, "blockSize: 64")
	assert.Contains(t, config, "hashSeed: \"12345\"")
	assert.Contains(t, config, "nonCachedTokens: 128")
	assert.Contains(t, config, "meta-llama/Llama-3.1-8B-Instruct")

	// Should NOT have single-profile
	assert.NotContains(t, config, "single-profile-handler")
}

func TestGenerateEndpointPickerConfig_SpeculativeIndexing(t *testing.T) {
	// Enabled (default)
	spec := &kaitov1beta1.DisaggregatedServingSpec{
		Enabled: true,
		Scheduler: &kaitov1beta1.LlmdSchedulerConfig{
			SpeculativeTTL: "5s",
		},
	}
	config, err := GenerateEndpointPickerConfig("test-model", spec)
	require.NoError(t, err)
	assert.Contains(t, config, "speculativeIndexing: true")
	assert.Contains(t, config, "speculativeTTL: 5s")
	assert.Contains(t, config, "prepareDataPlugins")

	// Disabled
	spec.Scheduler.EnableSpeculativeIndexing = ptr.To(false)
	config, err = GenerateEndpointPickerConfig("test-model", spec)
	require.NoError(t, err)
	assert.NotContains(t, config, "speculativeIndexing")
}

func TestGenerateEndpointPickerConfig_NoLoadAware(t *testing.T) {
	spec := &kaitov1beta1.DisaggregatedServingSpec{
		Enabled: true,
		Scheduler: &kaitov1beta1.LlmdSchedulerConfig{
			Scorers: &kaitov1beta1.ScorerWeights{
				PrefixCacheWeight: 100,
				LoadAwareWeight:   0,
			},
		},
	}

	config, err := GenerateEndpointPickerConfig("test-model", spec)
	require.NoError(t, err)
	assert.NotContains(t, config, "load-aware-scorer")
}

func TestGenerateVLLMKVEventsConfig(t *testing.T) {
	tests := []struct {
		name     string
		spec     *kaitov1beta1.DisaggregatedServingSpec
		model    string
		expected string
	}{
		{
			name:     "nil spec",
			spec:     nil,
			model:    "test",
			expected: `{"enable_kv_cache_events":true}`,
		},
		{
			name: "default config",
			spec: &kaitov1beta1.DisaggregatedServingSpec{
				Scheduler: &kaitov1beta1.LlmdSchedulerConfig{},
			},
			model:    "Qwen/Qwen3-32B",
			expected: `{"enable_kv_cache_events":true,"publisher":"zmq","endpoint":"tcp://*:5557","topic":"kv@${POD_IP}@Qwen/Qwen3-32B"}`,
		},
		{
			name: "custom port",
			spec: &kaitov1beta1.DisaggregatedServingSpec{
				Scheduler: &kaitov1beta1.LlmdSchedulerConfig{
					KVEventsPort: 6000,
					TopicFilter:  "cache@",
				},
			},
			model:    "my-model",
			expected: `{"enable_kv_cache_events":true,"publisher":"zmq","endpoint":"tcp://*:6000","topic":"cache@${POD_IP}@my-model"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateVLLMKVEventsConfig(tt.spec, tt.model)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGenerateVLLMHashSeedEnv(t *testing.T) {
	assert.Equal(t, "0", GenerateVLLMHashSeedEnv(nil))
	assert.Equal(t, "0", GenerateVLLMHashSeedEnv(&kaitov1beta1.DisaggregatedServingSpec{}))
	assert.Equal(t, "12345", GenerateVLLMHashSeedEnv(&kaitov1beta1.DisaggregatedServingSpec{
		Scheduler: &kaitov1beta1.LlmdSchedulerConfig{HashSeed: "12345"},
	}))
	assert.Equal(t, "0", GenerateVLLMHashSeedEnv(&kaitov1beta1.DisaggregatedServingSpec{
		Scheduler: &kaitov1beta1.LlmdSchedulerConfig{HashSeed: "notanumber"},
	}))
}

func TestGetPodRole(t *testing.T) {
	// Nil spec
	assert.Equal(t, "", GetPodRole(nil, false))

	// Disabled
	assert.Equal(t, "", GetPodRole(&kaitov1beta1.DisaggregatedServingSpec{Enabled: false}, false))

	// Enabled, non-disagg (all pods serve both roles)
	spec := &kaitov1beta1.DisaggregatedServingSpec{Enabled: true}
	assert.Equal(t, "prefill-decode", GetPodRole(spec, false))
	assert.Equal(t, "prefill-decode", GetPodRole(spec, true))

	// P/D disagg
	pdSpec := &kaitov1beta1.DisaggregatedServingSpec{
		Enabled:         true,
		PrefillReplicas: 2,
		DecodeReplicas:  3,
	}
	assert.Equal(t, "prefill", GetPodRole(pdSpec, true))
	assert.Equal(t, "decode", GetPodRole(pdSpec, false))
}

func TestGenerateEndpointPickerConfig_YAMLFormat(t *testing.T) {
	spec := &kaitov1beta1.DisaggregatedServingSpec{
		Enabled: true,
	}

	config, err := GenerateEndpointPickerConfig("test-model", spec)
	require.NoError(t, err)

	// Verify it starts with the expected header
	lines := strings.Split(config, "\n")
	assert.True(t, len(lines) > 2)
	assert.Contains(t, config, "apiVersion: inference.networking.x-k8s.io/v1alpha1")
	assert.Contains(t, config, "kind: EndpointPickerConfig")
}
