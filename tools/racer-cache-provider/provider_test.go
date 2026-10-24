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

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func config(endpoint string) map[string]string {
	return map[string]string{"KAITO_RACER_ENDPOINT": endpoint, "KAITO_RACER_ACCOUNT": "account123", "KAITO_RACER_CONTAINER": "models-test", "KAITO_RACER_MAX_ATTEMPTS": "3"}
}
func testProvider(t *testing.T, endpoint string, overrides map[string]string) *provider {
	t.Helper()
	values := config(endpoint)
	for k, v := range overrides {
		values[k] = v
	}
	p, err := newProvider(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.client.CloseIdleConnections)
	return p
}
func reply(w http.ResponseWriter, start int, data []byte) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+len(data)-1, start+len(data)+100))
	w.WriteHeader(206)
	_, _ = w.Write(data)
}

func TestConfiguration(t *testing.T) {
	for _, address := range []string{"https://127.0.0.1:80", "http://localhost:80", "http://169.254.169.254:80", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user:secret@127.0.0.1:80", "http://127.0.0.1:80/a", "http://127.0.0.1:80?token=x", "http://127.0.0.1:80?", "http://127.0.0.1:80#secret", "http://127.0.0.1:80/%2e"} {
		t.Run(address, func(t *testing.T) {
			v := config(address)
			if _, err := newProvider(func(k string) string { return v[k] }); err == nil {
				t.Fatal("accepted unsafe endpoint")
			}
		})
	}
	for _, kv := range [][2]string{{"KAITO_RACER_ACCOUNT", ""}, {"KAITO_RACER_CONTAINER", "../x"}, {"KAITO_RACER_TIMEOUT_MS", "0"}, {"KAITO_RACER_MAX_ATTEMPTS", "6"}, {"KAITO_RACER_MAX_CONCURRENCY", "65"}} {
		v := config("http://127.0.0.1:18080")
		v[kv[0]] = kv[1]
		if _, err := newProvider(func(k string) string { return v[k] }); err == nil {
			t.Fatalf("accepted %v", kv)
		}
	}
	for _, address := range []string{"http://127.0.0.1:80/", "http://[::1]:8080"} {
		testProvider(t, address, nil)
	}
}

func TestRangeAndEscaping(t *testing.T) {
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.URL.EscapedPath() != "/models-test/model/a%20b%3F%23%25%E4%B8%AD.safetensors" {
			t.Errorf("wrong path: %s", r.URL.EscapedPath())
		}
		if r.Header.Get("Range") != "bytes=16777214-16777217" {
			t.Error("wrong range")
		}
		if r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" || r.Header.Get("Cookie") != "" {
			t.Error("credential forwarded")
		}
		reply(w, 16777214, []byte("abcd"))
	}))
	defer server.Close()
	// Proxy environment is deliberately ignored, not used even for loopback.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	p := testProvider(t, server.URL, nil)
	buf := make([]byte, 4)
	if err := p.read("account123", "models-test", "model/a b?#%中.safetensors", 16777214, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "abcd" || count.Load() != 1 {
		t.Fatal("bad result")
	}
}

func TestRejectedRequestsDoNotAccessNetwork(t *testing.T) {
	var count atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { count.Add(1) }))
	defer s.Close()
	p := testProvider(t, s.URL, nil)
	for _, args := range [][3]string{{"wrong", "models-test", "a"}, {"account123", "wrong", "a"}, {"account123", "models-test", "../a"}, {"account123", "models-test", "a/./b"}, {"account123", "models-test", ""}, {"account123", "models-test", strings.Repeat("x", 1025)}} {
		if err := p.read(args[0], args[1], args[2], 0, make([]byte, 4)); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if err := p.read("account123", "models-test", "a", 1<<63-1, make([]byte, 2)); err == nil {
		t.Fatal("overflow accepted")
	}
	if err := p.read("account123", "models-test", "a", 0, nil); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 0 {
		t.Fatal("unexpected network access")
	}
}

