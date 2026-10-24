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
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The ABI has no revision argument. Callers MUST use immutable model blobs for
// the entire model-loading lifetime. This provider never falls back to Azure.
type provider struct {
	endpoint, account, container string
	timeout                      time.Duration
	attempts                     int
	slots                        chan struct{}
	client                       *http.Client
}

func newProvider(getenv func(string) string) (*provider, error) {
	endpoint := getenv("KAITO_RACER_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:18080"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
		return nil, errors.New("KAITO_RACER_ENDPOINT must be a literal loopback HTTP URL without credentials, path or query")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("KAITO_RACER_ENDPOINT requires an explicit port")
	}
	account, container := getenv("KAITO_RACER_ACCOUNT"), getenv("KAITO_RACER_CONTAINER")
	if !validName(account, 3, 24, false) || !validName(container, 3, 63, true) {
		return nil, errors.New("valid KAITO_RACER_ACCOUNT and KAITO_RACER_CONTAINER are required")
	}
	readInt := func(key string, def, max int) (int, error) {
		v := getenv(key)
		if v == "" {
			return def, nil
		}
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > max {
			return 0, fmt.Errorf("invalid %s", key)
		}
		return n, nil
	}
	ms, err := readInt("KAITO_RACER_TIMEOUT_MS", 30000, 300000)
	if err != nil {
		return nil, err
	}
	attempts, err := readInt("KAITO_RACER_MAX_ATTEMPTS", 3, 5)
	if err != nil {
		return nil, err
	}
	concurrency, err := readInt("KAITO_RACER_MAX_CONCURRENCY", 16, 64)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		// Never use HTTP_PROXY, a credential-bearing proxy, or upstream Azure.
		Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Duration(ms) * time.Millisecond, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression: true, MaxConnsPerHost: concurrency, MaxIdleConnsPerHost: concurrency,
		IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: time.Duration(ms) * time.Millisecond,
		MaxResponseHeaderBytes: 32 * 1024,
	}
	return &provider{endpoint: strings.TrimSuffix(endpoint, "/"), account: account, container: container,
		timeout: time.Duration(ms) * time.Millisecond, attempts: attempts, slots: make(chan struct{}, concurrency),
		client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func validName(s string, min, max int, dash bool) bool {
	if len(s) < min || len(s) > max || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.Contains(s, "--") {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && !(dash && c == '-') {
			return false
		}
	}
	return true
}

func escapedBlob(blob string) (string, error) {
	if blob == "" || len(blob) > 1024 || strings.ContainsRune(blob, 0) {
		return "", errors.New("invalid blob name")
	}
	parts := strings.Split(blob, "/")
	for i, part := range parts {
		if part == "." || part == ".." {
			return "", errors.New("dot path segments are not supported")
		}
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/"), nil
}

func (p *provider) read(account, container, blob string, offset uint64, dst []byte) error {
	if account != p.account || container != p.container {
		return errors.New("account/container not authorized by Racer binding")
	}
	escaped, err := escapedBlob(blob)
	if err != nil {
		return err
	}
	if len(dst) == 0 {
		return nil
	}
	// Safely limit the ABI to nonnegative signed 64-bit file ranges.
	const max = uint64(1<<63 - 1)
	if offset > max || uint64(len(dst))-1 > max-offset {
		return errors.New("range overflow")
	}
	end := offset + uint64(len(dst)) - 1
	// One bounded deadline includes queueing, all attempts and backoff.
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return errors.New("Racer read queue deadline exceeded")
	}
	for attempt := 0; attempt < p.attempts; attempt++ {
		retry, readErr := p.readOnce(ctx, p.endpoint+"/"+container+"/"+escaped, offset, end, dst)
		if readErr == nil {
			return nil
		}
		err = readErr
		if !retry || attempt+1 == p.attempts {
			break
		}
		// Equal-jitter exponential backoff avoids synchronizing loading ranks.
		// Retry only transient status/connect failures, never partial responses.
		ceiling := time.Duration(100*(1<<attempt)) * time.Millisecond
		delay := ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2)+1))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return errors.New("Racer read deadline exceeded")
		}
	}
	return err
}

func (p *provider) readOnce(ctx context.Context, address string, start, end uint64, dst []byte) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false, errors.New("cannot construct Racer request")
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := p.client.Do(req)
	if err != nil {
		return ctx.Err() == nil, errors.New("Racer connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		retry := resp.StatusCode == 429 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504
		// Respect server Retry-After by not doing an early internal retry. The caller
		// sees failure and can apply workload-level retry policy.
		if resp.Header.Get("Retry-After") != "" {
			retry = false
		}
		return retry, fmt.Errorf("Racer returned HTTP %d", resp.StatusCode)
	}
	values := resp.Header.Values("Content-Range")
	if len(values) != 1 || !validContentRange(values[0], start, end) {
		return false, errors.New("invalid Racer Content-Range")
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return false, errors.New("unexpected Racer content encoding")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != int64(len(dst)) {
		return false, errors.New("Racer content length mismatch")
	}
	// Write into the caller buffer without a second full-weight allocation. On
	// failure the ABI returns -1; callers MUST discard all bytes from this read.
	if _, err = io.ReadFull(resp.Body, dst); err != nil {
		return false, errors.New("incomplete Racer range")
	}
	var extra [1]byte
	if n, e := io.ReadFull(resp.Body, extra[:]); n != 0 || e != io.EOF {
		return false, errors.New("Racer response exceeds requested range or did not finish")
	}
	return false, nil
}

func validContentRange(value string, start, end uint64) bool {
	if !strings.HasPrefix(value, "bytes ") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(parts) != 2 {
		return false
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return false
	}
	parse := func(s string) (uint64, error) {
		if s == "" {
			return 0, errors.New("empty number")
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, errors.New("invalid number")
			}
		}
		return strconv.ParseUint(s, 10, 64)
	}
	a, e1 := parse(bounds[0])
	b, e2 := parse(bounds[1])
	total, e3 := parse(parts[1])
	return e1 == nil && e2 == nil && e3 == nil && a == start && b == end && b < total
}

var processProvider struct {
	sync.Once
	p   *provider
	err error
}
