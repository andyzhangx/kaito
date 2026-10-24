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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kaito-project/kaito/pkg/utils/generator"
)

func bindingFixture() RacerBinding {
	return RacerBinding{Namespace: "team-a", Account: "account123", Container: "models-test", CacheName: "team-a-models", ObjectImage: "example.invalid/racer-object:v0.18.0", RuntimeImage: "example.invalid/kaito-racer:poc", Immutable: true}
}
func installBinding(t *testing.T) {
	t.Helper()
	old := RacerBindings
	RacerBindings = []RacerBinding{bindingFixture()}
	t.Cleanup(func() { RacerBindings = old })
}
func racerFixture() (*generator.WorkspaceGeneratorContext, *corev1.PodSpec, *StreamingConfig) {
	ws := &v1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "team-a", Annotations: map[string]string{AnnotationModelCache: "racer"}}}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "original:v1", Command: []string{"python3", "inference_api.py"}, Env: []corev1.EnvVar{{Name: "KEEP", Value: "yes"}, {Name: "KAITO_RACER_ENDPOINT", Value: "http://evil:80"}}}}, NodeSelector: map[string]string{"gpu": "true"}}
	cfg := &StreamingConfig{ModelPath: "az://models-test/org/model", ProviderEnvVars: []corev1.EnvVar{{Name: "AZURE_STORAGE_ACCOUNT_NAME", Value: "account123"}}}
	return &generator.WorkspaceGeneratorContext{Workspace: ws}, spec, cfg
}

func TestParseRacerBindings(t *testing.T) {
	good := bindingFixture()
	data, _ := json.Marshal([]RacerBinding{good})
	got, err := ParseRacerBindings(string(data))
	require.NoError(t, err)
	require.Equal(t, []RacerBinding{good}, got)
	for _, raw := range []string{"", "[]", "null"} {
		b, err := ParseRacerBindings(raw)
		require.NoError(t, err)
		require.Empty(t, b)
	}
	for _, mutate := range []func(*RacerBinding){func(b *RacerBinding) { b.Namespace = "../x" }, func(b *RacerBinding) { b.CacheName = "../cache" }, func(b *RacerBinding) { b.CacheName = strings.Repeat("a", 41) + "." + strings.Repeat("b", 41) }, func(b *RacerBinding) { b.Account = "UPPER" }, func(b *RacerBinding) { b.Container = "a--b" }, func(b *RacerBinding) { b.Immutable = false }, func(b *RacerBinding) { b.ObjectImage = "image:latest" }, func(b *RacerBinding) { b.RuntimeImage = "registry:5000/image" }} {
		b := good
		mutate(&b)
		raw, _ := json.Marshal([]RacerBinding{b})
		_, err := ParseRacerBindings(string(raw))
		require.Error(t, err)
	}
	for _, raw := range []string{`[{"typo":true}]`, string(data) + ` []`, `{}`} {
		_, err := ParseRacerBindings(raw)
		require.Error(t, err)
	}
	raw, _ := json.Marshal([]RacerBinding{good, good})
	_, err = ParseRacerBindings(string(raw))
	require.Error(t, err)
	other := good
	other.Account = "anotheraccount"
	raw, _ = json.Marshal([]RacerBinding{good, other})
	_, err = ParseRacerBindings(string(raw))
	require.Error(t, err)
}

func TestRacerOptInValidation(t *testing.T) {
	ctx, _, _ := racerFixture()
	require.Error(t, ValidateRacerRequest(ctx.Workspace, false))
	require.NoError(t, ValidateRacerRequest(ctx.Workspace, true))
	ctx.Workspace.Annotations[AnnotationModelCache] = "typo"
	require.Error(t, ValidateRacerRequest(ctx.Workspace, true))
	for _, mode := range []string{"", "disabled"} {
		ctx.Workspace.Annotations[AnnotationModelCache] = mode
		require.NoError(t, ValidateRacerRequest(ctx.Workspace, false))
	}
}

func TestRacerStreamingPod(t *testing.T) {
	installBinding(t)
	ctx, spec, cfg := racerFixture()
	require.NoError(t, SetStreamingConfig(cfg, "org/model", "streaming-sa")(ctx, spec))
	require.Equal(t, "streaming-sa", spec.ServiceAccountName)
	require.Equal(t, bindingFixture().RuntimeImage, spec.Containers[0].Image)
	require.Equal(t, []string{"python3", "inference_api.py"}, spec.Containers[0].Command)
	require.Empty(t, spec.Containers[0].VolumeMounts, "application must not receive cache socket")
	env := map[string]string{}
	for _, e := range spec.Containers[0].Env {
		_, exists := env[e.Name]
		require.False(t, exists, "duplicate %s", e.Name)
		env[e.Name] = e.Value
	}
	require.Equal(t, "yes", env["KEEP"])
	require.Equal(t, "account123", env["AZURE_STORAGE_ACCOUNT_NAME"])
	require.Equal(t, "1", env["RUNAI_STREAMER_EXPERIMENTAL_AZURE_CACHE_ENABLED"])
	require.Equal(t, "http://127.0.0.1:18080", env["KAITO_RACER_ENDPOINT"])
	require.Len(t, spec.InitContainers, 1)
	sidecar := spec.InitContainers[0]
	require.Equal(t, "racer-object", sidecar.Name)
	require.Equal(t, corev1.ContainerRestartPolicyAlways, *sidecar.RestartPolicy)
	require.Contains(t, sidecar.Args, "--api=azure")
	require.Contains(t, sidecar.Args, "--cache=team-a-models")
	require.Empty(t, sidecar.Env)
	require.Nil(t, sidecar.ReadinessProbe)
	require.Equal(t, int64(65532), *sidecar.SecurityContext.RunAsUser)
	require.False(t, *sidecar.SecurityContext.AllowPrivilegeEscalation)
	require.Len(t, spec.Volumes, 1)
	require.Equal(t, "/run/racer/team-a-models/client", spec.Volumes[0].HostPath.Path)
	require.Equal(t, corev1.HostPathDirectory, *spec.Volumes[0].HostPath.Type)
	require.Equal(t, "true", spec.NodeSelector["gpu"])
	require.Equal(t, "linux", spec.NodeSelector["kubernetes.io/os"])
	require.Equal(t, "racer.unbounded-cloud.io/exclude", spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Key)
	// Generated specs must be deterministic to avoid perpetual rollouts.
	ctx2, spec2, cfg2 := racerFixture()
	require.NoError(t, SetStreamingConfig(cfg2, "org/model", "streaming-sa")(ctx2, spec2))
	require.Equal(t, spec, spec2)
}

