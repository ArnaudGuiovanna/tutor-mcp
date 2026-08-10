// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"fmt"
	"html/template"
	"net/http"
)

type accountPageData struct {
	Title            string
	Message          string
	Token            string
	CSRFToken        string
	ClientName       string
	ClientID         string
	RedirectOrigin   string
	ScopeDescription string
	ShowVerify       bool
	ShowRecover      bool
	ShowReset        bool
	ShowBackToLogin  bool
}

var accountTmpl = template.Must(template.New("account").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} — tutor/mcp</title>
  <style>
    :root { color-scheme: light; font-family: ui-sans-serif, system-ui, sans-serif; background: #f8f5f0; color: #211f1c; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 1.25rem; }
    main { width: min(100%, 28rem); background: white; border: 1px solid #ddd6cc; border-radius: 1rem; padding: 2rem; box-shadow: 0 1rem 3rem #33221112; }
    h1 { margin-top: 0; font-size: 1.55rem; }
    p { color: #5b554d; line-height: 1.55; }
    label { display: block; margin: 1rem 0 .35rem; font-weight: 650; }
    input { width: 100%; box-sizing: border-box; padding: .75rem; border: 1px solid #aaa096; border-radius: .55rem; font: inherit; }
    .client { padding: .85rem; border: 1px solid #ddd6cc; border-radius: .65rem; background: #f8f5f0; overflow-wrap: anywhere; }
    .consent { display: flex; gap: .65rem; align-items: flex-start; font-weight: 600; }
    .consent input { width: auto; margin-top: .2rem; }
    button { width: 100%; margin-top: 1.2rem; border: 0; border-radius: .55rem; padding: .8rem; color: white; background: #c65f35; font: inherit; font-weight: 700; cursor: pointer; }
    a { color: #99451f; }
  </style>
</head>
<body><main>
  <h1>{{.Title}}</h1>
  {{if .Message}}<p>{{.Message}}</p>{{end}}
  {{if .ShowVerify}}
  <form method="post" action="/verify-email">
    <input type="hidden" name="token" value="{{.Token}}">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <div class="client">
      <strong>{{.ClientName}}</strong><br>
      Client ID: {{.ClientID}}<br>
      Authorization code destination: {{.RedirectOrigin}}<br>
      <strong>Requested permissions:</strong> {{.ScopeDescription}}
    </div>
    <p>Client names are self-declared. Verify the client ID and destination before continuing.</p>
    <label for="verify-password">Choose your password</label>
    <input id="verify-password" name="password" type="password" autocomplete="new-password" minlength="12" maxlength="72" required>
    <label for="verify-password-confirm">Confirm password</label>
    <input id="verify-password-confirm" name="password_confirm" type="password" autocomplete="new-password" minlength="12" maxlength="72" required>
    <label class="consent" for="verify-consent">
      <input id="verify-consent" type="checkbox" name="approve_client" value="yes" required>
      <span>I initiated this request and authorize this exact client and destination.</span>
    </label>
    <button type="submit">Set password, verify email, and authorize client</button>
  </form>
  {{end}}
  {{if .ShowRecover}}
  <form method="post" action="/recover">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <label for="email">Email</label>
    <input id="email" name="email" type="email" autocomplete="email" maxlength="254" required>
    <button type="submit">Send reset link</button>
  </form>
  {{end}}
  {{if .ShowReset}}
  <form method="post" action="/reset-password">
    <input type="hidden" name="token" value="{{.Token}}">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <label for="password">New password</label>
    <input id="password" name="password" type="password" autocomplete="new-password" minlength="12" maxlength="72" required>
    <label for="password-confirm">Confirm password</label>
    <input id="password-confirm" name="password_confirm" type="password" autocomplete="new-password" minlength="12" maxlength="72" required>
    <button type="submit">Reset password</button>
  </form>
  {{end}}
  {{if .ShowBackToLogin}}<p><a href="/authorize">Return to sign in</a></p>{{end}}
</main></body></html>`))

func renderAccountPage(w http.ResponseWriter, status int, data accountPageData) {
	if data.ShowVerify {
		if data.ClientName == "" {
			data.ClientName = data.ClientID
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	formAction := "'self'"
	if data.ShowVerify && data.RedirectOrigin != "" {
		formAction += " " + data.RedirectOrigin
	}
	w.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'none'; style-src 'unsafe-inline'; form-action %s; base-uri 'none'; frame-ancestors 'none'", formAction))
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := accountTmpl.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
