// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"net"
	"net/http/httptest"
	"testing"
)

func TestClientIP_IgnoresXFFFromUntrustedPeer(t *testing.T) {
	// Force trusted set to empty (simulating unset TRUSTED_PROXY_CIDRS).
	trustedProxiesOnce.Do(func() {})
	trustedProxies = nil

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.7:55001"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := clientIP(r); got != "203.0.113.7" {
		t.Fatalf("XFF must be ignored without trusted proxies; got %q want %q", got, "203.0.113.7")
	}
}

func TestClientIP_HonorsXFFFromTrustedPeer(t *testing.T) {
	trustedProxiesOnce.Do(func() {})
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	trustedProxies = []*net.IPNet{cidr}
	t.Cleanup(func() { trustedProxies = nil })

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:443"
	r.Header.Set("X-Forwarded-For", "198.51.100.42, 10.0.0.5")

	if got := clientIP(r); got != "198.51.100.42" {
		t.Fatalf("rightmost untrusted XFF entry expected; got %q", got)
	}
}

func TestClientIP_KeysIPv6FromXFFByPrefix(t *testing.T) {
	trustedProxiesOnce.Do(func() {})
	_, cidr, _ := net.ParseCIDR("::1/128")
	trustedProxies = []*net.IPNet{cidr}
	t.Cleanup(func() { trustedProxies = nil })

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[::1]:443"
	// Mixed-case + zero-compression that net.ParseIP canonicalizes.
	r.Header.Set("X-Forwarded-For", "2001:DB8:0:0:0:0:0:1")

	got := clientIP(r)
	if got != "2001:db8::/64" {
		t.Fatalf("ipv6 not canonicalized to its /64 bucket: got %q", got)
	}
}

func TestClientIP_FallsBackWhenXFFMalformed(t *testing.T) {
	trustedProxiesOnce.Do(func() {})
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	trustedProxies = []*net.IPNet{cidr}
	t.Cleanup(func() { trustedProxies = nil })

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:443"
	r.Header.Set("X-Forwarded-For", "not-an-ip")

	if got := clientIP(r); got != "10.0.0.5" {
		t.Fatalf("malformed XFF should fall back to peer; got %q", got)
	}
}
