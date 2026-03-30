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

package v1beta1

// DisaggregatedServingSpec configures Prefill/Decode disaggregated inference
// with llm-d scheduler integration.
type DisaggregatedServingSpec struct {
	// Enabled enables P/D disaggregated serving with llm-d inference scheduler.
	// When enabled, the controller will deploy an llm-d EPP (Endpoint Picker) alongside
	// the inference workload, configure KV cache event ingestion, and set up
	// prefix-cache-aware routing.
	// +optional
	// +kubebuilder:default:=false
	Enabled bool `json:"enabled,omitempty"`

	// PrefillReplicas is the number of vLLM pods dedicated to prefill.
	// If 0 and Enabled is true, all pods run in prefill-decode mode.
	// +optional
	// +kubebuilder:default:=0
	PrefillReplicas int `json:"prefillReplicas,omitempty"`

	// DecodeReplicas is the number of vLLM pods dedicated to decode.
	// If 0 and Enabled is true, all pods run in prefill-decode mode.
	// +optional
	// +kubebuilder:default:=0
	DecodeReplicas int `json:"decodeReplicas,omitempty"`

	// SchedulerConfig contains fine-grained configuration for the llm-d scheduler.
	// +optional
	Scheduler *LlmdSchedulerConfig `json:"scheduler,omitempty"`
}

// LlmdSchedulerConfig configures the llm-d inference scheduler (EPP).
type LlmdSchedulerConfig struct {
	// Image overrides the default llm-d scheduler image.
	// +optional
	Image string `json:"image,omitempty"`

	// BlockSize is the number of tokens per KV block. Must match vLLM's --block-size.
	// +optional
	// +kubebuilder:default:=16
	BlockSize int `json:"blockSize,omitempty"`

	// HashSeed is the hash seed for block key computation. Must match vLLM's PYTHONHASHSEED.
	// +optional
	// +kubebuilder:default:="0"
	HashSeed string `json:"hashSeed,omitempty"`

	// NonCachedTokenThreshold is the uncached-token threshold for P/D disaggregation bypass.
	// When the best decode worker's uncached tokens are below this threshold,
	// the router skips prefill disaggregation.
	// +optional
	// +kubebuilder:default:=256
	NonCachedTokenThreshold int `json:"nonCachedTokenThreshold,omitempty"`

	// KVEventsPort is the ZMQ port for KV cache events on vLLM pods.
	// +optional
	// +kubebuilder:default:=5557
	KVEventsPort int `json:"kvEventsPort,omitempty"`

	// TopicFilter is the ZMQ topic prefix filter for KV events.
	// +optional
	// +kubebuilder:default:="kv@"
	TopicFilter string `json:"topicFilter,omitempty"`

	// EnableSpeculativeIndexing enables speculative index insertion after routing decisions.
	// +optional
	// +kubebuilder:default:=true
	EnableSpeculativeIndexing *bool `json:"enableSpeculativeIndexing,omitempty"`

	// SpeculativeTTL is the TTL for speculative index entries (Go duration string, e.g., "2s").
	// +optional
	// +kubebuilder:default:="2s"
	SpeculativeTTL string `json:"speculativeTTL,omitempty"`

	// Scorers configures additional scorer weights for the scheduling profiles.
	// +optional
	Scorers *ScorerWeights `json:"scorers,omitempty"`
}

// ScorerWeights configures the relative weights of different scorers.
type ScorerWeights struct {
	// PrefixCacheWeight is the weight for the precise-prefix-cache-scorer.
	// +optional
	// +kubebuilder:default:=50
	PrefixCacheWeight int `json:"prefixCacheWeight,omitempty"`

	// LoadAwareWeight is the weight for the load-aware-scorer. Set to 0 to disable.
	// +optional
	// +kubebuilder:default:=10
	LoadAwareWeight int `json:"loadAwareWeight,omitempty"`

	// SessionAffinityWeight is the weight for the session-affinity-scorer. Set to 0 to disable.
	// +optional
	// +kubebuilder:default:=0
	SessionAffinityWeight int `json:"sessionAffinityWeight,omitempty"`
}