func TestRacerDisabledPreservesOriginalPath(t *testing.T) {
	installBinding(t)
	for _, mode := range []string{"", "disabled"} {
		ctx, spec, cfg := racerFixture()
		ctx.Workspace.Annotations[AnnotationModelCache] = mode
		require.NoError(t, SetStreamingConfig(cfg, "org/model", "sa")(ctx, spec))
		require.Equal(t, "original:v1", spec.Containers[0].Image)
		require.Empty(t, spec.InitContainers)
		require.Empty(t, spec.Volumes)
	}
}

func TestRacerRejectsUnsupportedOrUnauthorized(t *testing.T) {
	installBinding(t)
	cases := map[string]func(*generator.WorkspaceGeneratorContext, *corev1.PodSpec, *StreamingConfig){
		"namespace": func(ctx *generator.WorkspaceGeneratorContext, _ *corev1.PodSpec, _ *StreamingConfig) {
			ctx.Workspace.Namespace = "team-b"
		},
		"account": func(_ *generator.WorkspaceGeneratorContext, _ *corev1.PodSpec, cfg *StreamingConfig) {
			cfg.ProviderEnvVars[0].Value = "otheraccount"
		},
		"container": func(_ *generator.WorkspaceGeneratorContext, _ *corev1.PodSpec, cfg *StreamingConfig) {
			cfg.ModelPath = "az://other-container/model"
		},
		"sas": func(_ *generator.WorkspaceGeneratorContext, _ *corev1.PodSpec, cfg *StreamingConfig) {
			cfg.InitContainers = []corev1.Container{{Name: "fetch-sas"}}
		},
		"query": func(_ *generator.WorkspaceGeneratorContext, _ *corev1.PodSpec, cfg *StreamingConfig) {
			cfg.ModelPath += "?sig=secret"
		},
		"endpoint": func(_ *generator.WorkspaceGeneratorContext, _ *corev1.PodSpec, cfg *StreamingConfig) {
			cfg.ProviderEnvVars = append(cfg.ProviderEnvVars, corev1.EnvVar{Name: "AZURE_STORAGE_ENDPOINT_SUFFIX", Value: "blob.core.usgovcloudapi.net"})
		},
		"hostNetwork": func(_ *generator.WorkspaceGeneratorContext, s *corev1.PodSpec, _ *StreamingConfig) {
			s.HostNetwork = true
		},
		"duplicate": func(_ *generator.WorkspaceGeneratorContext, s *corev1.PodSpec, _ *StreamingConfig) {
			s.InitContainers = append(s.InitContainers, corev1.Container{Name: "racer-object"})
		},
		"windows": func(_ *generator.WorkspaceGeneratorContext, s *corev1.PodSpec, _ *StreamingConfig) {
			s.NodeSelector["kubernetes.io/os"] = "windows"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, spec, cfg := racerFixture()
			mutate(ctx, spec, cfg)
			require.Error(t, SetStreamingConfig(cfg, "org/model", "sa")(ctx, spec))
		})
	}
}

func TestRacerPreservesRequiredAffinity(t *testing.T) {
	installBinding(t)
	for _, terms := range [][]corev1.NodeSelectorTerm{nil, {{}}, {{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "pool", Operator: corev1.NodeSelectorOpIn, Values: []string{"gpu"}}}}, {MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a"}}}}, {}}} {
		ctx, spec, cfg := racerFixture()
		spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms}}}
		before := spec.DeepCopy()
		require.NoError(t, SetStreamingConfig(cfg, "org/model", "sa")(ctx, spec))
		result := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		require.Len(t, result, len(terms))
		for i, old := range before.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if len(old.MatchFields) == 0 && len(old.MatchExpressions) == 0 {
				require.Empty(t, result[i].MatchExpressions)
			} else {
				require.Equal(t, old.MatchFields, result[i].MatchFields)
				require.EqualValues(t, len(old.MatchExpressions)+1, len(result[i].MatchExpressions))
				for j := range old.MatchExpressions {
					require.Equal(t, old.MatchExpressions[j], result[i].MatchExpressions[j])
				}
				require.Equal(t, "racer.unbounded-cloud.io/exclude", result[i].MatchExpressions[len(result[i].MatchExpressions)-1].Key)
			}
		}
	}
}
