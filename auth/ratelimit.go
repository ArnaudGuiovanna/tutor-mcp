// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	storeport "tutor-mcp/store"
)

// RateLimitBackend is an optional shared store the RateLimiter delegates to so a
// multi-instance fleet enforces one combined token bucket per key instead of an
// independent per-process bucket. Implemented in package db (Postgres-backed);
// nil means the in-memory default.
//
// It is an alias for store.RateLimitBackend: the interface lives in the neutral
// persistence-port package so package db can implement it without an import
// cycle (auth and db both depend on store; neither depends on the other).
type RateLimitBackend = storeport.RateLimitBackend

type bucket struct {
	tokens   float64
	lastTime time.Time
	elem     *list.Element
}

const authBackendTimeout = 2 * time.Second

const maxLocalRateLimitBuckets = 10_000

// RateLimiter implements a token bucket rate limiter keyed by caller identity.
type RateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	lru       *list.List
	rate      float64 // tokens per second
	burst     int     // max tokens
	namespace string  // separates policies sharing one persistent backend
	stop      chan struct{}
	stopOnce  sync.Once
	backend   RateLimitBackend // optional shared store; nil = in-memory only
}

// NewRateLimiter creates a rate limiter. rate is tokens/second, burst is max tokens.
// Starts a background goroutine to purge stale entries.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return NewRateLimiterWithNamespace("default", rate, burst)
}

// NewRateLimiterWithNamespace creates a limiter whose persistent bucket keys
// are isolated from other policies sharing the same backend. The in-memory
// path already has one map per limiter, so the prefix is needed only on the
// backend call.
func NewRateLimiterWithNamespace(namespace string, rate float64, burst int) *RateLimiter {
	if strings.TrimSpace(namespace) == "" {
		namespace = "default"
	}
	rl := &RateLimiter{
		buckets:   make(map[string]*bucket),
		lru:       list.New(),
		rate:      rate,
		burst:     burst,
		namespace: namespace,
		stop:      make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

// SetBackend installs a shared backend after construction. Passing nil restores
// the in-memory default.
func (rl *RateLimiter) SetBackend(backend RateLimitBackend) {
	rl.mu.Lock()
	rl.backend = backend
	rl.mu.Unlock()
}

// Allow consumes one token for the given key. Returns false if the bucket is empty.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	backend := rl.backend
	rl.mu.Unlock()
	if backend != nil {
		backendKey := rl.namespace + ":" + key
		ctx, cancel := context.WithTimeout(context.Background(), authBackendTimeout)
		allowed, err := backend.Allow(ctx, backendKey, rl.rate, rl.burst, time.Now())
		cancel()
		if err != nil {
			// Preserve availability without turning a shared-store outage into
			// an unthrottled authentication endpoint. The local bucket is weaker
			// than the fleet-wide policy, but still bounds each process. Never log
			// the bucket key: it can contain an IP address or learner identifier.
			slog.Warn("rate limit backend error, using local fallback", "err", err, "namespace", rl.namespace)
			return rl.allowLocal(key, time.Now())
		}
		return allowed
	}
	return rl.allowLocal(key, time.Now())
}

func (rl *RateLimiter) allowLocal(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) >= maxLocalRateLimitBuckets {
			if oldest := rl.lru.Back(); oldest != nil {
				oldestKey, _ := oldest.Value.(string)
				delete(rl.buckets, oldestKey)
				rl.lru.Remove(oldest)
			}
		}
		elem := rl.lru.PushFront(key)
		rl.buckets[key] = &bucket{tokens: float64(rl.burst) - 1, lastTime: now, elem: elem}
		return true
	}
	rl.lru.MoveToFront(b.elem)

	// Refill tokens based on elapsed time
	elapsed := now.Sub(b.lastTime).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.lastTime = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Stop shuts down the background cleanup goroutine.
