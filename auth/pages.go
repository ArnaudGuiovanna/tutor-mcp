// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
)

// formActionOriginFromRedirectURI returns the origin (scheme://host[:port])
// of a validated redirect_uri so it can be added to the CSP form-action
// allowlist. The CSP form-action directive applies to the entire request
// chain — including the 302 the server returns after a successful login —
// so without listing the redirect_uri origin alongside 'self', the browser
// silently blocks the navigation to e.g. claude.ai/api/mcp/auth_callback
// and the OAuth flow stalls (issue: claude.ai never POSTs /token).
//
// Returns "" if the URI fails to parse; the caller falls back to 'self'
// only, which still works for first-party callback URLs on the same host.
func formActionOriginFromRedirectURI(redirectURI string) string {
	if redirectURI == "" {
		return ""
	}
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

type authPageData struct {
	ClientID            string
	ClientName          string
	RedirectURI         string
	RedirectOrigin      string
	ResponseType        string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Scope               string
	ScopeDescription    string
	Resource            string
	CSRFToken           string
	TenantOptions       []tenantOption
}

type tenantOption struct {
	ID   string
	Name string
}

// generateNonce returns a fresh base64 nonce for CSP script-src.
func generateNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

var authTmpl = template.Must(template.New("auth").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>tutor/mcp — sign in</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

    :root {
      --cream-1: #FCFAF7;
      --cream-2: #F5F0EA;
      --terracotta: #e8804a;
      --terracotta-deep: #d97757;
      --terracotta-shadow: #c46a3c;
      --lavender: #b496c8;
      --ink: #1c1a18;
      --ink-soft: #5a5249;
      --ink-mute: #7a7370;
    }

    html, body { min-height: 100vh; }

    body {
      font-family: system-ui, -apple-system, "Inter Tight", "Segoe UI", Roboto, sans-serif;
      color: var(--ink);
      background: #ffffff;
      position: relative;
      overflow-x: hidden;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem 1rem;
    }

    /* Subtle decorative blobs — same colour cues as banner.svg, lighter
       so the page reads as predominantly white. They give the glass card
       something to refract behind without breaking the light feel. */
    body::before, body::after {
      content: "";
      position: fixed;
      pointer-events: none;
      border-radius: 50%;
      filter: blur(90px);
      z-index: 0;
    }
    body::before {
      width: 55vw;
      height: 55vw;
      top: -18vw;
      right: -18vw;
      background: radial-gradient(circle, rgba(232,128,74,0.10) 0%, rgba(232,128,74,0) 65%);
    }
    body::after {
      width: 45vw;
      height: 45vw;
      bottom: -14vw;
      left: -14vw;
      background: radial-gradient(circle, rgba(180,150,200,0.08) 0%, rgba(180,150,200,0) 65%);
    }

    .shell {
      position: relative;
      z-index: 1;
      width: 100%;
      max-width: 420px;
    }

    .eyebrow {
      font-family: ui-monospace, "JetBrains Mono", "Cascadia Mono", "SFMono-Regular", Menlo, Consolas, monospace;
      font-size: 0.7rem;
      letter-spacing: 0.22em;
      color: var(--ink-mute);
      margin-bottom: 1rem;
      display: flex;
      align-items: center;
      gap: 0.6rem;
    }
    .eyebrow::before {
      content: "";
      display: inline-block;
      width: 1.25rem;
      height: 1px;
      background: var(--ink-mute);
      opacity: 0.45;
    }

    .brand {
      display: flex;
      align-items: center;
      gap: 0.85rem;
      margin-bottom: 0.9rem;
    }
    .brand svg { flex: 0 0 auto; }
    .wordmark {
      font-size: 2rem;
      font-weight: 700;
      letter-spacing: -0.04em;
      color: var(--ink);
      line-height: 1;
    }
    .wordmark .slash { color: var(--terracotta); }

    .tagline {
      font-family: "Instrument Serif", Georgia, "Times New Roman", serif;
      font-style: italic;
      font-size: 1.5rem;
      line-height: 1.15;
      margin-bottom: 1.8rem;
      background: linear-gradient(90deg, var(--terracotta) 0%, var(--terracotta-deep) 100%);
      -webkit-background-clip: text;
      background-clip: text;
      color: transparent;
    }

    .card {
      background: rgba(255, 255, 255, 0.55);
      border: 1px solid rgba(232, 128, 74, 0.22);
      border-radius: 18px;
      padding: 2rem 1.75rem;
      backdrop-filter: blur(18px) saturate(140%);
      -webkit-backdrop-filter: blur(18px) saturate(140%);
      box-shadow:
        inset 0 1px 0 rgba(255, 255, 255, 0.7),
        0 1px 2px rgba(28, 26, 24, 0.04),
        0 12px 36px rgba(28, 26, 24, 0.06);
    }

    .subtitle {
      font-family: "Instrument Serif", Georgia, "Times New Roman", serif;
      font-style: italic;
      font-size: 1.05rem;
      color: var(--terracotta-shadow);
      margin-bottom: 1.4rem;
    }

    .error-box {
      background: rgba(232, 128, 74, 0.08);
      border: 1px solid rgba(232, 128, 74, 0.35);
      border-radius: 10px;
      padding: 0.75rem 0.95rem;
      font-size: 0.88rem;
      color: var(--terracotta-shadow);
      margin-bottom: 1.2rem;
    }

    .consent-box {
      margin-top: 1.2rem;
      padding: 0.85rem 0.95rem;
      border: 1px solid rgba(180, 150, 200, 0.35);
      border-radius: 12px;
      background: rgba(180, 150, 200, 0.08);
      color: var(--ink-soft);
      font-size: 0.84rem;
      line-height: 1.45;
    }
    .consent-box strong { color: var(--ink); }
    .consent-check {
      display: flex;
      gap: 0.55rem;
      align-items: flex-start;
      margin-top: 0.75rem;
      font-family: system-ui, -apple-system, "Inter Tight", "Segoe UI", Roboto, sans-serif;
      font-size: 0.84rem;
      color: var(--ink);
    }
    .consent-check input {
      width: auto;
      margin-top: 0.15rem;
      accent-color: var(--terracotta);
    }

    label {
      display: block;
      font-family: ui-monospace, "JetBrains Mono", "Cascadia Mono", "SFMono-Regular", Menlo, Consolas, monospace;
      font-size: 0.7rem;
      font-weight: 500;
      color: var(--ink-soft);
      margin-bottom: 0.4rem;
      margin-top: 1.05rem;
      text-transform: uppercase;
      letter-spacing: 0.18em;
    }
    label:first-of-type { margin-top: 0; }

    input[type="email"],
	input[type="password"],
	select {
      width: 100%;
      background: rgba(255, 255, 255, 0.85);
      border: 1px solid rgba(122, 115, 112, 0.3);
      border-radius: 10px;
      padding: 0.7rem 0.9rem;
      font-size: 0.95rem;
      font-family: inherit;
      color: var(--ink);
      outline: none;
      transition: border-color 0.15s, box-shadow 0.15s, background 0.15s;
    }
    input[type="email"]::placeholder,
    input[type="password"]::placeholder { color: rgba(122, 115, 112, 0.55); }

	input:focus, select:focus {
      border-color: var(--terracotta);
      background: #fff;
      box-shadow: 0 0 0 3px rgba(232, 128, 74, 0.18);
    }

    button[type="submit"] {
      margin-top: 1.6rem;
      width: 100%;
      background: linear-gradient(180deg, var(--terracotta) 0%, #d97742 100%);
      color: #fff;
      border: none;
      border-radius: 10px;
      padding: 0.78rem;
      font-size: 0.98rem;
      font-weight: 600;
      letter-spacing: -0.01em;
      cursor: pointer;
      transition: transform 0.08s, box-shadow 0.15s, filter 0.15s;
      box-shadow: 0 2px 0 rgba(196, 106, 60, 0.25), 0 6px 16px rgba(232, 128, 74, 0.25);
    }
    button[type="submit"]:hover { filter: brightness(1.04); }
    button[type="submit"]:active { transform: translateY(1px); box-shadow: 0 1px 0 rgba(196, 106, 60, 0.25), 0 3px 10px rgba(232, 128, 74, 0.22); }

    .toggle {
      margin-top: 1.4rem;
      text-align: center;
      font-size: 0.88rem;
      color: var(--ink-soft);
    }
    .toggle a {
      color: var(--terracotta-shadow);
      text-decoration: none;
      font-weight: 600;
      cursor: pointer;
      border-bottom: 1px dashed rgba(196, 106, 60, 0.4);
      padding-bottom: 1px;
    }
    .toggle a:hover { color: var(--terracotta); border-bottom-color: var(--terracotta); }

    .footnote {
      margin-top: 1.5rem;
      font-family: ui-monospace, "JetBrains Mono", "Cascadia Mono", "SFMono-Regular", Menlo, Consolas, monospace;
      font-size: 0.7rem;
      letter-spacing: 0.08em;
      color: var(--ink-mute);
      text-align: center;
      opacity: 0.75;
      line-height: 1.7;
    }
    .footnote .built-by {
      font-family: "Instrument Serif", Georgia, "Times New Roman", serif;
      font-style: italic;
      font-size: 0.85rem;
      letter-spacing: 0;
      color: var(--ink-soft);
    }
    .footnote .built-by a {
      color: var(--terracotta-shadow);
      text-decoration: none;
      border-bottom: 1px dashed rgba(196, 106, 60, 0.4);
      padding-bottom: 1px;
    }
    .footnote .built-by a:hover {
      color: var(--terracotta);
      border-bottom-color: var(--terracotta);
    }

    .hidden { display: none; }
  </style>
</head>
<body>
  <div class="shell">
    <div class="eyebrow">OPEN&nbsp;SOURCE&nbsp;&nbsp;·&nbsp;&nbsp;MODEL&nbsp;CONTEXT&nbsp;PROTOCOL</div>

    <div class="brand">
      <!-- Neural-net node logo (from banner.svg) -->
      <svg width="56" height="56" viewBox="0 0 100 100" aria-hidden="true">
        <g stroke="#e8804a" stroke-width="1.5" stroke-linecap="round" stroke-opacity="0.6" fill="none">
          <line x1="26" y1="24" x2="50" y2="56"/>
          <line x1="72" y1="18" x2="50" y2="56"/>
          <line x1="82" y1="46" x2="50" y2="56"/>
          <line x1="68" y1="78" x2="50" y2="56"/>
          <line x1="22" y1="70" x2="50" y2="56"/>
          <line x1="44" y1="44" x2="50" y2="56"/>
        </g>
        <circle cx="26" cy="24" r="3.6" fill="#e8804a"/>
        <circle cx="72" cy="18" r="2.6" fill="#e8804a"/>
        <circle cx="82" cy="46" r="3.6" fill="#e8804a"/>
        <circle cx="68" cy="78" r="3.6" fill="#e8804a"/>
        <circle cx="22" cy="70" r="3.6" fill="#e8804a"/>
        <circle cx="50" cy="56" r="5" fill="#d97757"/>
      </svg>
      <div class="wordmark">tutor<span class="slash">/</span>mcp</div>
    </div>

    <div class="tagline">Self-learning is a superpower.</div>

    <div class="card">
      {{if .ErrMsg}}
      <div class="error-box">{{.ErrMsg}}</div>
      {{end}}

      <!-- Login form -->
      <div id="login-view">
        <p class="subtitle">Sign in to continue.</p>
        <form method="POST" action="/authorize">
          <input type="hidden" name="mode" value="login" />
          <input type="hidden" name="csrf_token"            value="{{.Data.CSRFToken}}" />
          <input type="hidden" name="client_id"             value="{{.Data.ClientID}}" />
          <input type="hidden" name="redirect_uri"          value="{{.Data.RedirectURI}}" />
          <input type="hidden" name="response_type"         value="{{.Data.ResponseType}}" />
          <input type="hidden" name="state"                 value="{{.Data.State}}" />
          <input type="hidden" name="code_challenge"        value="{{.Data.CodeChallenge}}" />
          <input type="hidden" name="code_challenge_method" value="{{.Data.CodeChallengeMethod}}" />
          <input type="hidden" name="scope"                 value="{{.Data.Scope}}" />
		  <input type="hidden" name="resource"              value="{{.Data.Resource}}" />

          <label for="login-email">Email</label>
          <input id="login-email" type="email" name="email" placeholder="you@example.com" required autocomplete="email" />

          <label for="login-password">Password</label>
          <input id="login-password" type="password" name="password" placeholder="••••••••" required autocomplete="current-password" />

          {{if .Data.TenantOptions}}
          <label for="login-tenant">Organization</label>
          <select id="login-tenant" name="tenant_id" required>
            <option value="">Choose an organization</option>
            {{range .Data.TenantOptions}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
          </select>
          {{end}}

          <div class="consent-box">
            <p><strong>{{.Data.ClientName}}</strong> wants access to your tutor/mcp learner account.</p>
			<p><strong>Requested permissions:</strong> {{.Data.ScopeDescription}}</p>
            <p>After sign-in, tutor/mcp will send an authorization code to <strong>{{.Data.RedirectOrigin}}</strong>.</p>
            <label class="consent-check" for="login-consent">
              <input id="login-consent" type="checkbox" name="approve_client" value="yes" required />
              <span>I recognize this client and approve sharing access.</span>
            </label>
          </div>

          <button type="submit">Sign in →</button>
        </form>
        <p class="toggle"><a href="/recover">Forgot your password?</a></p>
        <p class="toggle">No account? <a href="#" class="toggle-link">Create one</a></p>
      </div>

      <!-- Register form -->
      <div id="register-view" class="hidden">
        <p class="subtitle">Enter your email. You will choose your password and approve the client from the verification link.</p>
        <form method="POST" action="/authorize">
          <input type="hidden" name="mode" value="register" />
          <input type="hidden" name="csrf_token"            value="{{.Data.CSRFToken}}" />
          <input type="hidden" name="client_id"             value="{{.Data.ClientID}}" />
          <input type="hidden" name="redirect_uri"          value="{{.Data.RedirectURI}}" />
          <input type="hidden" name="response_type"         value="{{.Data.ResponseType}}" />
          <input type="hidden" name="state"                 value="{{.Data.State}}" />
          <input type="hidden" name="code_challenge"        value="{{.Data.CodeChallenge}}" />
          <input type="hidden" name="code_challenge_method" value="{{.Data.CodeChallengeMethod}}" />
          <input type="hidden" name="scope"                 value="{{.Data.Scope}}" />
		  <input type="hidden" name="resource"              value="{{.Data.Resource}}" />

          <label for="reg-email">Email</label>
          <input id="reg-email" type="email" name="email" placeholder="you@example.com" required autocomplete="email" />

          <div class="consent-box">
            <p><strong>{{.Data.ClientName}}</strong> requested access to a tutor/mcp learner account.</p>
			<p><strong>Requested permissions:</strong> {{.Data.ScopeDescription}}</p>
            <p>No password or authorization is created here. The email verification page will show the exact client and destination before asking for consent.</p>
          </div>

          <button type="submit">Send verification email →</button>
        </form>
        <p class="toggle">Already have an account? <a href="#" class="toggle-link">Sign in</a></p>
      </div>
    </div>

    <p class="footnote">
      tutor/mcp · open source · MIT<br/>
      <span class="built-by">Built by <a href="https://www.aguiovanna.fr" target="_blank" rel="noopener noreferrer">Arnaud Guiovanna</a></span>
    </p>
  </div>

  <script nonce="{{.Nonce}}">
    function toggleView() {
      document.getElementById('login-view').classList.toggle('hidden');
      document.getElementById('register-view').classList.toggle('hidden');
    }
    document.querySelectorAll('.toggle-link').forEach(function (el) {
      el.addEventListener('click', function (e) {
        e.preventDefault();
        toggleView();
      });
    });
    {{if eq .Mode "register"}}toggleView();{{end}}
  </script>
</body>
</html>
`))

type tmplData struct {
	Data   authPageData
	ErrMsg string
	Mode   string // "login" or "register"
	Nonce  string
}

func renderAuthPage(w http.ResponseWriter, data authPageData, errMsg string, mode string) {
	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusUnauthorized
	}
	renderAuthPageStatus(w, status, data, errMsg, mode)
}

// renderAuthPageStatus keeps security and transient failures on the same HTML
// path while allowing callers to distinguish invalid credentials (401),
// progressive backpressure (429), and exhausted bcrypt capacity (503).
func renderAuthPageStatus(w http.ResponseWriter, status int, data authPageData, errMsg string, mode string) {
	if data.ClientName == "" {
		data.ClientName = data.ClientID
	}
	if data.RedirectOrigin == "" {
		data.RedirectOrigin = formActionOriginFromRedirectURI(data.RedirectURI)
	}
	if data.ScopeDescription == "" {
		data.ScopeDescription = oauthScopeDescription(data.Scope)
	}
	nonce, err := generateNonce()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Strict CSP: only same-origin resources; inline scripts must carry our
	// per-request nonce. style-src keeps 'unsafe-inline' for the page's
	// inline <style> block — style injection is not credential-exfil-grade.
	//
	// form-action must include the redirect_uri origin in addition to 'self':
	// the directive applies to the entire submission chain, so the 302 we
	// return to claude.ai (or any other client) after a successful login
	// would otherwise be blocked client-side. The redirect_uri has already
	// been validated against the registered list at this point, so listing
	// its origin here cannot widen the attack surface.
	formAction := "'self'"
	if origin := formActionOriginFromRedirectURI(data.RedirectURI); origin != "" {
		formAction = "'self' " + origin
	}
	w.Header().Set("Content-Security-Policy", fmt.Sprintf(
		"default-src 'self'; script-src 'self' 'nonce-%s'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; form-action %s; base-uri 'none'; frame-ancestors 'none'",
		nonce, formAction,
	))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if mode == "" {
		mode = "login"
	}
	if err := authTmpl.Execute(w, tmplData{Data: data, ErrMsg: errMsg, Mode: mode, Nonce: nonce}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
