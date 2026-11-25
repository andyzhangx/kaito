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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Check the actual multi-document example for fields pruned by both shipped
// CRDs. This is an offline unknown-field check, not full schema validation,
// webhook validation or cluster admission coverage.
func TestRacerWorkspaceExampleSchema(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, "examples", "racer-model-streaming-poc.yaml"))
	require.NoError(t, err)
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
	var workspace map[string]interface{}
	for {
		var document map[string]interface{}
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if document["kind"] == "Workspace" {
			require.Nil(t, workspace, "example must contain exactly one Workspace")
			workspace = document
		}
	}
	require.NotNil(t, workspace)
	runtimeAnnotation, found, err := unstructured.NestedString(workspace, "metadata", "annotations", "kaito.sh/runtime")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "vllm", runtimeAnnotation)

	for _, crdPath := range []string{
		"config/crd/bases/kaito.sh_workspaces.yaml",
		"charts/kaito/workspace/templates/kaito.sh_workspaces.yaml",
	} {
		t.Run(crdPath, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, crdPath))
			require.NoError(t, err)
			var crd apiextensionsv1.CustomResourceDefinition
			require.NoError(t, utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096).Decode(&crd))
			var versionSchema *apiextensionsv1.JSONSchemaProps
			for _, version := range crd.Spec.Versions {
				if crd.Spec.Group+"/"+version.Name == workspace["apiVersion"] && version.Served {
					require.NotNil(t, version.Schema)
					versionSchema = version.Schema.OpenAPIV3Schema
				}
			}
			require.NotNil(t, versionSchema, "example API version must be served")
			var schema apiextensions.JSONSchemaProps
			require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(versionSchema, &schema, nil))
			structural, err := structuralschema.NewStructural(&schema)
			require.NoError(t, err)
			options := structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}
			require.Empty(t, pruning.PruneWithOptions(runtime.DeepCopyJSON(workspace), structural, true, options), "example must not rely on fields silently pruned by Kubernetes")

			// Prove the check detects the original regression rather than merely
			// accepting fields that would be silently removed during admission.
			invalid := runtime.DeepCopyJSON(workspace)
			require.NoError(t, unstructured.SetNestedField(invalid, "vllm", "inference", "preset", "runtime"))
			require.Contains(t, pruning.PruneWithOptions(invalid, structural, true, options), "inference.preset.runtime")
		})
	}
}
