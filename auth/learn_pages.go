// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"html/template"
	"net/http"
	"time"
	"tutor-mcp/models"
)

type learnPageData struct {
	Page, Title, Error, CSRFToken, RetryKey, MCPURL, Institution, Email string
	TenantOptions                                                       []tenantOption
	Formations                                                          []models.LearnerFormation
	NextAfter                                                           string
	Mine                                                                bool
	Detail                                                              *models.LearnerFormationDetail
	CredentialID, Secret                                                string
	RecoveryCodes                                                       []string
}

func learnerCanJoin(f models.LearnerFormation) bool {
	if f.FormationStatus != "active" || f.CohortStatus != "open" || (f.EndsAt != nil && !time.Now().Before(*f.EndsAt)) {
		return false
	}
	if f.Enrollment != nil && (f.Enrollment.Status != "cancelled" || f.Enrollment.DomainID == "" || f.Enrollment.SeatReserved) {
		return false
	}
	switch f.EnrollmentPolicy {
	case "invitation":
		return f.AdmissionStatus == "invited" && f.AvailableSeats > 0
	case "approval":
		return f.AdmissionStatus != "pending" && f.AdmissionStatus != "revoked" && (f.AdmissionStatus != "approved" || f.AvailableSeats > 0)
	case "open":
		return f.AvailableSeats > 0
	}
	return false
}

