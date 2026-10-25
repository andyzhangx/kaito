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

"""Exercise real GNU Make with fake Docker/Go; never build or push images.

Run: MAKE=/path/to/make python3 hack/test-racer-build-targets.py
"""
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MAKE = os.environ.get("MAKE") or shutil.which("make")


class RacerBuildTargets(unittest.TestCase):
    def setUp(self):
        if not MAKE:
            self.fail("GNU Make is required; set MAKE to its executable path")
        self.tmp = tempfile.TemporaryDirectory(prefix="kaito-make-test-")
        self.addCleanup(self.tmp.cleanup)
        self.bin = Path(self.tmp.name)
        self.log = self.bin / "docker.jsonl"
        docker = self.bin / "docker"
        docker.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, sys\n"
            "with open(os.environ['MOCK_DOCKER_LOG'], 'a') as f:\n"
            "    f.write(json.dumps(sys.argv[1:]) + '\\n')\n"
            "if sys.argv[1:] == ['buildx', 'ls']:\n"
            "    print('img-builder')\n"
            "elif sys.argv[1:3] not in (['buildx', 'use'], ['buildx', 'build']):\n"
            "    raise SystemExit('Unexpected Docker operation in mock')\n"
        )
        docker.chmod(0o755)
        go = self.bin / "go"
        go.write_text('#!/bin/sh\nif [ "$2" = GOPATH ]; then echo /tmp/mock-gopath; fi\n')
        go.chmod(0o755)
        self.env = dict(os.environ)
        for key in ("MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS", "MAKELEVEL", "MAKEFILES",
                    "KAITO_BASE_IMG_NAME", "KAITO_BASE_IMG_TAG", "RACER_STREAMING_IMAGE", "IMG_TAG",
                    "IMG_NAME", "BUILD_FLAGS"):
            self.env.pop(key, None)
        self.env.update(PATH=str(self.bin) + os.pathsep + os.environ.get("PATH", ""),
                        MOCK_DOCKER_LOG=str(self.log), REGISTRY="andyzhangx",
                        OUTPUT_TYPE="type=docker", ARCH="amd64", PLATFORMS="linux/amd64",
                        VERSION="0.13.0-racer", BUILDX_BUILDER_NAME="img-builder")

    def run_make(self, target, *args, ok=True):
        result = subprocess.run([MAKE, "--no-print-directory", target, *args], cwd=ROOT,
                                env=self.env, capture_output=True, text=True, timeout=30)
        if ok:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)
        calls = [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []
        return result, calls

    def build_call(self, calls):
        builds = [c for c in calls if c[:2] == ["buildx", "build"]]
        self.assertEqual(len(builds), 1, calls)
        return builds[0]

    def test_base_default_image_and_preset(self):
        _, calls = self.run_make("docker-build-kaito-base", "ARCH=amd64")
        call = self.build_call(calls)
        self.assertIn("--platform=linux/amd64", call)
        self.assertIn("--output=type=docker", call)
        self.assertIn("./docker/presets/models/tfs/Dockerfile", call)
        self.assertIn("VERSION=v0.0.1", call)
        self.assertIn("MODEL_TYPE=text-generation", call)
        self.assertEqual(call[call.index("--tag") + 1], "andyzhangx/kaito-base:v0.0.1")

    def test_runtime_default_images(self):
        _, calls = self.run_make("docker-build-racer-streaming", "ARCH=amd64")
        call = self.build_call(calls)
        self.assertIn("./docker/racer-streaming/Dockerfile", call)
        self.assertIn("KAITO_BASE_IMAGE=andyzhangx/kaito-base:v0.0.1", call)
        self.assertIn("--platform=linux/amd64", call)
        self.assertIn("--output=type=docker", call)
        self.assertIn("--pull", call)
        self.assertEqual(call[call.index("--tag") + 1], "andyzhangx/kaito-vllm-racer:0.13.0-racer")

    def test_overrides_and_registry_output(self):
        base = "registry.example/team/base:custom"
        _, calls = self.run_make("docker-build-racer-streaming", "REGISTRY=registry.example",
                                 "KAITO_BASE_IMG_NAME=team/base", "KAITO_BASE_IMG_TAG=custom",
                                 "RACER_STREAMING_IMAGE=registry.example/racer:custom",
                                 "OUTPUT_TYPE=type=registry", "BUILD_FLAGS=--no-cache --progress=plain")
        call = self.build_call(calls)
        self.assertIn("KAITO_BASE_IMAGE=" + base, call)
        self.assertIn("--output=type=registry", call)
        self.assertIn("--no-cache", call)
        self.assertIn("--progress=plain", call)
        self.assertEqual(call[call.index("--tag") + 1], "registry.example/racer:custom")

    def test_existing_base_overrides_match_runtime_input(self):
        args = ("REGISTRY=registry.example", "KAITO_BASE_IMG_NAME=team/base", "KAITO_BASE_IMG_TAG=custom")
        _, calls = self.run_make("docker-build-kaito-base", *args)
        base_call = self.build_call(calls)
        base = base_call[base_call.index("--tag") + 1]
        self.assertEqual(base, "registry.example/team/base:custom")
        self.assertIn("VERSION=custom", base_call)
        self.log.unlink()
        _, calls = self.run_make("docker-build-racer-streaming", *args)
        runtime_call = self.build_call(calls)
        self.assertIn("KAITO_BASE_IMAGE=" + base, runtime_call)
        self.assertNotIn("./docker/presets/models/tfs/Dockerfile", runtime_call)
        self.assertFalse(any(c[0] == "push" for c in calls))

    def test_existing_workspace_command_unchanged(self):
        _, calls = self.run_make("docker-build-workspace", "ARCH=amd64")
        call = self.build_call(calls)
        self.assertIn("./docker/workspace/Dockerfile", call)
        self.assertIn("--platform=linux/amd64", call)
        self.assertIn("--output=type=docker", call)
        self.assertEqual(call[call.index("--tag") + 1], "andyzhangx/workspace:0.13.0-racer")

    def test_validation_precedes_docker_even_parallel(self):
        cases = [
            ("ARCH=arm64",),
            ("PLATFORMS=linux/arm64",),
            ("PLATFORMS=linux/amd64,linux/arm64",),
            ("REGISTRY=",),
            ("KAITO_BASE_IMG_NAME=",),
            ("KAITO_BASE_IMG_TAG=",),
            ("RACER_STREAMING_IMAGE=",),
        ]
        for target in ("docker-build-racer-streaming",):
            for args in cases:
                with self.subTest(target=target, args=args):
                    _, calls = self.run_make(target, "-j4", *args, ok=False)
                    self.assertEqual(calls, [], "Invalid configuration must not touch Docker")

    def test_output_must_be_tag_and_runtime_must_not_replace_base(self):
        digest = "example.invalid/image@sha256:" + "b" * 64
        for target, args in [
            ("docker-build-racer-streaming", ["RACER_STREAMING_IMAGE=" + digest]),
            ("docker-build-racer-streaming", ["REGISTRY=example.invalid", "KAITO_BASE_IMG_NAME=base", "KAITO_BASE_IMG_TAG=1",
                                             "RACER_STREAMING_IMAGE=example.invalid/base:1"]),
        ]:
            with self.subTest(target=target, args=args):
                _, calls = self.run_make(target, *args, ok=False)
                self.assertEqual(calls, [])

    def test_dry_run_does_not_invoke_docker(self):
        result, calls = self.run_make("docker-build-racer-streaming", "-n")
        self.assertEqual(calls, [])
        self.assertIn("docker buildx build", result.stdout)
        self.assertIn("KAITO_BASE_IMAGE=", result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