func (rl *RateLimiter) Stop() {
	rl.stopOnce.Do(func() { close(rl.stop) })
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for key, b := range rl.buckets {
				if b.lastTime.Before(cutoff) {
					delete(rl.buckets, key)
					rl.lru.Remove(b.elem)
				}
			}
			rl.mu.Unlock()
		case <-rl.stop:
			return
		}
	}
}

// trustedProxiesOnce parses TRUSTED_PROXY_CIDRS exactly once at first use.
// XFF is honored only when the direct peer (r.RemoteAddr) falls inside one of
// these CIDRs, preventing a client from spoofing its own bucket key.
var (
	trustedProxiesOnce sync.Once
	trustedProxies     []*net.IPNet
)

// Narrowest prefix accepted for a trusted reverse-proxy network. A shorter
// prefix covers a large share of the public Internet, so most direct peers
// would be allowed to choose their own rate-limit key through X-Forwarded-For.
// The limits still admit 10.0.0.0/8 and published CDN ranges (Cloudflare's
// widest IPv6 range is a /29). IPv6 unique-local space (fc00::/7) is
// non-routable and is accepted at its natural size for container networks.
const (
	minTrustedProxyPrefixIPv4 = 8
	minTrustedProxyPrefixIPv6 = 16
)

var ipv6UniqueLocal = &net.IPNet{IP: net.ParseIP("fc00::"), Mask: net.CIDRMask(7, 128)}

// CheckTrustedProxyCIDR rejects networks too broad to be a reverse proxy.
// Catch-all CIDRs (0.0.0.0/0, ::/0) are reported as such so operators see why
// their value is unsafe. Startup validation and the runtime parser share it.
func CheckTrustedProxyCIDR(cidr *net.IPNet) error {
	ones, bits := cidr.Mask.Size()
	if bits == 128 && cidr.IP.To4() != nil {
		// An IPv4-mapped network (::ffff:a.b.c.d/n) matches IPv4 peers on its
		// last 32 bits, so ::ffff:0.0.0.0/96 would trust every IPv4 address.
		ones, bits = ones-96, 32
		if ones < 0 {
			ones = 0
		}
	}
	if ones == 0 {
		return fmt.Errorf("catch-all CIDR %s is unsafe", cidr)
	}
	if bits == 32 {
		if ones < minTrustedProxyPrefixIPv4 {
			return fmt.Errorf("CIDR %s is broader than /%d", cidr, minTrustedProxyPrefixIPv4)
		}
		return nil
	}
	uniqueLocalOnes, _ := ipv6UniqueLocal.Mask.Size()
	if ones >= uniqueLocalOnes && ipv6UniqueLocal.Contains(cidr.IP) {
		return nil
	}
	if ones < minTrustedProxyPrefixIPv6 {
		return fmt.Errorf("CIDR %s is broader than /%d", cidr, minTrustedProxyPrefixIPv6)
	}
	return nil
}

// parseTrustedProxiesCIDRs parses a comma-separated CIDR list and returns
// the valid net.IPNet entries. Catch-all and overly broad CIDRs are rejected
// with a slog.Warn — they would treat most direct peers as trusted, letting a
// client spoof X-Forwarded-For at will and defeating the per-IP rate limiter.
func parseTrustedProxiesCIDRs(raw string) []*net.IPNet {
	if raw == "" {
		return nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(part)
		if err != nil {
			slog.Warn("invalid TRUSTED_PROXY_CIDRS entry", "value", part, "err", err)
			continue
		}
		if err := CheckTrustedProxyCIDR(cidr); err != nil {
			slog.Warn("rejecting TRUSTED_PROXY_CIDRS entry — XFF would become attacker-controlled", "value", part, "err", err)
			continue
		}
		out = append(out, cidr)
	}
	return out
}

func loadTrustedProxies() []*net.IPNet {
	trustedProxiesOnce.Do(func() {
		trustedProxies = parseTrustedProxiesCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	})
	return trustedProxies
}

