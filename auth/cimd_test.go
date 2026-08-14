// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func cimdResponse(clientID, cacheControl string) *http.Response {
	body := fmt.Sprintf(`{
		"client_id": %q,
		"client_name": "Claude Test Client",
		"redirect_uris": ["https://client.example/callback"],
		"grant_types": ["authorization_code"],
		"response_types": ["code"],
		"token_endpoint_auth_method": "none"
	}`, clientID)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Cache-Control": []string{cacheControl}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestResolveOAuthClientFromCIMDAndCachesDocument(t *testing.T) {
	s, _ := newTestServer(t)
	const clientID = "https://client.example/oauth/metadata.json"
	var calls atomic.Int32
	s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.String() != clientID {
			t.Fatalf("fetch URL = %q", req.URL)
		}
		return cimdResponse(clientID, "public, max-age=3600"), nil
	})}

	for range 2 {
		client, err := s.resolveOAuthClient(context.Background(), clientID)
		if err != nil {
			t.Fatalf("resolve CIMD: %v", err)
		}
		if client.ClientID != clientID || client.ClientName != "Claude Test Client" || client.ClientSecretHash != "" {
			t.Fatalf("resolved client = %#v", client)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("CIMD fetch calls = %d, want 1", calls.Load())
	}
}

func TestFetchCIMDDocumentRejectsUnsafeURLSyntaxBeforeTransport(t *testing.T) {
	s, _ := newTestServer(t)
	var calls atomic.Int32
	s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("transport must not be reached")
	})}

	for _, clientID := range []string{
		"http://client.example/oauth/metadata.json",
		"https://client.example/oauth/meta data.json",
		"https://client.example/oauth/metadata.json\r\nX-Forged: true",
	} {
		if _, _, err := s.fetchCIMDDocument(context.Background(), clientID); err == nil {
			t.Errorf("fetchCIMDDocument(%q) accepted an unsafe URL", clientID)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unsafe CIMD URL reached transport %d times", calls.Load())
	}
}

func TestCIMDCacheEvictsLeastRecentlyUsedAtCapacity(t *testing.T) {
	s, _ := newTestServer(t)
	var callsMu sync.Mutex
	calls := make(map[string]int)
	s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		callsMu.Lock()
		calls[req.URL.String()]++
		callsMu.Unlock()
		return cimdResponse(req.URL.String(), "public, max-age=3600"), nil
	})}

	clientID := func(i int) string {
		return fmt.Sprintf("https://client.example/oauth/metadata-%03d.json", i)
	}
	for i := range cimdCacheCapacity {
		if _, err := s.fetchCIMDClient(context.Background(), clientID(i)); err != nil {
			t.Fatalf("prime cache entry %d: %v", i, err)
		}
	}
	// Refresh entry zero so entry one becomes the least recently used.
	if _, err := s.fetchCIMDClient(context.Background(), clientID(0)); err != nil {
		t.Fatalf("refresh first cache entry: %v", err)
	}
	if _, err := s.fetchCIMDClient(context.Background(), clientID(cimdCacheCapacity)); err != nil {
		t.Fatalf("insert overflow cache entry: %v", err)
	}

	s.cimdMu.Lock()
	cacheLen := len(s.cimdCache)
	_, keptRecent := s.cimdCache[clientID(0)]
	_, evictedOldest := s.cimdCache[clientID(1)]
	_, keptNew := s.cimdCache[clientID(cimdCacheCapacity)]
	s.cimdMu.Unlock()
	if cacheLen != cimdCacheCapacity {
		t.Fatalf("cache entries = %d, want fixed capacity %d", cacheLen, cimdCacheCapacity)
	}
	if !keptRecent || evictedOldest || !keptNew {
		t.Fatalf("unexpected LRU result: recent=%t oldest_present=%t new=%t", keptRecent, evictedOldest, keptNew)
	}

	if _, err := s.fetchCIMDClient(context.Background(), clientID(1)); err != nil {
		t.Fatalf("reload evicted cache entry: %v", err)
	}
	callsMu.Lock()
	firstCalls := calls[clientID(0)]
	evictedCalls := calls[clientID(1)]
	newCalls := calls[clientID(cimdCacheCapacity)]
	callsMu.Unlock()
	if firstCalls != 1 || evictedCalls != 2 || newCalls != 1 {
		t.Fatalf("fetch counts: recent=%d evicted=%d new=%d", firstCalls, evictedCalls, newCalls)
	}
}

