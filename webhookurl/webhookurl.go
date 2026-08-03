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
	if u.User != nil || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	if host == "" || strings.Contains(host, "..") {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}
	host = strings.ToLower(host)
	switch host {
	case "discord.com", "discordapp.com", "canary.discord.com",
		"canary.discordapp.com", "ptb.discord.com", "ptb.discordapp.com":
		// Accepted below after validating the endpoint path.
	default:
		return false
	}

	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) < 4 || parts[0] != "api" || parts[1] != "webhooks" || parts[2] == "" || parts[3] == "" {
		return false
	}
	// Discord supports optional /github and /slack compatibility suffixes.
	return len(parts) == 4 || (len(parts) == 5 && (parts[4] == "github" || parts[4] == "slack"))
}
