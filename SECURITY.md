# Security Policy

## Supported versions

Tutor MCP is in alpha. Only the most recent release tag (`v0.4.0`) and the `main` branch receive fixes. Older snapshots are not supported.

| Version | Supported |
|---|---|
| latest `v0.4.0` | ✅ |
| older release tags | ❌ — please upgrade |
| `main` (HEAD) | ✅ |

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.** Public disclosure before a fix is available exposes other operators running the runtime.

Two private channels are available:

1. **GitHub private vulnerability reporting** (preferred):
   - Go to https://github.com/ArnaudGuiovanna/tutor-mcp/security/advisories/new
   - Fill in the advisory form
   - This creates a private thread visible only to you and the maintainer

2. **Email**: send a description to `arnaud@tutor-mcp.dev` with the subject prefix `[tutor-mcp security]`. Include:
   - Affected version (release tag or commit SHA)
   - Steps to reproduce
   - Expected vs observed behavior
   - Any relevant logs (redact secrets)

## What to expect

- **Acknowledgement** within 72 hours.
- **Initial assessment** (severity, scope, exploitability) within 7 days. You'll be told whether the report is accepted as a security issue or treated as a regular bug.
- **Fix timeline** depends on severity:
  - Critical (auth bypass, RCE, persistent data leak across learners): patch within 7 days, coordinated disclosure
  - High (information disclosure, DoS): patch within 30 days
  - Medium / low: scheduled into the regular release cadence
- **Coordinated disclosure**: once a fix is released, the maintainer will publish a GitHub Security Advisory with credit to the reporter (unless you ask to remain anonymous).

## Scope

In scope:

- The runtime binary (`./tutor-mcp`)
- The OAuth 2.1 + PKCE auth flow
- The MCP tool surface (`tools/`)
- The cognitive engine (`engine/`, `algorithms/`)
- The persistence layer (`db/`)
- Default configuration of the runtime as documented in the README

Out of scope:

- Reverse-proxy / TLS termination configuration on your deployment
- Third-party MCP clients (Claude Desktop, ChatGPT, Le Chat, Gemini)
- Issues that require physical access to the host
- Issues that require an attacker to already have valid learner credentials *and* shell access to the host
- Vulnerabilities in upstream dependencies (please report those upstream first; we'll coordinate the bump once a fix lands there)

## Hardening checklist for operators

The runtime ships with reasonable defaults, but you are responsible for:

- Setting `JWT_SECRET` to 32+ random bytes (`openssl rand -base64 32`)
- Putting the runtime behind TLS in any deployment reachable from the public internet
- Setting `TRUSTED_PROXY_CIDRS` to your reverse proxy's CIDR so the per-IP rate limiter can distinguish clients
- Backing up `data/runtime.db` regularly (see [OPERATIONS.md](./OPERATIONS.md))
- Restricting who can register (see the comments around the `/register` rate limiter — public registration is on by default, lock it down at the proxy if you only want known users)
