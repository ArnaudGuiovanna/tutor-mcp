// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"tutor-mcp/models"
)

const (
	cimdDocumentLimitBytes int64 = 64 << 10
	cimdDefaultCacheTTL          = 5 * time.Minute
	cimdMaxCacheTTL              = 24 * time.Hour
	cimdFetchTimeout             = 5 * time.Second
	cimdCacheCapacity            = 256
)

type cimdDocument struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

type cachedCIMDClient struct {
	client    *models.OAuthClient
	expiresAt time.Time
	lastUsed  uint64
	flight    *cimdFlight
}

// cimdFlight represents one metadata request shared by every concurrent
// resolver for the same client_id. Each caller still waits on its own context;
// cancelling one waiter therefore cannot cancel the request for the others.
type cimdFlight struct {
	done    chan struct{}
	client  *models.OAuthClient
	err     error
	waiters atomic.Int32
}

var cimdCacheUseSequence atomic.Uint64

func newCIMDHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	return newCIMDHTTPClientWithNetwork(net.DefaultResolver.LookupIPAddr, dialer.DialContext)
}

type cimdLookupIPAddrFunc func(context.Context, string) ([]net.IPAddr, error)
type cimdDialContextFunc func(context.Context, string, string) (net.Conn, error)

func newCIMDHTTPClientWithNetwork(lookupIPAddr cimdLookupIPAddrFunc, dialContext cimdDialContextFunc) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 3 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("CIMD dial address: %w", err)
			}
			ips, err := lookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("CIMD resolve host: %w", err)
			}
			for _, resolved := range ips {
				if unsafeCIMDIP(resolved.IP) {
					return nil, fmt.Errorf("CIMD host resolves to a non-public address")
				}
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("CIMD host has no addresses")
			}
			// Connect to the already-vetted address rather than resolving the
			// hostname a second time. This closes DNS rebinding between validation
			// and connection while TLS still authenticates the original hostname.
			return dialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("CIMD redirects are not accepted")
		},
	}
}

func unsafeCIMDIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() || isPrivateIP(ip) {
		return true
	}
	for _, network := range cimdDeniedCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

var cimdDeniedCIDRs = func() []*net.IPNet {
	blocks := []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", // NAT64 translation prefixes.
		"2001::/32", "2001:10::/28", "2001:db8::/32", // Teredo, ORCHID, and documentation.
		"2002::/16", // 6to4 embeds an attacker-selected IPv4 destination.
	}
	var networks []*net.IPNet
	for _, block := range blocks {
		_, network, err := net.ParseCIDR(block)
		if err == nil {
			networks = append(networks, network)
		}
	}
	return networks
}()

