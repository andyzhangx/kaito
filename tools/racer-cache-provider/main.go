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

package main

/*
#include <stdint.h>
#include <stddef.h>
#include <sys/types.h>
#include <string.h>
*/
import "C"

import (
	"errors"
	"os"
	"unsafe"
)

//export cache_abi_version
func cache_abi_version() C.uint32_t { return 1 }

//export blob_read
func blob_read(account, container, blob *C.char, buf unsafe.Pointer, offset, length C.size_t, errorBuf *C.char, errorBufSize C.size_t) (result C.ssize_t) {
	result = -1
	fail := func(message string) {
		if errorBuf == nil || errorBufSize == 0 {
			return
		}
		// Never expose URLs, credentials, response bodies or upstream errors.
		n := len(message)
		if uint64(n) >= uint64(errorBufSize) {
			n = int(errorBufSize) - 1
		}
		out := unsafe.Slice((*byte)(unsafe.Pointer(errorBuf)), n+1)
		copy(out, message[:n])
		out[n] = 0
	}
	defer func() {
		if recover() != nil {
			result = -1
			fail("Racer provider internal failure")
		}
	}()
	fail("")
	boundedString := func(p *C.char, max int) (string, error) {
		if p == nil {
			return "", errors.New("null string argument")
		}
		n := C.strnlen(p, C.size_t(max+1))
		if int(n) > max {
			return "", errors.New("string argument too long")
		}
		return C.GoStringN(p, C.int(n)), nil
	}
	a, e := boundedString(account, 24)
	if e != nil {
		fail(e.Error())
		return
	}
	c, e := boundedString(container, 63)
	if e != nil {
		fail(e.Error())
		return
	}
	b, e := boundedString(blob, 1024)
	if e != nil {
		fail(e.Error())
		return
	}
	maxInt := uint64(^uint(0) >> 1)
	if uint64(length) > maxInt || (length != 0 && buf == nil) || uint64(offset) > maxInt || (length > 0 && uint64(length)-1 > maxInt-uint64(offset)) {
		fail("invalid buffer or range")
		return
	}
	processProvider.Do(func() { processProvider.p, processProvider.err = newProvider(os.Getenv) })
	if processProvider.err != nil {
		fail(processProvider.err.Error())
		return
	}
	dst := unsafe.Slice((*byte)(buf), int(length))
	if err := processProvider.p.read(a, c, b, uint64(offset), dst); err != nil {
		fail(err.Error())
		return
	}
	return C.ssize_t(length)
}

//export shutdown
func shutdown() {
	// Export the exact optional ABI symbol: without it, dlsym may resolve the
	// unrelated POSIX shutdown(int, int) in libc. The upstream loader can release
	// and recreate handles without dlclose, so keep the process configuration and
	// transport usable by later reads; closing idle connections is concurrency-safe.
	processProvider.Do(func() { processProvider.p, processProvider.err = newProvider(os.Getenv) })
	if processProvider.p != nil {
		processProvider.p.client.CloseIdleConnections()
	}
}

func main() {}
