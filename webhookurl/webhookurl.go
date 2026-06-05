// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// Package webhookurl provides an SSRF guard for outbound webhook URLs.
// Only Discord HTTPS endpoints are permitted; all other hosts are rejected.
package webhookurl

import (
	"net"
	"net/url"
	"strings"
)

// IsSafeWebhookURL validates that a webhook URL targets Discord over HTTPS.
// SSRF guard: only Discord webhook hosts allowed (blocks IMDS, internal ranges, etc.).
func IsSafeWebhookURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" || strings.Contains(host, "..") {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}
	host = strings.ToLower(host)
	switch host {
	case "discord.com", "discordapp.com":
		return true
	}
	return strings.HasSuffix(host, ".discord.com") || strings.HasSuffix(host, ".discordapp.com")
}