func TestCIMDCachePrunesExpiredDocument(t *testing.T) {
	s, _ := newTestServer(t)
	const clientID = "https://client.example/oauth/expiring-metadata.json"
	var calls atomic.Int32
	s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return cimdResponse(clientID, "max-age=3600"), nil
	})}

	if _, err := s.fetchCIMDClient(context.Background(), clientID); err != nil {
		t.Fatalf("prime expiring cache entry: %v", err)
	}
	s.cimdMu.Lock()
	cached := s.cimdCache[clientID]
	cached.expiresAt = time.Now().Add(-time.Second)
	s.cimdCache[clientID] = cached
	s.cimdMu.Unlock()
	if _, err := s.fetchCIMDClient(context.Background(), clientID); err != nil {
		t.Fatalf("refresh expired cache entry: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("CIMD fetch calls = %d, want 2 after expiry", calls.Load())
	}
}

type cimdFetchResult struct {
	clientID string
	err      error
}

func waitForCIMDWaiters(t *testing.T, s *OAuthServer, clientID string, want int32) *cimdFlight {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.cimdMu.Lock()
		cached, ok := s.cimdCache[clientID]
		s.cimdMu.Unlock()
		if ok && cached.flight != nil && cached.flight.waiters.Load() >= want {
			return cached.flight
		}
		runtime.Gosched()
	}
	t.Fatalf("CIMD flight did not reach %d waiters", want)
	return nil
}

func waitForNoCIMDWaiters(t *testing.T, flight *cimdFlight) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if flight.waiters.Load() == 0 {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("CIMD flight retained %d waiters", flight.waiters.Load())
}

func waitForCIMDFetchCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("CIMD fetch calls = %d, want %d", calls.Load(), want)
}

func TestCIMDConcurrentNoStoreFetchIsDeduplicatedButNotCached(t *testing.T) {
	s, _ := newTestServer(t)
	const clientID = "https://client.example/oauth/no-store-metadata.json"
	const workers = 24
	var calls atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		<-release
		return cimdResponse(clientID, "no-store"), nil
	})}

	results := make(chan cimdFetchResult, workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			<-start
			client, err := s.fetchCIMDClient(context.Background(), clientID)
			result := cimdFetchResult{err: err}
			if client != nil {
				result.clientID = client.ClientID
			}
			results <- result
		}()
	}
	close(start)
	flight := waitForCIMDWaiters(t, s, clientID, workers)
	waitForCIMDFetchCalls(t, &calls, 1)
	releaseOnce.Do(func() { close(release) })
	for range workers {
		result := <-results
		if result.err != nil || result.clientID != clientID {
			t.Fatalf("shared CIMD result = %#v", result)
		}
	}
	waitForNoCIMDWaiters(t, flight)
	s.cimdMu.Lock()
	_, cached := s.cimdCache[clientID]
	s.cimdMu.Unlock()
	if cached {
		t.Fatal("Cache-Control: no-store document was persisted")
	}

	// A later, non-overlapping request must fetch the no-store document again.
	if _, err := s.fetchCIMDClient(context.Background(), clientID); err != nil {
		t.Fatalf("refetch no-store document: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("no-store CIMD fetch calls = %d, want 2", calls.Load())
	}
}

