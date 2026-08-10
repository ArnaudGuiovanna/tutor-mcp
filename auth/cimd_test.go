// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
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
	} {
		if _, err := parseCIMDClientID(clientID); err == nil {
			t.Errorf("unsafe client_id accepted: %s", clientID)
		}
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
