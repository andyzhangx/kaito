#!/usr/bin/env python3
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

"""Lab-only Helm post-renderer: preserve omitted MRI role replica counts.

Requires PyYAML. This changes a cluster-scoped CRD: use only in a disposable
cluster where every fixed MRI role explicitly specifies a positive replica count.
"""

import sys

import yaml


def preserve_mri_replicas(documents):
    """Remove only the role replica default; fail closed on unexpected schemas."""
    matches = [
        doc
        for doc in documents
        if isinstance(doc, dict)
        and doc.get("kind") == "CustomResourceDefinition"
        and doc.get("metadata", {}).get("name") == "multiroleinferences.kaito.sh"
    ]
    if len(matches) != 1:
        raise ValueError("expected exactly one MRI CRD; enable the MRI feature gate")
    versions = matches[0]["spec"]["versions"]
    if not versions:
        raise ValueError("MRI CRD has no versions")
    for version in versions:
        role = version["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"][
            "roles"
        ]["items"]
        replicas = role["properties"]["replicas"]
        if (
            replicas.get("type") != "integer"
            or replicas.get("format") != "int32"
            or replicas.get("minimum") != 1
            or "replicas" in role.get("required", [])
            or ("default" in replicas and replicas["default"] != 1)
        ):
            raise ValueError(
                "unexpected MRI replica schema; review before using this POC"
            )
        replicas.pop("default", None)
    return documents


def main():
    try:
        documents = preserve_mri_replicas(list(yaml.safe_load_all(sys.stdin)))
    except (KeyError, TypeError, ValueError, yaml.YAMLError) as error:
        print(f"MRI autoscaling POC post-render failed: {error}", file=sys.stderr)
        return 1
    yaml.safe_dump_all(documents, sys.stdout, sort_keys=False)
    return 0


if __name__ == "__main__":
    sys.exit(main())