func parseCIMDClientID(clientID string) (*url.URL, error) {
	if clientID == "" || len(clientID) > 512 || !utf8.ValidString(clientID) {
		return nil, fmt.Errorf("invalid CIMD client_id")
	}
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Path == "" || u.Path == "/" {
		return nil, fmt.Errorf("CIMD client_id must be an HTTPS URL with a path")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && unsafeCIMDIP(ip) {
		return nil, fmt.Errorf("CIMD client_id host must be public")
	}
	return u, nil
}

func isCIMDClientID(clientID string) bool {
	_, err := parseCIMDClientID(clientID)
	return err == nil
}

func (s *OAuthServer) resolveOAuthClient(ctx context.Context, clientID string) (*models.OAuthClient, error) {
	client, err := s.store.GetOAuthClient(ctx, clientID)
	if err == nil {
		return client, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !isCIMDClientID(clientID) {
		return nil, err
	}
	return s.fetchCIMDClient(ctx, clientID)
}

func (s *OAuthServer) fetchCIMDClient(ctx context.Context, clientID string) (*models.OAuthClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := parseCIMDClientID(clientID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	s.cimdMu.Lock()
	if s.cimdCache == nil {
		s.cimdCache = make(map[string]cachedCIMDClient)
	}
	s.pruneExpiredCIMDClientsLocked(now)
	if cached, ok := s.cimdCache[clientID]; ok && cached.client != nil {
		cached.lastUsed = cimdCacheUseSequence.Add(1)
		s.cimdCache[clientID] = cached
		client := *cached.client
		s.cimdMu.Unlock()
		return &client, nil
	}
	if cached, ok := s.cimdCache[clientID]; ok && cached.flight != nil {
		flight := cached.flight
		s.cimdMu.Unlock()
		return waitForCIMDFlight(ctx, flight)
	}
	for len(s.cimdCache) >= cimdCacheCapacity {
		if !s.evictCIMDClientLocked() {
			s.cimdMu.Unlock()
			return nil, fmt.Errorf("CIMD cache is at capacity")
		}
	}
	flight := &cimdFlight{done: make(chan struct{})}
	s.cimdCache[clientID] = cachedCIMDClient{flight: flight}
	s.cimdMu.Unlock()

	// The fetch has its own bounded lifetime so cancellation of the first
	// caller cannot make every other waiter fail. The production HTTP client
	// has the same timeout as an additional transport-wide bound.
	go s.runCIMDFlight(clientID, flight)
	return waitForCIMDFlight(ctx, flight)
}

func waitForCIMDFlight(ctx context.Context, flight *cimdFlight) (*models.OAuthClient, error) {
	flight.waiters.Add(1)
	defer flight.waiters.Add(-1)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		if flight.err != nil {
			return nil, flight.err
		}
		client := *flight.client
		return &client, nil
	}
}

func (s *OAuthServer) runCIMDFlight(clientID string, flight *cimdFlight) {
	ctx, cancel := context.WithTimeout(context.Background(), cimdFetchTimeout)
	defer cancel()

	client, ttl, err := s.fetchCIMDDocument(ctx, clientID)

	s.cimdMu.Lock()
	flight.client = client
	flight.err = err
	if current, ok := s.cimdCache[clientID]; ok && current.flight == flight {
		if err != nil || ttl <= 0 {
			delete(s.cimdCache, clientID)
		} else {
			s.cimdCache[clientID] = cachedCIMDClient{
				client:    client,
				expiresAt: time.Now().UTC().Add(ttl),
				lastUsed:  cimdCacheUseSequence.Add(1),
			}
		}
	}
	close(flight.done)
	s.cimdMu.Unlock()
}

func (s *OAuthServer) fetchCIMDDocument(ctx context.Context, clientID string) (*models.OAuthClient, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create CIMD request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tutor-mcp-cimd/1")
	resp, err := s.cimdHTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch CIMD: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("fetch CIMD: unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdDocumentLimitBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read CIMD: %w", err)
	}
	if int64(len(body)) > cimdDocumentLimitBytes {
		return nil, 0, fmt.Errorf("CIMD document too large")
	}
	var doc cimdDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, 0, fmt.Errorf("decode CIMD: %w", err)
	}
	if doc.ClientID != clientID || doc.ClientName == "" || len(doc.ClientName) > clientNameMaxLen ||
		!utf8.ValidString(doc.ClientName) || strings.ContainsAny(doc.ClientName, "\r\n\t") {
		return nil, 0, fmt.Errorf("invalid CIMD identity metadata")
	}
	if doc.TokenEndpointAuthMethod != "none" {
		return nil, 0, fmt.Errorf("unsupported CIMD token_endpoint_auth_method")
	}
	if len(doc.RedirectURIs) == 0 || validateRegistrationRedirectURIs(doc.RedirectURIs) != nil {
		return nil, 0, fmt.Errorf("invalid CIMD redirect_uris")
	}
	if len(doc.GrantTypes) > 0 && !containsString(doc.GrantTypes, "authorization_code") {
		return nil, 0, fmt.Errorf("CIMD does not support authorization_code")
	}
	if len(doc.ResponseTypes) > 0 && !containsString(doc.ResponseTypes, "code") {
		return nil, 0, fmt.Errorf("CIMD does not support code responses")
	}
	redirectJSON, err := json.Marshal(doc.RedirectURIs)
	if err != nil {
		return nil, 0, fmt.Errorf("encode CIMD redirect_uris: %w", err)
	}
	client := &models.OAuthClient{
		ClientID:     clientID,
		ClientName:   doc.ClientName,
		RedirectURIs: string(redirectJSON),
	}
	return client, cimdCacheTTL(resp.Header.Get("Cache-Control")), nil
}

func (s *OAuthServer) pruneExpiredCIMDClientsLocked(now time.Time) {
	for clientID, cached := range s.cimdCache {
		if cached.flight == nil && (cached.client == nil || !cached.expiresAt.After(now)) {
			delete(s.cimdCache, clientID)
		}
	}
}

func (s *OAuthServer) evictCIMDClientLocked() bool {
	var victim string
	var oldest uint64
	found := false
	for clientID, cached := range s.cimdCache {
		if cached.flight != nil {
			continue
		}
		if !found || cached.lastUsed < oldest || (cached.lastUsed == oldest && clientID < victim) {
			victim = clientID
			oldest = cached.lastUsed
			found = true
		}
	}
	if found {
		delete(s.cimdCache, victim)
	}
	return found
}

func cimdCacheTTL(cacheControl string) time.Duration {
	if strings.Contains(strings.ToLower(cacheControl), "no-store") {
		return 0
	}
	for _, directive := range strings.Split(cacheControl, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(strings.ToLower(directive)), "=")
		if !found || key != "max-age" {
			continue
		}
		seconds, err := strconv.ParseInt(strings.Trim(value, `"`), 10, 64)
		if err != nil || seconds <= 0 {
			return 0
		}
		ttl := time.Duration(seconds) * time.Second
		if ttl > cimdMaxCacheTTL {
			return cimdMaxCacheTTL
		}
		return ttl
	}
	return cimdDefaultCacheTTL
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