func TestCIMDFlightSharesErrorThenAllowsRetry(t *testing.T) {
	s, _ := newTestServer(t)
	const clientID = "https://client.example/oauth/retry-metadata.json"
	const workers = 12
	wantErr := errors.New("temporary upstream failure")
	var calls atomic.Int32
	releaseFailure := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseFailure) })
	s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			<-releaseFailure
			return nil, wantErr
		}
		return cimdResponse(clientID, "max-age=3600"), nil
	})}

	errs := make(chan error, workers)
	for range workers {
		go func() {
			_, err := s.fetchCIMDClient(context.Background(), clientID)
			errs <- err
		}()
	}
	flight := waitForCIMDWaiters(t, s, clientID, workers)
	releaseOnce.Do(func() { close(releaseFailure) })
	for range workers {
		if err := <-errs; !errors.Is(err, wantErr) {
			t.Fatalf("shared CIMD error = %v, want %v", err, wantErr)
		}
	}
	waitForNoCIMDWaiters(t, flight)
	if calls.Load() != 1 {
		t.Fatalf("failed concurrent CIMD fetch calls = %d, want 1", calls.Load())
	}
	if _, err := s.fetchCIMDClient(context.Background(), clientID); err != nil {
		t.Fatalf("retry CIMD after shared error: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("CIMD calls after retry = %d, want 2", calls.Load())
	}
}

func TestCIMDWaiterCancellationDoesNotCancelSharedFetch(t *testing.T) {
	s, _ := newTestServer(t)
	const clientID = "https://client.example/oauth/cancel-metadata.json"
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	outboundContext := make(chan context.Context, 1)
	s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		outboundContext <- req.Context()
		<-release
		return cimdResponse(clientID, "max-age=3600"), nil
	})}

	leaderResult := make(chan cimdFetchResult, 1)
	go func() {
		client, err := s.fetchCIMDClient(context.Background(), clientID)
		result := cimdFetchResult{err: err}
		if client != nil {
			result.clientID = client.ClientID
		}
		leaderResult <- result
	}()
	flight := waitForCIMDWaiters(t, s, clientID, 1)
	fetchContext := <-outboundContext

	waiterContext, cancelWaiter := context.WithCancel(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, err := s.fetchCIMDClient(waiterContext, clientID)
		waiterResult <- err
	}()
	waitForCIMDWaiters(t, s, clientID, 2)
	cancelWaiter()
	if err := <-waiterResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter error = %v, want context.Canceled", err)
	}
	select {
	case <-fetchContext.Done():
		t.Fatalf("one waiter cancelled shared CIMD fetch: %v", fetchContext.Err())
	default:
	}

	releaseOnce.Do(func() { close(release) })
	result := <-leaderResult
	if result.err != nil || result.clientID != clientID {
		t.Fatalf("leader result after waiter cancellation = %#v", result)
	}
	waitForNoCIMDWaiters(t, flight)
}

func TestResolveOAuthClientRejectsMismatchedCIMDIdentity(t *testing.T) {
	s, _ := newTestServer(t)
	const clientID = "https://client.example/oauth/metadata.json"
	s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return cimdResponse("https://attacker.example/oauth/metadata.json", "max-age=60"), nil
	})}
	if _, err := s.resolveOAuthClient(context.Background(), clientID); err == nil {
		t.Fatal("mismatched client_id must be rejected")
	}
}

func TestCIMDClientIDAndOutboundIPGuards(t *testing.T) {
	for _, clientID := range []string{
		"http://client.example/metadata.json",
		"https://client.example",
		"https://user@client.example/metadata.json",
		"https://127.0.0.1/metadata.json",
		"https://10.0.0.1/metadata.json",
		"https://192.0.2.1/metadata.json",
		"https://[64:ff9b::c000:201]/metadata.json",
		"https://[64:ff9b:1::1]/metadata.json",
		"https://[2001::1]/metadata.json",
		"https://[2002:c000:201::1]/metadata.json",
	} {
		if _, err := parseCIMDClientID(clientID); err == nil {
			t.Errorf("unsafe client_id accepted: %s", clientID)
		}
	}
}

