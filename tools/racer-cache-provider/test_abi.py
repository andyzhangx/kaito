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
"""Exercise the compiled C ABI with a local mock; no GPU/cloud credentials.
Usage: python3 test_abi.py /absolute/path/libkaito_racer_cache.so
"""
import ctypes
import http.server
import os
import socket
import sys
import threading
import unittest
from concurrent.futures import ThreadPoolExecutor


class Handler(http.server.BaseHTTPRequestHandler):
    requests = []

    def do_GET(self):
        Handler.requests.append((self.path, dict(self.headers)))
        start, end = map(int, self.headers['Range'][6:].split('-'))
        data = bytes(i % 251 for i in range(start, end + 1))
        if self.path.endswith('/forbidden'):
            self.send_response(403)
            self.end_headers()
            return
        self.send_response(206)
        self.send_header('Content-Range', f'bytes {start}-{end}/1000000000')
        if self.path.endswith('/short'):
            data = data[:-1]
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass


class ABI(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        port = int(os.environ['KAITO_RACER_ENDPOINT'].rsplit(':', 1)[1])
        cls.server = http.server.ThreadingHTTPServer(('127.0.0.1', port), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.lib = ctypes.CDLL(LIBRARY)
        cls.lib.cache_abi_version.restype = ctypes.c_uint32
        cls.lib.blob_read.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_char_p,
                                     ctypes.c_void_p, ctypes.c_size_t, ctypes.c_size_t,
                                     ctypes.c_void_p, ctypes.c_size_t]
        cls.lib.blob_read.restype = ctypes.c_ssize_t

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def read(self, name=b'weights', start=0, size=32, account=b'account123', error_size=256):
        buf = ctypes.create_string_buffer(size)
        error = ctypes.create_string_buffer(error_size)
        n = self.lib.blob_read(account, b'models-test', name, buf, start, size, error, error_size)
        return n, buf.raw, error.value

    def test_abi_version(self):
        self.assertEqual(self.lib.cache_abi_version(), 1)

    def test_success_and_range(self):
        for start, size in [(0, 8), (16*1024*1024-2, 8), (4096, 8192)]:
            n, data, error = self.read(start=start, size=size)
            self.assertEqual(n, size, error)
            self.assertEqual(data, bytes(i % 251 for i in range(start, start+size)))
            self.assertEqual(error, b'')

    def test_refuse_account(self):
        count = len(Handler.requests)
        n, _, error = self.read(account=b'otheraccount')
        self.assertEqual(n, -1)
        self.assertIn(b'not authorized', error)
        self.assertEqual(len(Handler.requests), count)

    def test_null_args_and_bounded_error(self):
        error = ctypes.create_string_buffer(4)
        n = self.lib.blob_read(None, b'models-test', b'a', None, 0, 1, error, 4)
        self.assertEqual(n, -1)
        self.assertEqual(error.raw[-1], 0)
        self.assertEqual(self.lib.blob_read(None, None, None, None, 0, 1, None, 0), -1)
        self.assertEqual(self.lib.blob_read(b'account123', b'models-test', b'a', None, 0, 1, error, 4), -1)

    def test_overflow_and_zero(self):
        error = ctypes.create_string_buffer(256)
        buf = ctypes.create_string_buffer(2)
        self.assertEqual(self.lib.blob_read(b'account123', b'models-test', b'a', buf,
                                         2**64-1, 2, error, 256), -1)
        self.assertEqual(self.lib.blob_read(b'account123', b'models-test', b'a', None,
                                         0, 0, error, 256), 0)

    def test_no_partial_success_or_auth_retry(self):
        for name in [b'short', b'forbidden']:
            count = len(Handler.requests)
            self.assertEqual(self.read(name=name)[0], -1)
            self.assertEqual(len(Handler.requests), count+1)

    def test_concurrent_c_callers(self):
        with ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(lambda _: self.read(), range(32)))
        self.assertTrue(all(n == 32 and not error for n, _, error in results))

    def test_shutdown_and_reload(self):
        self.lib.shutdown.argtypes = []
        self.lib.shutdown.restype = None
        self.assertEqual(self.read()[0], 32)
        self.lib.shutdown()
        self.lib.shutdown()
        self.assertEqual(self.read()[0], 32)

    def test_no_credentials(self):
        self.read()
        _, headers = Handler.requests[-1]
        self.assertNotIn('Authorization', headers)
        self.assertNotIn('Cookie', headers)


if __name__ == '__main__':
    if '--child' not in sys.argv:
        if len(sys.argv) != 2:
            raise SystemExit(__doc__)
        # Go c-shared snapshots process-start environment. Re-exec with the
        # configuration set before startup, as it is in Kubernetes containers.
        with socket.socket() as probe:
            probe.bind(('127.0.0.1', 0))
            port = probe.getsockname()[1]
        env = dict(os.environ, KAITO_RACER_ENDPOINT=f'http://127.0.0.1:{port}',
                   KAITO_RACER_ACCOUNT='account123', KAITO_RACER_CONTAINER='models-test',
                   KAITO_RACER_MAX_CONCURRENCY='4', KAITO_RACER_MAX_ATTEMPTS='3',
                   KAITO_RACER_TIMEOUT_MS='5000')
        os.execve(sys.executable, [sys.executable, *sys.argv, '--child'], env)
    sys.argv.remove('--child')
    LIBRARY = os.path.abspath(sys.argv.pop())
    unittest.main(verbosity=2)
