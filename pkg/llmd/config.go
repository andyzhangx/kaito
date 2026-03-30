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
	"strconv"

	"gopkg.in/yaml.v2"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
)

// EndpointPickerConfig represents the llm-d EPP configuration YAML.
type EndpointPickerConfig struct {
	APIVersion          string               `yaml:"apiVersion"`
	Kind                string               `yaml:"kind"`
	FeatureGates        []string             `yaml:"featureGates,omitempty"`
	Plugins             []PluginConfig       `yaml:"plugins"`
	SchedulingProfiles  []SchedulingProfile  `yaml:"schedulingProfiles"`
}

// PluginConfig represents a single plugin entry in the EPP config.
type PluginConfig struct {
	Name       string                 `yaml:"name,omitempty"`
	Type       string                 `yaml:"type"`
	Parameters map[string]interface{} `yaml:"parameters,omitempty"`
}

// SchedulingProfile defines a named scheduling profile with plugin references.
type SchedulingProfile struct {
	Name    string            `yaml:"name"`
	Plugins []PluginReference `yaml:"plugins"`
}

// PluginReference references a plugin by name with an optional weight.
type PluginReference struct {
	PluginRef string `yaml:"pluginRef"`
	Weight    int    `yaml:"weight,omitempty"`
}

// GenerateEndpointPickerConfig generates the llm-d EndpointPickerConfig YAML
// for a given model name and disaggregated serving configuration.
func GenerateEndpointPickerConfig(modelName string, spec *kaitov1beta1.DisaggregatedServingSpec) (string, error) {
	if spec == nil || !spec.Enabled {
		return "", fmt.Errorf("disaggregated serving is not enabled")
	}

	scheduler := spec.Scheduler
	if scheduler == nil {
		scheduler = &kaitov1beta1.LlmdSchedulerConfig{}
	}

	blockSize := scheduler.BlockSize
	if blockSize == 0 {
		blockSize = 16
	}

	hashSeed := scheduler.HashSeed
	if hashSeed == "" {
		hashSeed = "0"
	}

	kvEventsPort := scheduler.KVEventsPort
	if kvEventsPort == 0 {
		kvEventsPort = 5557
	}

	topicFilter := scheduler.TopicFilter
	if topicFilter == "" {
		topicFilter = "kv@"
	}

	nonCachedTokenThreshold := scheduler.NonCachedTokenThreshold
	if nonCachedTokenThreshold == 0 {
		nonCachedTokenThreshold = 256
	}

	speculativeTTL := scheduler.SpeculativeTTL
	if speculativeTTL == "" {
		speculativeTTL = "2s"
	}

	enableSpeculative := true
	if scheduler.EnableSpeculativeIndexing != nil {
		enableSpeculative = *scheduler.EnableSpeculativeIndexing
	}

	prefixCacheWeight := 50
	loadAwareWeight := 10
	if scheduler.Scorers != nil {
		if scheduler.Scorers.PrefixCacheWeight > 0 {
			prefixCacheWeight = scheduler.Scorers.PrefixCacheWeight
		}
		if scheduler.Scorers.LoadAwareWeight >= 0 {
			loadAwareWeight = scheduler.Scorers.LoadAwareWeight
		}
	}

	isPDDisagg := spec.PrefillReplicas > 0 || spec.DecodeReplicas > 0

	// Build plugins list
	plugins := []PluginConfig{}
	featureGates := []string{}

	// 1. precise-prefix-cache-scorer (always present)
	prefixScorerParams := map[string]interface{}{
		"tokenProcessorConfig": map[string]interface{}{
			"blockSize": blockSize,
			"hashSeed":  hashSeed,
		},
		"indexerConfig": map[string]interface{}{
			"kvBlockIndexConfig": map[string]interface{}{
				"inMemoryConfig": map[string]interface{}{
					"size":         100000000,
					"podCacheSize": 10,
				},
				"enableMetrics": true,
			},
			"tokenizersPoolConfig": map[string]interface{}{
				"modelName": modelName,
			},
		},
		"kvEventsConfig": map[string]interface{}{
			"topicFilter":  topicFilter,
			"concurrency":  4,
			"discoverPods": true,
			"podDiscoveryConfig": map[string]interface{}{
				"socketPort": kvEventsPort,
			},
		},
	}

	if enableSpeculative {
		prefixScorerParams["speculativeIndexing"] = true
		prefixScorerParams["speculativeTTL"] = speculativeTTL
		featureGates = append(featureGates, "prepareDataPlugins")
	}

	plugins = append(plugins, PluginConfig{
		Type:       "precise-prefix-cache-scorer",
		Parameters: prefixScorerParams,
	})

	// 2. load-aware-scorer (optional)
	if loadAwareWeight > 0 {
		plugins = append(plugins, PluginConfig{
			Type: "load-aware-scorer",
			Parameters: map[string]interface{}{
				"threshold": 100,
			},
		})
	}

	// 3. max-score-picker
	plugins = append(plugins, PluginConfig{
		Type: "max-score-picker",
	})

	if isPDDisagg {
		// 4. Filters for P/D disaggregation
		plugins = append(plugins, PluginConfig{
			Type: "prefill-filter",
		})
		plugins = append(plugins, PluginConfig{
			Type: "decode-filter",
		})

		// 5. P/D decider
		plugins = append(plugins, PluginConfig{
			Name: "prefix-based-pd-decider",
			Type: "prefix-based-pd-decider",
			Parameters: map[string]interface{}{
				"nonCachedTokens": nonCachedTokenThreshold,
			},
		})

		// 6. Disagg headers handler
		plugins = append(plugins, PluginConfig{
			Type: "disagg-headers-handler",
		})

		// 7. Disagg profile handler
		plugins = append(plugins, PluginConfig{
			Type: "disagg-profile-handler",
			Parameters: map[string]interface{}{
				"deciders": map[string]interface{}{
					"prefill": "prefix-based-pd-decider",
				},
			},
		})

		if enableSpeculative {
			featureGates = appendUnique(featureGates, "prepareDataPlugins")
		}
	} else {
		// Non-disaggregated: single profile handler
		plugins = append(plugins, PluginConfig{
			Type: "single-profile-handler",
		})
	}

	// Build scheduling profiles
	profiles := []SchedulingProfile{}

	if isPDDisagg {
		// Prefill profile
		prefillPlugins := []PluginReference{
			{PluginRef: "prefill-filter"},
			{PluginRef: "max-score-picker"},
			{PluginRef: "precise-prefix-cache-scorer", Weight: prefixCacheWeight},
		}
		if loadAwareWeight > 0 {
			prefillPlugins = append(prefillPlugins, PluginReference{PluginRef: "load-aware-scorer", Weight: loadAwareWeight})
		}
		profiles = append(profiles, SchedulingProfile{
			Name:    "prefill",
			Plugins: prefillPlugins,
		})

		// Decode profile
		decodePlugins := []PluginReference{
			{PluginRef: "decode-filter"},
			{PluginRef: "max-score-picker"},
			{PluginRef: "precise-prefix-cache-scorer", Weight: prefixCacheWeight},
		}
		if loadAwareWeight > 0 {
			decodePlugins = append(decodePlugins, PluginReference{PluginRef: "load-aware-scorer", Weight: loadAwareWeight})
		}
		profiles = append(profiles, SchedulingProfile{
			Name:    "decode",
			Plugins: decodePlugins,
		})
	} else {
		// Single default profile
		defaultPlugins := []PluginReference{
			{PluginRef: "max-score-picker"},
			{PluginRef: "precise-prefix-cache-scorer", Weight: prefixCacheWeight},
		}
		if loadAwareWeight > 0 {
			defaultPlugins = append(defaultPlugins, PluginReference{PluginRef: "load-aware-scorer", Weight: loadAwareWeight})
		}
		profiles = append(profiles, SchedulingProfile{
			Name:    "default",
			Plugins: defaultPlugins,
		})
	}

	config := EndpointPickerConfig{
		APIVersion:         "inference.networking.x-k8s.io/v1alpha1",
		Kind:               "EndpointPickerConfig",
		FeatureGates:       featureGates,
		Plugins:            plugins,
		SchedulingProfiles: profiles,
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("failed to marshal EndpointPickerConfig: %w", err)
	}

	return string(data), nil
}

