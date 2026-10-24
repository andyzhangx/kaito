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

package modelstreaming

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kaito-project/kaito/pkg/utils/generator"
)

const (
	// AnnotationModelCache is independent of kaito.sh/model-streaming. POC opt-in.
	AnnotationModelCache = "kaito.sh/model-cache"
	racerContainerName   = "racer-object"
	racerVolumeName      = "racer-client"
)

// RacerBinding is trusted controller configuration, NOT a user-selected cache.
// One namespace is authorized for all data in this container/cache. Immutable
// attests that administrators do not overwrite mirrored blobs while in use.
// The POC neither creates ClusterCaches nor changes their identities/lifecycle.
type RacerBinding struct {
	Namespace    string `json:"namespace"`
	Account      string `json:"account"`
	Container    string `json:"container"`
	CacheName    string `json:"cacheName"`
	ObjectImage  string `json:"objectImage"`
	RuntimeImage string `json:"runtimeImage"`
	Immutable    bool   `json:"immutable"`
}

// RacerBindings is set once at controller startup from administrator flags.
var RacerBindings []RacerBinding

func ParseRacerBindings(raw string) ([]RacerBinding, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var bindings []RacerBinding
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bindings); err != nil {
		return nil, fmt.Errorf("invalid Racer bindings: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("Racer bindings must contain one JSON array")
	}
	accounts := regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	containers := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	seen := map[string]bool{}
	sources := map[string]string{}
	for i, b := range bindings {
		if len(validation.IsDNS1123Label(b.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(b.CacheName)) != 0 || len(b.CacheName) > 82 || !accounts.MatchString(b.Account) || !containers.MatchString(b.Container) || strings.Contains(b.Container, "--") || !b.Immutable {
			return nil, fmt.Errorf("Racer binding %d requires valid namespace/account/container/cacheName and immutable=true", i)
		}
		for _, image := range []string{b.ObjectImage, b.RuntimeImage} {
			if image == "" || strings.ContainsAny(image, " \t\n") || strings.HasSuffix(image, ":latest") || (!strings.Contains(image, "@sha256:") && !strings.Contains(image[strings.LastIndex(image, "/")+1:], ":")) {
				return nil, fmt.Errorf("Racer binding %d requires explicit non-latest objectImage and runtimeImage tags/digests", i)
			}
		}
		source := b.Account + "/" + b.Container
		key := b.Namespace + "/" + source
		if seen[key] {
			return nil, fmt.Errorf("duplicate Racer binding for namespace/account/container")
		}
		seen[key] = true
		if old, ok := sources[b.CacheName]; ok && old != source {
			return nil, fmt.Errorf("Racer cache cannot refer to different storage sources")
		}
		sources[b.CacheName] = source
	}
	return bindings, nil
}

// ValidateRacerRequest prevents an opt-in from silently becoming uncached when
// streaming is disabled or the runtime/source is unsupported.
func ValidateRacerRequest(ws *v1beta1.Workspace, streamingEnabled bool) error {
	mode := ws.Annotations[AnnotationModelCache]
	switch mode {
	case "", "disabled":
		return nil
	case "racer":
		if !streamingEnabled {
			return fmt.Errorf("Racer requires enabled vLLM ModelStreaming")
		}
		return nil
	default:
		return fmt.Errorf("unsupported %s: use racer or disabled", AnnotationModelCache)
	}
}

func resolveRacerBinding(ws *v1beta1.Workspace, cfg *StreamingConfig) (*RacerBinding, error) {
	if ws.Annotations[AnnotationModelCache] != "racer" {
		return nil, nil
	}
	if cfg == nil || len(cfg.InitContainers) > 0 || len(cfg.Volumes) > 0 {
		return nil, fmt.Errorf("Racer POC supports only Azure Workload Identity ModelMirror streaming, not SAS")
	}
	u, err := url.Parse(cfg.ModelPath)
	if err != nil || u.Scheme != "az" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || u.Path == "" || u.Path == "/" {
		return nil, fmt.Errorf("Racer requires a resolved az://container/model path")
	}
	account := ""
	for _, env := range cfg.ProviderEnvVars {
		switch env.Name {
		case "AZURE_STORAGE_ACCOUNT_NAME":
			if env.ValueFrom != nil || account != "" {
				return nil, fmt.Errorf("invalid Azure account configuration")
			}
			account = env.Value
		case "AZURE_STORAGE_ENDPOINT_SUFFIX":
			if env.Value != "blob.core.windows.net" {
				return nil, fmt.Errorf("Racer POC only supports Azure public cloud endpoints")
			}
		case "AZURE_STORAGE_SAS_TOKEN", "AZURE_STORAGE_ACCOUNT_KEY", "AZURE_STORAGE_CONNECTION_STRING":
			return nil, fmt.Errorf("Racer POC requires Workload Identity, not SAS/shared keys")
		}
	}
	for i := range RacerBindings {
		b := &RacerBindings[i]
		if b.Namespace == ws.Namespace && b.Account == account && b.Container == u.Host {
			if !b.Immutable {
				return nil, fmt.Errorf("Racer requires immutable model storage")
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("no administrator Racer binding for workspace namespace and resolved Azure account/container")
}

// applyRacer configures the shared generated PodSpec, so all StatefulSet ranks
// inherit the same sidecar and main-container environment. No dependency on
// webhook annotation propagation or an Unbounded Go SDK is required.
func applyRacer(ctx *generator.WorkspaceGeneratorContext, spec *corev1.PodSpec, cfg *StreamingConfig, mainIdx int) error {
	b, err := resolveRacerBinding(ctx.Workspace, cfg)
	if err != nil || b == nil {
		return err
	}
	if spec.HostNetwork {
		return fmt.Errorf("Racer loopback sidecar must not use hostNetwork")
	}
	for _, c := range append(append([]corev1.Container{}, spec.Containers...), spec.InitContainers...) {
		if c.Name == racerContainerName {
			return fmt.Errorf("Racer container name already in use")
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == racerVolumeName {
			return fmt.Errorf("Racer volume name already in use")
		}
	}
	// Preserve application identity/discovery and replace only the administrator-
	// selected runtime image and owned provider variables.
	app := &spec.Containers[mainIdx]
	app.Image = b.RuntimeImage
	env := map[string]string{
		"RUNAI_STREAMER_EXPERIMENTAL_AZURE_CACHE_ENABLED": "1",
		"RUNAI_STREAMER_EXPERIMENTAL_AZURE_CACHE_LIB":     "/opt/kaito/racer/libkaito_racer_cache.so",
		"KAITO_RACER_ENDPOINT":                            "http://127.0.0.1:18080",
		"KAITO_RACER_ACCOUNT":                             b.Account, "KAITO_RACER_CONTAINER": b.Container,
		"KAITO_RACER_TIMEOUT_MS": "30000", "KAITO_RACER_MAX_ATTEMPTS": "3", "KAITO_RACER_MAX_CONCURRENCY": "16",
	}
	kept := make([]corev1.EnvVar, 0, len(app.Env)+len(env))
	for _, e := range app.Env {
		if _, owned := env[e.Name]; !owned {
			kept = append(kept, e)
		}
	}
	// Explicit stable order avoids an endless StatefulSet rollout diff.
	for _, name := range []string{"RUNAI_STREAMER_EXPERIMENTAL_AZURE_CACHE_ENABLED", "RUNAI_STREAMER_EXPERIMENTAL_AZURE_CACHE_LIB", "KAITO_RACER_ENDPOINT", "KAITO_RACER_ACCOUNT", "KAITO_RACER_CONTAINER", "KAITO_RACER_TIMEOUT_MS", "KAITO_RACER_MAX_ATTEMPTS", "KAITO_RACER_MAX_CONCURRENCY"} {
		kept = append(kept, corev1.EnvVar{Name: name, Value: env[name]})
	}
	app.Env = kept
	socketDir := "/run/racer/" + b.CacheName + "/client"
	directory := corev1.HostPathDirectory
	spec.Volumes = append(spec.Volumes, corev1.Volume{Name: racerVolumeName, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: socketDir, Type: &directory}}})
	always := corev1.ContainerRestartPolicyAlways
	uid := int64(65532)
	yes, no := true, false
	sidecar := corev1.Container{
		Name: racerContainerName, Image: b.ObjectImage, ImagePullPolicy: corev1.PullIfNotPresent, RestartPolicy: &always,
		Args:            []string{"sidecar", "--api=azure", "--cache=" + b.CacheName, "--listen=127.0.0.1:18080"},
		SecurityContext: &corev1.SecurityContext{RunAsUser: &uid, RunAsGroup: &uid, RunAsNonRoot: &yes, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("512Mi")}},
		VolumeMounts:    []corev1.VolumeMount{{Name: racerVolumeName, MountPath: socketDir, ReadOnly: true}},
	}
	spec.InitContainers = append([]corev1.Container{sidecar}, spec.InitContainers...)
	if spec.NodeSelector == nil {
		spec.NodeSelector = map[string]string{}
	}
	if os := spec.NodeSelector["kubernetes.io/os"]; os != "" && os != "linux" {
		return fmt.Errorf("Racer requires Linux nodes")
	}
	spec.NodeSelector["kubernetes.io/os"] = "linux"
	// Required nodeSelectorTerms are ORed. Add the exclusion check to EVERY term
	// while preserving MatchFields and all pre-existing constraints.
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	createdRequired := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil
	if createdRequired {
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}}
	}
	required := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	for i := range required.NodeSelectorTerms {
		term := &required.NodeSelectorTerms[i]
		// Empty terms previously matched no nodes. Keep them empty rather than
		// accidentally broadening a pre-existing unschedulable selector.
		if !createdRequired && len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue
		}
		term.MatchExpressions = append(term.MatchExpressions, corev1.NodeSelectorRequirement{Key: "racer.unbounded-cloud.io/exclude", Operator: corev1.NodeSelectorOpDoesNotExist})
	}
	return nil
}
