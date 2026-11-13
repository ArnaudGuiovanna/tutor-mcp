// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Browser cookies use the __Host- prefix: the browser then accepts them only
// with Secure, Path=/ and no Domain attribute, so a sibling subdomain or a
// plain-HTTP response can neither set nor shadow them. Every cookie is
// HttpOnly. Path=/ means a per-flow name replaces the old per-path scoping.
//
// The prefix requires Secure, which a browser does not honour over plain
// http://localhost in every engine. A BASE_URL on an HTTP loopback origin
// (local development only; production requires HTTPS) therefore uses the same
// names without the prefix and without Secure. No other origin downgrades.
const (
	hostCookiePrefix             = "__Host-"
	authorizeCSRFCookieName      = hostCookiePrefix + "tutor_csrf_authorize"
	trustedLoginDeviceCookieName = hostCookiePrefix + "tutor_trusted_device"
)

// accountCSRFCookieName names the CSRF cookie of one account form, so two
// account flows open in separate tabs (for example e-mail verification and
// password recovery) do not overwrite each other's token.
func accountCSRFCookieName(path string) string {
	suffix := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.ToLower(strings.Trim(path, "/")))
	return hostCookiePrefix + "tutor_csrf_" + suffix
}

// loopbackHTTPOrigin reports whether baseURL is a plain-HTTP loopback origin.
func loopbackHTTPOrigin(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// cookieName returns the effective name of a __Host- cookie for this server.
func (s *OAuthServer) cookieName(name string) string {
	if s.loopbackHTTPCookies {
		return strings.TrimPrefix(name, hostCookiePrefix)
	}
	return name
}

// setCookie writes (maxAge > 0) or clears (maxAge < 0) a host-only cookie.
func (s *OAuthServer) setCookie(w http.ResponseWriter, name, value string, maxAge int, sameSite http.SameSite) {
	cookie := &http.Cookie{
		Name: s.cookieName(name), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: !s.loopbackHTTPCookies, SameSite: sameSite,
	}
	if maxAge < 0 {
		cookie.Expires = time.Unix(1, 0).UTC()
	}
	http.SetCookie(w, cookie)
}

// cookieValue reads a cookie written by setCookie.
func (s *OAuthServer) cookieValue(r *http.Request, name string) (string, bool) {
	cookie, err := r.Cookie(s.cookieName(name))
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}