// GenerateVLLMKVEventsConfig returns the vLLM --kv-events-config JSON string
// for a given scheduler configuration.
func GenerateVLLMKVEventsConfig(spec *kaitov1beta1.DisaggregatedServingSpec, modelName string) string {
	if spec == nil || spec.Scheduler == nil {
		return `{"enable_kv_cache_events":true}`
	}

	port := spec.Scheduler.KVEventsPort
	if port == 0 {
		port = 5557
	}

	topic := spec.Scheduler.TopicFilter
	if topic == "" {
		topic = "kv@"
	}

	return fmt.Sprintf(
		`{"enable_kv_cache_events":true,"publisher":"zmq","endpoint":"tcp://*:%d","topic":"%s${POD_IP}@%s"}`,
		port, topic, modelName,
	)
}

// GenerateVLLMHashSeedEnv returns the PYTHONHASHSEED value from the scheduler config.
func GenerateVLLMHashSeedEnv(spec *kaitov1beta1.DisaggregatedServingSpec) string {
	if spec == nil || spec.Scheduler == nil || spec.Scheduler.HashSeed == "" {
		return "0"
	}
	// Validate it's a number
	if _, err := strconv.Atoi(spec.Scheduler.HashSeed); err != nil {
		return "0"
	}
	return spec.Scheduler.HashSeed
}

// GetPodRole determines the llm-d pod role label value based on the disaggregated serving config.
func GetPodRole(spec *kaitov1beta1.DisaggregatedServingSpec, isPrefill bool) string {
	if spec == nil || !spec.Enabled {
		return ""
	}

	isPDDisagg := spec.PrefillReplicas > 0 || spec.DecodeReplicas > 0
	if !isPDDisagg {
		// All pods serve both roles
		return "prefill-decode"
	}

	if isPrefill {
		return "prefill"
	}
	return "decode"
}

func appendUnique(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}
