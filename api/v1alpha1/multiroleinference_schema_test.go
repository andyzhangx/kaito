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

package v1alpha1

import (
	"bytes"
	"os"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"sigs.k8s.io/yaml"
)

// The MRI chart CRD uses only a Go-template feature-gate wrapper. Render that
// exact template and exercise Kubernetes' structural defaulting implementation,
// rather than merely searching YAML text for an absent default.
func TestMultiRoleInferenceReplicaSchemaDefaulting(t *testing.T) {
	var baseReplicas *apiextensionsv1.JSONSchemaProps
	for _, path := range []string{
		"../../config/crd/bases/kaito.sh_multiroleinferences.yaml",
		"../../charts/kaito/workspace/templates/kaito.sh_multiroleinferences.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			tmpl, err := template.New("mri-crd").Parse(string(data))
			require.NoError(t, err)
			var rendered bytes.Buffer
			require.NoError(t, tmpl.Execute(&rendered, map[string]interface{}{
				"Values": map[string]interface{}{"featureGates": map[string]interface{}{"enableMultiRoleInferenceController": true}},
			}))
			crd := &apiextensionsv1.CustomResourceDefinition{}
			require.NoError(t, yaml.Unmarshal(rendered.Bytes(), crd))
			require.Equal(t, "multiroleinferences.kaito.sh", crd.Name)
			require.NotEmpty(t, crd.Spec.Versions)
			for _, version := range crd.Spec.Versions {
				t.Run(version.Name, func(t *testing.T) {
					role := version.Schema.OpenAPIV3Schema.Properties["spec"].Properties["roles"].Items.Schema
					replicas := role.Properties["replicas"]
					require.Nil(t, replicas.Default)
					assert.NotContains(t, role.Required, "replicas")
					require.NotNil(t, replicas.Minimum)
					assert.Equal(t, float64(1), *replicas.Minimum)
					if baseReplicas == nil {
						baseReplicas = replicas.DeepCopy()
					} else {
						assert.Equal(t, baseReplicas, &replicas)
					}
					internal := &apiextensions.JSONSchemaProps{}
					require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, internal, nil))
					schema, err := structuralschema.NewStructural(internal)
					require.NoError(t, err)
					for _, tc := range []struct {
						name string
						role map[string]interface{}
						want map[string]interface{}
					}{
						{name: "omitted remains absent", role: map[string]interface{}{"type": "prefill"}, want: map[string]interface{}{"type": "prefill"}},
						{name: "null is pruned not defaulted", role: map[string]interface{}{"type": "decode", "replicas": nil}, want: map[string]interface{}{"type": "decode"}},
						{name: "stored fixed count unchanged", role: map[string]interface{}{"type": "decode", "replicas": int64(3)}, want: map[string]interface{}{"type": "decode", "replicas": int64(3)}},
					} {
						t.Run(tc.name, func(t *testing.T) {
							object := map[string]interface{}{"spec": map[string]interface{}{"roles": []interface{}{tc.role}}}
							structuraldefaulting.PruneNonNullableNullsWithoutDefaults(object, schema)
							structuraldefaulting.Default(object, schema)
							structuraldefaulting.Default(object, schema)
							assert.Equal(t, tc.want, tc.role)
						})
					}
				})
			}
		})
	}
}
