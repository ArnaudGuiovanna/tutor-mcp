// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"html/template"
	"net/http"
	"time"
)

type consoleMemberRow struct {
	MembershipID string
	Email        string
	Status       string
	MFAEnrolled  bool
	Self         bool
	Roles        map[string]bool
	RoleList     string
}

type consoleInvitationRow struct {
	ID        string
	Email     string
	Roles     string
	ExpiresAt time.Time
}

type consoleInviteLink struct {
	Email  string
	Mailed bool
	Error  string
}

type consolePageData struct {
	Page      string
	Title     string
	Message   string
	Error     string
	CSRFToken string
	Token     string

	// Signed-in header.
	Institution string
	UserEmail   string

	// Login.
	Email         string
	TenantOptions []tenantOption

	// MFA setup.
	CredentialID  string
	Secret        string
	OTPAuthURI    string
	QRDataURI     template.URL
	RecoveryCodes []string

	// Members.
	CanManage   bool
	IsOwner     bool
	Members     []consoleMemberRow
	Invitations []consoleInvitationRow
	InviteLinks []consoleInviteLink
	RoleOptions []string
	MCPURL      string

	// Invitation and signup.
	Roles        string
	ExistingUser bool
	TenantName   string
	TenantSlug   string
	SignupOpen   bool
}

