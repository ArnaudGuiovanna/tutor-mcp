// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setTrustedProxiesForTest(t *testing.T, cidrs ...string) {
	t.Helper()
	trustedProxiesOnce.Do(func() {})
	previous := trustedProxies
	parsed := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		_, cidr, err := net.ParseCIDR(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		parsed = append(parsed, cidr)
	}
	trustedProxies = parsed
	t.Cleanup(func() { trustedProxies = previous })
}

// Reverse proxies append the address they saw. Whatever sits left of the
// first untrusted hop was written by the client and must not choose the
// rate-limit bucket.
func TestClientIP_UsesRightmostUntrustedForwardedHop(t *testing.T) {
	setTrustedProxiesForTest(t, "10.0.0.0/8")
	cases := []struct {
		name    string
		headers []string
		want    string
	}{
		{name: "client-supplied prefix is ignored", headers: []string{"1.2.3.4, 198.51.100.42"}, want: "198.51.100.42"},
		{name: "chained trusted proxies are skipped", headers: []string{"6.6.6.6, 198.51.100.42, 10.0.0.9, 10.0.0.8"}, want: "198.51.100.42"},
		{name: "repeated header lines are one list", headers: []string{"1.2.3.4", "198.51.100.42"}, want: "198.51.100.42"},
		{name: "fully internal chain keeps the originating host", headers: []string{"10.0.0.20, 10.0.0.9"}, want: "10.0.0.20"},
		{name: "empty entries are skipped", headers: []string{"198.51.100.42, , "}, want: "198.51.100.42"},
		{name: "malformed trusted-side entry falls back to peer", headers: []string{"198.51.100.42, junk, 10.0.0.9"}, want: "10.0.0.5"},
		{name: "ipv4-mapped ipv6 is keyed as ipv4", headers: []string{"::ffff:198.51.100.42"}, want: "198.51.100.42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.0.0.5:443"
			for _, value := range tc.headers {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientIP_GroupsIPv6ByPrefix64(t *testing.T) {
	setTrustedProxiesForTest(t)
	key := func(remote string) string {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		return clientIP(r)
	}
	a := key("[2001:db8:1:2::1]:443")
	b := key("[2001:db8:1:2:ffff:ffff:ffff:ffff]:443")
	c := key("[2001:db8:1:3::1]:443")
	if a != b {
		t.Fatalf("addresses in one /64 got different buckets: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("distinct /64 networks share bucket %q", a)
	}
	if a != "2001:db8:1:2::/64" {
		t.Fatalf("bucket key = %q", a)
	}
	if got := key("198.51.100.7:443"); got != "198.51.100.7" {
		t.Fatalf("ipv4 bucket key = %q", got)
	}
}

// Before the fix, rotating the leftmost X-Forwarded-For value gave every
// request a fresh bucket behind any appending reverse proxy.
func TestRateLimitMiddleware_SpoofedForwardedPrefixSharesBucket(t *testing.T) {
	setTrustedProxiesForTest(t, "127.0.0.1/32")
	limiter := NewRateLimiter(0.001, 2)
	t.Cleanup(limiter.Stop)
	handler := RateLimitMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	statuses := make([]int, 0, 6)
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest(http.MethodPost, "/console/login", nil)
		r.RemoteAddr = "127.0.0.1:50000"
		spoofed := net.IPv4(203, 0, 113, byte(i+1)).String()
		r.Header.Set("X-Forwarded-For", spoofed+", 198.51.100.42")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		statuses = append(statuses, rec.Code)
	}
	for i, status := range statuses {
		want := http.StatusNoContent
		if i >= 2 {
			want = http.StatusTooManyRequests
		}
		if status != want {
			t.Fatalf("request %d status=%d want %d (all=%v)", i+1, status, want, statuses)
		}
	}
}

func TestRateLimitMiddleware_IPv6RotationWithinPrefixSharesBucket(t *testing.T) {
	setTrustedProxiesForTest(t)
	limiter := NewRateLimiter(0.001, 2)
	t.Cleanup(limiter.Stop)
	handler := RateLimitMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	limited := 0
	for i := 1; i <= 5; i++ {
		r := httptest.NewRequest(http.MethodPost, "/learn/login", nil)
		r.RemoteAddr = fmt.Sprintf("[2001:db8:42:7:%x::%x]:443", i*4099, i)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited != 3 {
		t.Fatalf("rotating addresses inside one /64 were limited %d times, want 3", limited)
	}
}

func TestCheckTrustedProxyCIDR(t *testing.T) {
	cases := []struct {
		cidr    string
		wantErr string
	}{
		{cidr: "0.0.0.0/0", wantErr: "catch-all"},
		{cidr: "::/0", wantErr: "catch-all"},
		{cidr: "0.0.0.0/1", wantErr: "broader than /8"},
		{cidr: "128.0.0.0/7", wantErr: "broader than /8"},
		{cidr: "2000::/3", wantErr: "broader than /16"},
		{cidr: "2001:db8::/15", wantErr: "broader than /16"},
		{cidr: "::ffff:0.0.0.0/96", wantErr: "catch-all"},
		{cidr: "::ffff:0.0.0.0/97", wantErr: "broader than /8"},
		{cidr: "::ffff:10.0.0.0/104"},
		{cidr: "10.0.0.0/8"},
		{cidr: "172.16.0.0/12"},
		{cidr: "104.16.0.0/13"},
		{cidr: "127.0.0.1/32"},
		{cidr: "::1/128"},
		{cidr: "2a06:98c0::/29"},
		{cidr: "fc00::/7"},
		{cidr: "fd00::/8"},
	}
	for _, tc := range cases {
		t.Run(tc.cidr, func(t *testing.T) {
			_, cidr, err := net.ParseCIDR(tc.cidr)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckTrustedProxyCIDR(cidr)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckTrustedProxyCIDR(%s) = %v, want nil", tc.cidr, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CheckTrustedProxyCIDR(%s) = %v, want %q", tc.cidr, err, tc.wantErr)
			}
		})
	}
	if got := parseTrustedProxiesCIDRs("0.0.0.0/1, 10.0.0.0/8"); len(got) != 1 || got[0].String() != "10.0.0.0/8" {
		t.Fatalf("parseTrustedProxiesCIDRs kept broad entry: %v", got)
	}
}