// shouldWarnRateLimiterMisconfig returns true when the deployment looks public
// (https + non-loopback hostname) but TRUSTED_PROXY_CIDRS is unset — meaning
// every request will bucket under the proxy's loopback IP, collapsing the
// per-IP rate limiter to a single shared bucket.
func shouldWarnRateLimiterMisconfig(baseURL, trustedProxiesEnv string) bool {
	if trustedProxiesEnv != "" {
		return false
	}
	u, err := url.Parse(baseURL)
	if err != nil || u == nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" || host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// WarnRateLimiterMisconfig emits a slog.Warn at startup if the BASE_URL points
// to a public hostname but TRUSTED_PROXY_CIDRS is unset. In that case the
// per-IP rate limiter cannot distinguish callers — every request shares one
// bucket — and the auth limiter shield collapses to a single global throttle.
func WarnRateLimiterMisconfig(baseURL string) {
	if shouldWarnRateLimiterMisconfig(baseURL, os.Getenv("TRUSTED_PROXY_CIDRS")) {
		slog.Warn(
			"rate limiter cannot distinguish clients behind a reverse proxy — set TRUSTED_PROXY_CIDRS to the proxy's CIDR (e.g. 127.0.0.1/32 or 10.0.0.0/8)",
			"base_url", baseURL,
		)
	}
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

func isTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, cidr := range loadTrustedProxies() {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP returns the bucket key. X-Forwarded-For is honored only when the
// direct peer is in TRUSTED_PROXY_CIDRS; otherwise a client could spoof its
// own bucket and bypass the per-IP limit.
//
// Reverse proxies append the address they received the request from, so only
// the right-hand side of the header is trustworthy: every entry to the left of
// the first untrusted hop may have been written by the client. The key is the
// rightmost address that is not itself a trusted proxy, as with nginx
// real_ip_recursive. If every hop is trusted, the leftmost one is the client.
// A malformed entry stops the walk and falls back to the direct peer, because
// nothing to its left can be attributed to a trusted proxy.
func clientIP(r *http.Request) string {
	peer := remoteIP(r)
	if isTrustedProxy(peer) {
		if forwarded, ok := forwardedClientIP(r.Header.Values("X-Forwarded-For")); ok {
			return rateLimitKey(forwarded)
		}
	}
	if peer != nil {
		return rateLimitKey(peer)
	}
	return r.RemoteAddr
}

func forwardedClientIP(headerValues []string) (net.IP, bool) {
	var hops []string
	for _, value := range headerValues {
		hops = append(hops, strings.Split(value, ",")...)
	}
	var candidate net.IP
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		parsed := net.ParseIP(hop)
		if parsed == nil {
			return nil, false
		}
		if !isTrustedProxy(parsed) {
			return parsed, true
		}
		candidate = parsed
	}
	return candidate, candidate != nil
}

// rateLimitKey canonicalizes an address into its rate-limit bucket. IPv4
// (including IPv4-mapped IPv6) is keyed per address. IPv6 is keyed per /64:
// a single subscriber or cloud instance routinely controls a whole /64, so a
// per-address key would hand each client 2^64 fresh buckets.
func rateLimitKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return (&net.IPNet{IP: ip.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}).String()
}

// RateLimitMiddleware wraps an http.Handler with rate limiting.
// Returns 429 Too Many Requests when the limit is exceeded.
func RateLimitMiddleware(limiter *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !limiter.Allow(ip) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, `{"error":"rate_limit_exceeded"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LearnerRateLimitMiddleware wraps an authenticated handler with per-learner
// rate limiting. It must run after BearerMiddleware, which injects learner_id.
func LearnerRateLimitMiddleware(limiter *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		learnerID := GetLearnerID(r.Context())
		if principal, ok := GetPrincipal(r.Context()); ok {
			learnerID = principal.SessionBindingID()
		}
		if learnerID == "" {
			slog.Warn("learner rate limiter missing learner_id in context")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !limiter.Allow(learnerID) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, `{"error":"rate_limit_exceeded"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
