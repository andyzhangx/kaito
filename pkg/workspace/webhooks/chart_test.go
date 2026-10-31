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

package webhooks

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"knative.dev/pkg/webhook/resourcesemantics"
)

// Check the chart before the admission controller can reconcile its rules. A
// live-cluster check after controller startup would miss #2495: knative repairs
// the missing v1beta1 rule, hiding the install/upgrade regression. Equivalent
// matching is not sufficient because it can route beta requests to alpha checks.
func TestChartValidationRulesMatchHandlers(t *testing.T) {
	source, err := os.ReadFile("../../../charts/kaito/workspace/templates/webhooks.yaml")
	require.NoError(t, err)

	// Render this standalone template without requiring Helm in the unit-test
	// environment. Only presentation helpers are stubbed; feature gates and all
	// admission rules come from the actual chart template, unchanged.
	tmpl, err := template.New("webhooks.yaml").Option("missingkey=error").Funcs(template.FuncMap{
		"include": func(name string, _ interface{}) string {
			switch name {
			case "kaito.labels":
				return "app.kubernetes.io/name: workspace"
			case "kaito.serviceName":
				return "kaito-workspace"
			default:
				t.Fatalf("unexpected chart helper %q", name)
				return ""
			}
		},
		"nindent": func(spaces int, value string) string {
			indent := strings.Repeat(" ", spaces)
			return "\n" + indent + strings.ReplaceAll(value, "\n", "\n"+indent)
		},
	}).Parse(string(source))
	require.NoError(t, err)

	// Exercise each gate independently as well as all enabled/disabled, so an
	// optional webhook cannot accidentally depend on a different feature gate.
	for mask := 0; mask < 8; mask++ {
		gates := map[string]bool{
			"enableInferenceSetController":       mask&1 != 0,
			"enableMultiRoleInferenceController": mask&2 != 0,
			"ModelMirror":                        mask&4 != 0,
		}
		t.Run(strings.Join([]string{
			gateTestName("InferenceSet", gates["enableInferenceSetController"]),
			gateTestName("MultiRoleInference", gates["enableMultiRoleInferenceController"]),
			gateTestName("ModelMirror", gates["ModelMirror"]),
		}, "/"), func(t *testing.T) {
			var rendered bytes.Buffer
			require.NoError(t, tmpl.Execute(&rendered, map[string]interface{}{
				"Release": map[string]interface{}{"Namespace": "kaito-workspace"},
				"Values": map[string]interface{}{
					"featureGates": gates,
					"webhook":      map[string]interface{}{"port": 9443},
				},
			}))

			configs := map[string]admissionregistrationv1.ValidatingWebhookConfiguration{}
			decoder := yaml.NewYAMLOrJSONDecoder(&rendered, 4096)
			for {
				var config admissionregistrationv1.ValidatingWebhookConfiguration
				err := decoder.Decode(&config)
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				require.NotEmpty(t, config.Name)
				require.NotContains(t, configs, config.Name)
				configs[config.Name] = config
			}

			for _, resource := range []struct {
				name     string
				plural   string
				enabled  bool
				handlers map[schema.GroupVersionKind]resourcesemantics.GenericCRD
			}{
				{"workspace", "workspaces", true, WorkspaceResources},
				{"inferenceset", "inferencesets", gates["enableInferenceSetController"], InferenceSetResources},
				{"multiroleinference", "multiroleinferences", gates["enableMultiRoleInferenceController"], MultiRoleInferenceResources},
				{"modelmirror", "modelmirrors", gates["ModelMirror"], ModelMirrorResources},
			} {
				name := "validation." + resource.name + ".kaito.sh"
				config, present := configs[name]
				require.Equal(t, resource.enabled, present, name)
				if !present {
					continue
				}
				delete(configs, name)
				require.Len(t, config.Webhooks, 1, name)
				webhook := config.Webhooks[0]
				assert.Equal(t, name, webhook.Name)
				require.Len(t, webhook.Rules, 1, name)
				rule := webhook.Rules[0]
				assert.Equal(t, []string{resource.plural}, rule.Resources, name)
				versions := make([]string, 0, len(resource.handlers))
				for gvk := range resource.handlers {
					assert.Equal(t, []string{gvk.Group}, rule.APIGroups, name)
					versions = append(versions, gvk.Version)
				}
				assert.ElementsMatch(t, versions, rule.APIVersions, name)
				assert.ElementsMatch(t, []admissionregistrationv1.OperationType{
					admissionregistrationv1.Create, admissionregistrationv1.Update,
				}, rule.Operations, name)
			}
			assert.Empty(t, configs, "unexpected validation webhook configurations")
		})
	}
}

func gateTestName(name string, enabled bool) string {
	if enabled {
		return name + "=on"
	}
	return name + "=off"
}
