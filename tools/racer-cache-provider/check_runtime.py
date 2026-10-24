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

"""Check the actual installed wheel pair and dlopen the provider (no GPU needed).
This structural check does not establish end-to-end Racer/model correctness.
An optional first argument overrides the provider path for local diagnostics.
"""
import ctypes
import importlib.metadata
import sys
from pathlib import Path


def main():
    library = sys.argv[1] if len(sys.argv) > 1 else '/opt/kaito/racer/libkaito_racer_cache.so'
    names = ('runai-model-streamer', 'runai-model-streamer-azure')
    try:
        distributions = {name: importlib.metadata.distribution(name) for name in names}
    except importlib.metadata.PackageNotFoundError as error:
        raise SystemExit('Core and Azure Model Streamer must be installed in the KAITO base') from error
    versions = {name: dist.version for name, dist in distributions.items()}
    if any(version != '0.16.0' for version in versions.values()):
        raise SystemExit(f'POC requires the audited core/Azure 0.16.0 wheel pair: {versions}')
    azure = distributions['runai-model-streamer-azure']
    matches = [Path(azure.locate_file(f)) for f in (azure.files or [])
               if str(f).endswith('libstreamerazure.so')]
    markers = [b'RUNAI_STREAMER_EXPERIMENTAL_AZURE_CACHE_ENABLED',
               b'RUNAI_STREAMER_EXPERIMENTAL_AZURE_CACHE_LIB',
               b'cache_abi_version', b'blob_read']
    if not matches or not any(all(marker in path.read_bytes() for marker in markers)
                              for path in matches):
        raise SystemExit('Installed Azure streamer lacks the required experimental provider loader')
    provider = ctypes.CDLL(library)
    provider.cache_abi_version.restype = ctypes.c_uint32
    if provider.cache_abi_version() != 1:
        raise SystemExit('Expected provider ABI 1')
    getattr(provider, 'blob_read')
    getattr(provider, 'shutdown')
    print('Provider ABI 1 loaded; core/Azure 0.16.0 pair and native loader markers verified.')
    print('This does not validate vLLM/GPU loading or Racer connectivity.')


if __name__ == '__main__':
    main()