var consoleTmpl = template.Must(template.New("console").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} — tutor/mcp</title>
  <style>
    :root { color-scheme: light; font-family: ui-sans-serif, system-ui, sans-serif; background: #f8f5f0; color: #211f1c; }
    body { margin: 0; padding: 1.25rem; }
    header { max-width: 60rem; margin: 0 auto 1rem; display: flex; gap: 1rem; align-items: center; justify-content: space-between; flex-wrap: wrap; }
    header form { margin: 0; }
    header button { width: auto; margin: 0; padding: .45rem .9rem; background: #5b554d; }
    main { max-width: 28rem; margin: 0 auto; background: white; border: 1px solid #ddd6cc; border-radius: 1rem; padding: 2rem; box-shadow: 0 1rem 3rem #33221112; }
    main.wide { max-width: 60rem; }
    h1 { margin-top: 0; font-size: 1.55rem; }
    h2 { font-size: 1.15rem; margin-top: 2rem; }
    p, li { color: #5b554d; line-height: 1.55; }
    label { display: block; margin: 1rem 0 .35rem; font-weight: 650; }
    input, select, textarea { width: 100%; box-sizing: border-box; padding: .7rem; border: 1px solid #aaa096; border-radius: .55rem; font: inherit; }
    textarea { min-height: 6rem; }
    .inline { display: inline-flex; gap: .35rem; align-items: center; margin: .2rem .8rem .2rem 0; font-weight: 500; }
    .inline input { width: auto; }
    button { width: 100%; margin-top: 1.2rem; border: 0; border-radius: .55rem; padding: .75rem; color: white; background: #c65f35; font: inherit; font-weight: 700; cursor: pointer; }
    table button, table select { width: auto; margin: 0; padding: .4rem .7rem; }
    table { width: 100%; border-collapse: collapse; font-size: .92rem; }
    th, td { text-align: left; padding: .55rem .4rem; border-bottom: 1px solid #eee6dc; vertical-align: top; }
    .error { padding: .75rem; border-radius: .55rem; background: #fdecea; color: #8a1f11; }
    .notice { padding: .75rem; border-radius: .55rem; background: #eaf6ec; color: #1d5a2a; }
    .mono { font-family: ui-monospace, monospace; overflow-wrap: anywhere; }
    .codes { columns: 2; font-family: ui-monospace, monospace; font-size: 1.05rem; }
    .qr { display: block; margin: 1rem auto; width: 12rem; height: 12rem; image-rendering: pixelated; }
    a { color: #99451f; }
  </style>
</head>
<body>
{{if .UserEmail}}
<header>
  <div><strong>{{.Institution}}</strong> · {{.UserEmail}}</div>
  <form method="post" action="/console/logout">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <button type="submit">Sign out</button>
  </form>
</header>
{{end}}
<main{{if eq .Page "members"}} class="wide"{{end}}>
  <h1>{{.Title}}</h1>
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  {{if .Message}}<p class="notice">{{.Message}}</p>{{end}}

  {{if eq .Page "login"}}
  <p>Sign in to administer your institution. Learners use their AI client instead.</p>
  <form method="post" action="/console/login">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <label for="email">Email</label>
    <input id="email" name="email" type="email" autocomplete="username" maxlength="254" value="{{.Email}}" required>
    <label for="password">Password</label>
    <input id="password" name="password" type="password" autocomplete="current-password" maxlength="72" required>
    {{if .TenantOptions}}
    <label for="tenant">Institution</label>
    <select id="tenant" name="tenant_id" required>
      <option value="">Choose an institution</option>
      {{range .TenantOptions}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
    </select>
    {{end}}
    <button type="submit">Sign in</button>
  </form>
  <p><a href="/recover">Forgot your password?</a>{{if .SignupOpen}} · <a href="/signup">Create an institution</a>{{end}}</p>
  {{end}}

  {{if eq .Page "mfa"}}
  <p>Enter the 6-digit code from your authenticator app, or one of your recovery codes.</p>
  <form method="post" action="/console/mfa">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <label for="code">Code</label>
    <input id="code" name="code" autocomplete="one-time-code" inputmode="text" maxlength="16" required autofocus>
    <button type="submit">Verify</button>
  </form>
  {{end}}

  {{if eq .Page "setup"}}
  <p>Your role requires two-factor authentication. Scan this code with an authenticator app (for example Google Authenticator, Microsoft Authenticator, 1Password or Aegis), then enter the 6-digit code it shows.</p>
  {{if .QRDataURI}}<img class="qr" src="{{.QRDataURI}}" alt="QR code for your authenticator app">{{end}}
  <p>Cannot scan? Enter this key manually: <span class="mono">{{.Secret}}</span></p>
  <form method="post" action="/console/mfa/setup">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <input type="hidden" name="credential_id" value="{{.CredentialID}}">
    <label for="code">6-digit code</label>
    <input id="code" name="code" autocomplete="one-time-code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required>
    <button type="submit">Enable two-factor authentication</button>
  </form>
  {{end}}

  {{if eq .Page "codes"}}
  <p>Two-factor authentication is on. Store these recovery codes somewhere safe. Each code works once if you lose your authenticator. They will not be shown again.</p>
  <ul class="codes">{{range .RecoveryCodes}}<li>{{.}}</li>{{end}}</ul>
  <p><a href="/console">Continue to the console</a></p>
  {{end}}

  {{if eq .Page "members"}}
  <p><a href="/console/progress">Learning progress</a> · <a href="/console/admissions">Formation admissions</a> · <a href="/learn">Learner portal</a></p>
  <p>Learners and trainers connect their AI client (Claude, ChatGPT…) to <span class="mono">{{.MCPURL}}</span> and sign in with the email of their invitation.</p>
  {{if .CanManage}}
  {{if .InviteLinks}}
  <h2>Invitations sent</h2>
  <table>
    <tr><th>Email</th><th>Status</th></tr>
    {{range .InviteLinks}}<tr><td>{{.Email}}</td><td>{{if .Mailed}}emailed, valid 7 days{{else}}{{.Error}}{{end}}</td></tr>{{end}}
  </table>
  {{end}}
  <h2>Invite people</h2>
  <form method="post" action="/console/members/invite">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <label for="emails">Emails (one per line, up to 50)</label>
    <textarea id="emails" name="emails" required></textarea>
    <label>Roles</label>
    {{range .RoleOptions}}<span class="inline"><input type="checkbox" name="roles" value="{{.}}" id="invite-{{.}}"{{if eq . "learner"}} checked{{end}}><span>{{.}}</span></span>{{end}}
    <button type="submit">Create invitations</button>
  </form>
  {{if .Invitations}}
  <h2>Pending invitations</h2>
  <table>
    <tr><th>Email</th><th>Roles</th><th>Expires</th><th></th></tr>
    {{range .Invitations}}<tr><td>{{.Email}}</td><td>{{.Roles}}</td><td>{{.ExpiresAt.Format "2006-01-02"}}</td>
      <td><form method="post" action="/console/invitations/revoke"><input type="hidden" name="csrf_token" value="{{$.CSRFToken}}"><input type="hidden" name="invitation_id" value="{{.ID}}"><button type="submit">Revoke</button></form></td></tr>{{end}}
  </table>
  {{end}}
  <h2>Members</h2>
  <table>
    <tr><th>Email</th><th>Roles and status</th><th>2FA</th></tr>
    {{range .Members}}{{$member := .}}<tr><td>{{.Email}}{{if .Self}} (you){{end}}</td>
      <td><form method="post" action="/console/members/update">
        <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
        <input type="hidden" name="membership_id" value="{{.MembershipID}}">
        {{range $.RoleOptions}}<span class="inline"><input type="checkbox" name="roles" value="{{.}}"{{if index $member.Roles .}} checked{{end}}><span>{{.}}</span></span>{{end}}
        <select name="status">
          <option value="active"{{if eq .Status "active"}} selected{{end}}>active</option>
          <option value="suspended"{{if eq .Status "suspended"}} selected{{end}}>suspended</option>
          <option value="revoked">remove</option>
        </select>
        <button type="submit">Save</button>
      </form></td>
      <td>{{if .MFAEnrolled}}on{{else}}off{{end}}</td></tr>{{end}}
  </table>
  {{else}}
  <p>Your role does not manage members. Learner dashboards arrive in a later release.</p>
  {{end}}
  {{end}}

  {{if eq .Page "invite"}}
  <p>You are invited to join <strong>{{.TenantName}}</strong> as <strong>{{.Roles}}</strong> with the email <strong>{{.Email}}</strong>.</p>
  <form method="post" action="/invite">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <input type="hidden" name="token" value="{{.Token}}">
    {{if .ExistingUser}}
    <p>You already have a tutor/mcp account. Enter its password to accept.</p>
    <label for="password">Password</label>
    <input id="password" name="password" type="password" autocomplete="current-password" maxlength="72" required>
    {{else}}
    <label for="password">Choose your password</label>
    <input id="password" name="password" type="password" autocomplete="new-password" minlength="12" maxlength="72" required>
    <label for="password-confirm">Confirm password</label>
    <input id="password-confirm" name="password_confirm" type="password" autocomplete="new-password" minlength="12" maxlength="72" required>
    {{end}}
    <button type="submit">Accept the invitation</button>
  </form>
  {{end}}

  {{if eq .Page "joined"}}
  <p>You joined <strong>{{.TenantName}}</strong>. In your AI client (Claude, ChatGPT…), add the MCP connector <span class="mono">{{.MCPURL}}</span> and sign in with <strong>{{.Email}}</strong>.</p>
  <p><a href="/learn">Browse formations in your learning space</a></p>
  {{end}}

  {{if eq .Page "signup"}}
  <p>Create your institution. You become its owner and can then invite trainers and learners.</p>
  <form method="post" action="/signup">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <label for="tenant-name">Institution name</label>
    <input id="tenant-name" name="tenant_name" maxlength="120" value="{{.TenantName}}" required>
    <label for="tenant-slug">Identifier</label>
    <input id="tenant-slug" name="tenant_slug" pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]" minlength="3" maxlength="63" value="{{.TenantSlug}}" required>
    <p>3–63 lowercase letters, digits or hyphens, for example <span class="mono">lycee-victor-hugo</span>.</p>
    <label for="email">Your email</label>
    <input id="email" name="email" type="email" autocomplete="email" maxlength="254" value="{{.Email}}" required>
    <label class="inline" for="terms"><input id="terms" type="checkbox" name="accept_terms" value="yes" required><span>I accept the terms of service and act on behalf of this institution.</span></label>
    <button type="submit">Send the confirmation email</button>
  </form>
  {{end}}

  {{if eq .Page "signup-complete"}}
  <p>Confirm the creation of <strong>{{.TenantName}}</strong> (<span class="mono">{{.TenantSlug}}</span>) for <strong>{{.Email}}</strong>.</p>
  <form method="post" action="/signup/complete">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <input type="hidden" name="token" value="{{.Token}}">
    {{if .ExistingUser}}
    <p>You already have a tutor/mcp account. Enter its password to confirm.</p>
    <label for="password">Password</label>
    <input id="password" name="password" type="password" autocomplete="current-password" maxlength="72" required>
    {{else}}
    <label for="password">Choose your password</label>
    <input id="password" name="password" type="password" autocomplete="new-password" minlength="12" maxlength="72" required>
    <label for="password-confirm">Confirm password</label>
    <input id="password-confirm" name="password_confirm" type="password" autocomplete="new-password" minlength="12" maxlength="72" required>
    {{end}}
    <button type="submit">Create the institution</button>
  </form>
  {{end}}
</main>
</body>
</html>`))

func renderConsolePage(w http.ResponseWriter, status int, data consolePageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := consoleTmpl.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