func TestCIMDTransportRejectsUnsafeDNSWithoutDialing(t *testing.T) {
	tests := []struct {
		name string
		ips  []net.IPAddr
	}{
		{name: "private", ips: []net.IPAddr{{IP: net.ParseIP("10.0.0.8")}}},
		{name: "loopback", ips: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		{name: "link local", ips: []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}},
		{name: "carrier grade NAT", ips: []net.IPAddr{{IP: net.ParseIP("100.64.0.1")}}},
		{name: "NAT64", ips: []net.IPAddr{{IP: net.ParseIP("64:ff9b::a00:1")}}},
		{name: "Teredo", ips: []net.IPAddr{{IP: net.ParseIP("2001::ffff")}}},
		{name: "IPv6 documentation", ips: []net.IPAddr{{IP: net.ParseIP("2001:db8::1")}}},
		{name: "6to4", ips: []net.IPAddr{{IP: net.ParseIP("2002:0a00:0001::")}}},
		{name: "mixed public and private", ips: []net.IPAddr{
			{IP: net.ParseIP("8.8.8.8")},
			{IP: net.ParseIP("10.0.0.8")},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lookups atomic.Int32
			var dials atomic.Int32
			client := newCIMDHTTPClientWithNetwork(
				func(_ context.Context, host string) ([]net.IPAddr, error) {
					lookups.Add(1)
					if host != "client.example" {
						t.Fatalf("resolved host = %q", host)
					}
					return tt.ips, nil
				},
				func(context.Context, string, string) (net.Conn, error) {
					dials.Add(1)
					return nil, errors.New("unexpected dial")
				},
			)
			req, err := http.NewRequest(http.MethodGet, "https://client.example/oauth/metadata.json", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "non-public") {
				t.Fatalf("request error = %v, want non-public rejection", err)
			}
			if lookups.Load() != 1 || dials.Load() != 0 {
				t.Fatalf("network calls: lookups=%d dials=%d, want 1/0", lookups.Load(), dials.Load())
			}
		})
	}
}

func TestCIMDTransportDialsOnlyPinnedPublicAddress(t *testing.T) {
	var dialNetwork string
	var dialAddress string
	var dials atomic.Int32
	client := newCIMDHTTPClientWithNetwork(
		func(_ context.Context, host string) ([]net.IPAddr, error) {
			if host != "client.example" {
				t.Fatalf("resolved host = %q", host)
			}
			return []net.IPAddr{
				{IP: net.ParseIP("8.8.8.8")},
				{IP: net.ParseIP("1.1.1.1")},
			}, nil
		},
		func(_ context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			dialNetwork = network
			dialAddress = address
			return nil, errors.New("test dial stopped")
		},
	)
	req, err := http.NewRequest(http.MethodGet, "https://client.example/oauth/metadata.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "test dial stopped") {
		t.Fatalf("request error = %v, want injected dial error", err)
	}
	if dials.Load() != 1 || dialNetwork != "tcp" || dialAddress != "8.8.8.8:443" {
		t.Fatalf("dial calls=%d network=%q address=%q, want one tcp dial to pinned first IP", dials.Load(), dialNetwork, dialAddress)
	}
}

func TestAuthorizeAcceptsCIMDWithoutDynamicRegistration(t *testing.T) {
	s, _ := newTestServer(t)
	const clientID = "https://client.example/oauth/metadata.json"
	s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return cimdResponse(clientID, "max-age=3600"), nil
	})}
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"https://client.example/callback"},
		"response_type":         {"code"},
		"resource":              {testOAuthResource},
		"code_challenge":        {"challenge"},
		"code_challenge_method": {"S256"},
	}
	req := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	s.HandleAuthorizeGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorize CIMD status = %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Claude Test Client") {
		t.Fatalf("consent page missing CIMD client name")
	}
}