var learnTmpl = template.Must(template.New("learn").Funcs(template.FuncMap{"joinable": learnerCanJoin}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · tutor/mcp</title><style>
body{font:16px/1.6 system-ui,sans-serif;background:#f5f7f9;color:#182536;margin:0}main{max-width:900px;margin:3rem auto;padding:0 1.2rem}a{color:#164b8c}nav{display:flex;gap:1.3rem;flex-wrap:wrap;margin-bottom:1.5rem}article,section{padding:1.3rem;background:white;border:1px solid #d6dfe8;border-radius:12px;margin:1rem 0}label{display:block;margin-top:.8rem}input,select,button{font:inherit;padding:.55rem;border:1px solid #9caebe;border-radius:5px;max-width:100%;box-sizing:border-box}button{cursor:pointer;background:#164b8c;color:white;margin:.6rem 0}button.secondary{background:white;color:#164b8c}.error{padding:1rem;border-left:4px solid #ab2835;background:#fff0f1}.muted{color:#506275}.mono{font-family:monospace;overflow-wrap:anywhere}form.inline{display:inline-block;margin-right:1rem}ul{padding-left:1.4rem}h1,h2,h3{line-height:1.25}small{display:block}
</style></head><body><main><p>tutor/mcp · {{if .Institution}}{{.Institution}}{{else}}Learner portal{{end}}</p><h1>{{.Title}}</h1>
{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
{{if eq .Page "login"}}
<p>Sign in with the account you used to accept your institution's invitation.</p>
<form method="post" action="/learn/login"><input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
<label for="email">Email</label><input id="email" name="email" type="email" autocomplete="email" maxlength="254" value="{{.Email}}" required>
<label for="password">Password</label><input id="password" name="password" type="password" autocomplete="current-password" maxlength="72" required>
{{if .TenantOptions}}<label for="tenant">Institution</label><select id="tenant" name="tenant_id" required>{{range .TenantOptions}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select>{{end}}
<p><button type="submit">Sign in</button></p></form><p><a href="/console/login">Institution administration</a></p>
{{else if or (eq .Page "mfa") (eq .Page "setup")}}
{{if eq .Page "setup"}}<p>Add a time-based code to your authenticator using this secret: <span class="mono">{{.Secret}}</span>. Then enter the generated code.</p>{{end}}
<form method="post" action="/learn/mfa{{if eq .Page "setup"}}/setup{{end}}"><input type="hidden" name="csrf_token" value="{{.CSRFToken}}"><input type="hidden" name="credential_id" value="{{.CredentialID}}">
<label for="code">Authenticator or recovery code</label><input id="code" name="code" autocomplete="one-time-code" maxlength="32" required><p><button type="submit">Verify</button></p></form>
{{else if eq .Page "codes"}}<p>Store these codes somewhere safe. Each code works once.</p><ul>{{range .RecoveryCodes}}<li class="mono">{{.}}</li>{{end}}</ul><a href="/learn">Continue to your formations</a>
{{else}}
<nav aria-label="Learner navigation"><a href="/learn">Formation catalog</a><a href="/learn?mine=1">My formations</a></nav>
{{if .Email}}<p class="muted">{{.Email}}</p><form method="post" action="/learn/logout"><input type="hidden" name="csrf_token" value="{{.CSRFToken}}"><button class="secondary" type="submit">Sign out</button></form>{{end}}
{{range .Formations}}<article><h2><a href="/learn/formations/{{.CohortID}}">{{.Name}}</a></h2><p>{{.Description}}</p><p>{{.CohortName}} · Version {{.Version}} · {{.AvailableSeats}} seats available</p>
<p>Admission: {{.EnrollmentPolicy}}. Cohort: {{.CohortStatus}}.{{if eq .FormationStatus "archived"}} This formation is archived.{{end}}</p>
{{if .StartsAt}}<small>Starts: {{.StartsAt.Format "2006-01-02"}}</small>{{end}}{{if .EndsAt}}<small>Ends: {{.EndsAt.Format "2006-01-02"}}</small>{{end}}
{{if and (eq .EnrollmentPolicy "invitation") (ne .AdmissionStatus "invited") (not .Enrollment)}}<p>Ask your institution for an invitation to this cohort.</p>{{end}}
{{if .AdmissionStatus}}<p>Admission status: <strong>{{.AdmissionStatus}}</strong>{{if eq .AdmissionStatus "approved"}} — confirm your enrollment below.{{end}}</p>{{end}}
{{if .Enrollment}}<p>Your enrollment: <strong>{{.Enrollment.Status}}</strong></p>{{if eq .Enrollment.Status "active"}}<p>To continue, open your AI client connected to <span class="mono">{{$.MCPURL}}</span> and ask to resume <strong>{{.Name}}</strong> ({{.CohortName}}).</p>{{else if eq .Enrollment.Status "cancelled"}}<p>Your previous progress is saved. Rejoining this cohort restores it when admission and seats permit.</p>{{end}}{{end}}
{{if joinable .}}<form class="inline" method="post" action="/learn/enrollment"><input type="hidden" name="csrf_token" value="{{$.CSRFToken}}"><input type="hidden" name="idempotency_key" value="{{$.RetryKey}}-join-{{.CohortID}}"><input type="hidden" name="cohort_id" value="{{.CohortID}}"><input type="hidden" name="action" value="join"><button type="submit">{{if and (eq .EnrollmentPolicy "approval") (ne .AdmissionStatus "approved")}}Request enrollment{{else}}Join this cohort{{end}}</button></form>{{end}}
{{if or (eq .AdmissionStatus "pending") (and .Enrollment (eq .Enrollment.Status "active"))}}<form class="inline" method="post" action="/learn/enrollment"><input type="hidden" name="csrf_token" value="{{$.CSRFToken}}"><input type="hidden" name="idempotency_key" value="{{$.RetryKey}}-leave-{{.CohortID}}"><input type="hidden" name="cohort_id" value="{{.CohortID}}"><input type="hidden" name="action" value="leave"><button class="secondary" type="submit">{{if eq .AdmissionStatus "pending"}}Cancel request{{else}}Leave and keep my progress{{end}}</button></form>{{end}}</article>
{{else}}{{if eq .Page "catalog"}}<p>No formations to display yet.</p>{{end}}{{end}}
{{if .NextAfter}}<a href="/learn?after={{.NextAfter}}{{if .Mine}}&amp;mine=1{{end}}">Next page</a>{{end}}
{{with .Detail}}<section><h2>Program</h2>{{range .Modules}}<h3>{{.Title}}</h3>{{end}}{{range .Concepts}}<h3>{{.Label}}</h3><p>{{.Description}}</p>{{if .Outcomes}}<ul>{{range .Outcomes}}<li>{{.Statement}}</li>{{end}}</ul>{{end}}{{if .Criteria}}<p>Assessment criteria:</p><ul>{{range .Criteria}}<li>{{.Description}}</li>{{end}}</ul>{{end}}{{if .Prerequisites}}<p>Prerequisites: {{range .Prerequisites}}<span>{{.}} </span>{{end}}</p>{{end}}{{end}}</section>{{end}}
{{end}}</main></body></html>`))

func renderLearnPage(w http.ResponseWriter, status int, data learnPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := learnTmpl.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