func TestBadResponses(t *testing.T) {
	cases := []struct {
		name                   string
		status                 int
		contentRange, encoding string
		body                   string
		chunked                bool
	}{
		{"full200", 200, "", "", "abcd", false}, {"auth", 403, "", "", "denied", false}, {"missing", 404, "", "", "missing", false}, {"redirect", 302, "", "", "", false},
		{"missing-range", 206, "", "", "abcd", false}, {"wrong-start", 206, "bytes 1-4/10", "", "abcd", false}, {"wrong-total", 206, "bytes 0-3/3", "", "abcd", false}, {"wildcard", 206, "bytes 0-3/*", "", "abcd", false},
		{"short", 206, "bytes 0-3/10", "", "abc", false}, {"long", 206, "bytes 0-3/10", "", "abcde", false},
		{"chunked-short", 206, "bytes 0-3/10", "", "abc", true}, {"chunked-long", 206, "bytes 0-3/10", "", "abcde", true},
		{"compressed", 206, "bytes 0-3/10", "gzip", "abcd", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var count atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.Header().Set("Location", "http://169.254.169.254/latest/")
				w.WriteHeader(tc.status)
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			p := testProvider(t, s.URL, nil)
			if err := p.read("account123", "models-test", "a", 0, make([]byte, 4)); err == nil {
				t.Fatal("accepted invalid response")
			}
			if count.Load() != 1 {
				t.Fatalf("retried protocol/auth error: %d", count.Load())
			}
		})
	}
}

func TestRetryTransientAndRetryAfter(t *testing.T) {
	for _, retryAfter := range []bool{false, true} {
		t.Run(fmt.Sprint(retryAfter), func(t *testing.T) {
			var count atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if count.Add(1) == 1 {
					if retryAfter {
						w.Header().Set("Retry-After", "60")
					}
					w.WriteHeader(503)
					return
				}
				reply(w, 0, []byte("abcd"))
			}))
			defer s.Close()
			p := testProvider(t, s.URL, nil)
			err := p.read("account123", "models-test", "a", 0, make([]byte, 4))
			if retryAfter {
				if err == nil || count.Load() != 1 {
					t.Fatal("Retry-After ignored")
				}
			} else if err != nil || count.Load() != 2 {
				t.Fatalf("transient retry failed: %v / %d", err, count.Load())
			}
		})
	}
}

func TestConcurrentReads(t *testing.T) {
	var active, max atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			m := max.Load()
			if n <= m || max.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		reply(w, 0, []byte("abcd"))
	}))
	defer s.Close()
	p := testProvider(t, s.URL, map[string]string{"KAITO_RACER_MAX_CONCURRENCY": "2"})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := make([]byte, 4)
			if err := p.read("account123", "models-test", "a", 0, b); err != nil || !bytes.Equal(b, []byte("abcd")) {
				t.Errorf("read: %v", err)
			}
		}()
	}
	wg.Wait()
	if max.Load() > 2 {
		t.Fatalf("concurrency not bounded: %d", max.Load())
	}
}

func TestDeadlineIncludesQueue(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer s.Close()
	p := testProvider(t, s.URL, map[string]string{"KAITO_RACER_TIMEOUT_MS": "50", "KAITO_RACER_MAX_CONCURRENCY": "1"})
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.read("account123", "models-test", "a", 0, make([]byte, 4)) == nil {
				t.Error("expected timeout")
			}
		}()
	}
	wg.Wait()
	if time.Since(start) > time.Second {
		t.Fatal("deadline not bounded")
	}
}

func TestContentRangeParser(t *testing.T) {
	for _, s := range []string{"bytes +0-3/10", "bytes 0-3/+10", "bytes 0-3/10 garbage", "bytes 0-3/18446744073709551616", "bytes 0-3/10/11", "bytes 0-3/10, bytes 0-3/10"} {
		if validContentRange(s, 0, 3) {
			t.Fatal("accepted", s)
		}
	}
	if !validContentRange("bytes 0-3/10", 0, 3) {
		t.Fatal("rejected valid range")
	}
}
