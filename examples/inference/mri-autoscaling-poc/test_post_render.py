# Copyright (c) KAITO authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Offline POC post-render tests; no Kubernetes cluster or Helm required."""

import copy
import importlib.util
import subprocess
import sys
import unittest
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
SPEC = importlib.util.spec_from_file_location("post_render", HERE / "post-render.py")
POST_RENDER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(POST_RENDER)


def replica_schema(crd):
    return crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"][
        "spec"
    ]["properties"]["roles"]["items"]["properties"]["replicas"]


class PostRenderTest(unittest.TestCase):
    def setUp(self):
        chart = (
            ROOT / "charts/kaito/workspace/templates/kaito.sh_multiroleinferences.yaml"
        )
        # This CRD's only Helm constructs are the outer feature-gate lines.
        lines = chart.read_text().splitlines()
        self.assertEqual(
            lines[0],
            "{{- if .Values.featureGates.enableMultiRoleInferenceController -}}",
        )
        self.assertEqual(lines[-1], "{{- end -}}")
        self.chart = yaml.safe_load("\n".join(lines[1:-1]))
        self.base = yaml.safe_load(
            (ROOT / "config/crd/bases/kaito.sh_multiroleinferences.yaml").read_text()
        )

    def test_only_replica_default_changes_and_matches_base(self):
        other = {"apiVersion": "v1", "kind": "ConfigMap", "data": {"key": "value"}}
        before = [self.chart, other, None]
        expected = copy.deepcopy(before)
        self.assertEqual(replica_schema(expected[0]).pop("default"), 1)
        result = POST_RENDER.preserve_mri_replicas(copy.deepcopy(before))
        self.assertEqual(result, expected)
        # Description drift predates this POC; compare the effective replica schema.
        actual_schema = dict(replica_schema(result[0]))
        base_schema = dict(replica_schema(self.base))
        actual_schema.pop("description")
        base_schema.pop("description")
        self.assertEqual(actual_schema, base_schema)
        self.assertEqual(POST_RENDER.preserve_mri_replicas(result), expected)
        self.assertEqual(replica_schema(self.chart)["default"], 1)

    def test_all_versions_are_adjusted(self):
        version = copy.deepcopy(self.chart["spec"]["versions"][0])
        version["name"] = "v1alpha2"
        self.chart["spec"]["versions"].append(version)
        POST_RENDER.preserve_mri_replicas([self.chart])
        for version in self.chart["spec"]["versions"]:
            crd = {"spec": {"versions": [version]}}
            self.assertNotIn("default", replica_schema(crd))

    def test_cli_stream_and_failure(self):
        result = subprocess.run(
            [sys.executable, str(HERE / "post-render.py")],
            input=yaml.safe_dump_all([self.chart, {"kind": "ConfigMap"}]),
            text=True,
            capture_output=True,
            check=True,
        )
        documents = list(yaml.safe_load_all(result.stdout))
        self.assertNotIn("default", replica_schema(documents[0]))
        self.assertEqual(documents[1], {"kind": "ConfigMap"})
        failure = subprocess.run(
            [sys.executable, str(HERE / "post-render.py")],
            input="kind: ConfigMap\n",
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(failure.returncode, 1)
        self.assertEqual(failure.stdout, "")
        self.assertIn("expected exactly one MRI CRD", failure.stderr)

    def test_fail_closed(self):
        for documents in ([], [self.chart, self.chart]):
            with self.assertRaises(ValueError):
                POST_RENDER.preserve_mri_replicas(documents)
        replica_schema(self.chart)["default"] = 2
        with self.assertRaises(ValueError):
            POST_RENDER.preserve_mri_replicas([self.chart])


if __name__ == "__main__":
    unittest.main()
