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
	"time"
	"unicode/utf8"

	"tutor-mcp/models"
)

const (
	cimdDocumentLimitBytes int64 = 64 << 10
	cimdDefaultCacheTTL          = 5 * time.Minute
	cimdMaxCacheTTL              = 24 * time.Hour
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
}

func newCIMDHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
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
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
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
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
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
		"2001:db8::/32", "2001:10::/28",
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
	now := time.Now().UTC()
	s.cimdMu.Lock()
	if cached := s.cimdCache[clientID]; cached.client != nil && cached.expiresAt.After(now) {
		client := *cached.client
		s.cimdMu.Unlock()
		return &client, nil
	}
	s.cimdMu.Unlock()

	if _, err := parseCIMDClientID(clientID); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return nil, fmt.Errorf("create CIMD request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tutor-mcp-cimd/1")
	resp, err := s.cimdHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch CIMD: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch CIMD: unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdDocumentLimitBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read CIMD: %w", err)
	}
	if int64(len(body)) > cimdDocumentLimitBytes {
		return nil, fmt.Errorf("CIMD document too large")
	}
	var doc cimdDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode CIMD: %w", err)
	}
	if doc.ClientID != clientID || doc.ClientName == "" || len(doc.ClientName) > clientNameMaxLen ||
		!utf8.ValidString(doc.ClientName) || strings.ContainsAny(doc.ClientName, "\r\n\t") {
		return nil, fmt.Errorf("invalid CIMD identity metadata")
	}
	if doc.TokenEndpointAuthMethod != "none" {
		return nil, fmt.Errorf("unsupported CIMD token_endpoint_auth_method")
	}
	if len(doc.RedirectURIs) == 0 || validateRegistrationRedirectURIs(doc.RedirectURIs) != nil {
		return nil, fmt.Errorf("invalid CIMD redirect_uris")
	}
	if len(doc.GrantTypes) > 0 && !containsString(doc.GrantTypes, "authorization_code") {
		return nil, fmt.Errorf("CIMD does not support authorization_code")
	}
	if len(doc.ResponseTypes) > 0 && !containsString(doc.ResponseTypes, "code") {
		return nil, fmt.Errorf("CIMD does not support code responses")
	}
	redirectJSON, err := json.Marshal(doc.RedirectURIs)
	if err != nil {
		return nil, fmt.Errorf("encode CIMD redirect_uris: %w", err)
	}
	client := &models.OAuthClient{
		ClientID:     clientID,
		ClientName:   doc.ClientName,
		RedirectURIs: string(redirectJSON),
	}
	if ttl := cimdCacheTTL(resp.Header.Get("Cache-Control")); ttl > 0 {
		s.cimdMu.Lock()
		s.cimdCache[clientID] = cachedCIMDClient{client: client, expiresAt: now.Add(ttl)}
		s.cimdMu.Unlock()
	}
	copy := *client
	return &copy, nil
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
