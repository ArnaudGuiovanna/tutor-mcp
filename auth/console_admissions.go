// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strconv"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type admissionConsoleStore interface {
	storeport.FormationAdmissionStore
	ListCohorts(context.Context, models.Principal, string, int) (models.CohortPage, error)
}

func (s *OAuthServer) HandleConsoleAdmissions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && !s.consolePost(w, r) {
		return
	}
	c, ok := s.requireConsole(w, r)
	if !ok {
		return
	}
	store, ok := s.store.(admissionConsoleStore)
	if !ok {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	cohortID := r.URL.Query().Get("cohort_id")
	if r.Method == http.MethodPost {
		cohortID = r.FormValue("cohort_id")
		memberID := r.FormValue("membership_id")
		if email := r.FormValue("email"); email != "" {
			var err error
			memberID, err = store.FindFormationLearner(r.Context(), c.principal, cohortID, email)
			if err != nil {
				http.Error(w, "active learner not found or access denied", http.StatusForbidden)
				return
			}
		}
		version, err := strconv.ParseInt(r.FormValue("expected_version"), 10, 64)
		if err != nil {
			http.Error(w, "invalid version", http.StatusBadRequest)
			return
		}
		_, _, err = store.DecideFormationAdmission(r.Context(), c.principal, r.FormValue("idempotency_key"), cohortID, memberID, r.FormValue("decision"), version)
		if err != nil {
			http.Error(w, "admission not updated; refresh the list and check your assignment and the admission policy", http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/console/admissions?cohort_id="+url.QueryEscape(cohortID), http.StatusSeeOther)
		return
	}
	cohorts, err := store.ListCohorts(r.Context(), c.principal, r.URL.Query().Get("cohort_after"), 50)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var admissions []models.FormationAdmission
	if cohortID != "" {
		admissions, err = store.ListFormationAdmissions(r.Context(), c.principal, cohortID, r.URL.Query().Get("after"), 50)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	csrf, ok := consoleCSRF(w, consoleCookiePath)
	if !ok {
		return
	}
	key, err := newOpaqueToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := struct {
		Institution, CSRFToken, Key, CohortID, NextCohort, NextAdmission string
		Cohorts                                                          []models.Cohort
		Admissions                                                       []models.FormationAdmission
	}{
		Institution: c.membership.TenantName, CSRFToken: csrf, Key: key, CohortID: cohortID, NextCohort: cohorts.NextAfter, Cohorts: cohorts.Items, Admissions: admissions}
	if len(admissions) == 50 {
		data.NextAdmission = admissions[len(admissions)-1].MembershipID
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	if err := admissionTmpl.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

var admissionTmpl = template.Must(template.New("admissions").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Formation admissions</title><style>body{font:16px/1.6 system-ui;max-width:900px;margin:3rem auto;padding:1rem}section{border:1px solid #ccd5df;padding:1rem;margin:1rem 0}button,input,select{font:inherit;padding:.4rem}label{display:block}a{color:#164b8c}</style></head><body>
<p>{{.Institution}}</p><h1>Formation admissions</h1><nav><a href="/console">Members and invitations</a></nav>
<form method="get" action="/console/admissions"><label for="cohort">Cohort</label><select id="cohort" name="cohort_id">{{range .Cohorts}}<option value="{{.ID}}" {{if eq .ID $.CohortID}}selected{{end}}>{{.Name}}</option>{{end}}</select><button type="submit">View admissions</button></form>
{{if .NextCohort}}<a href="/console/admissions?cohort_after={{.NextCohort}}">More cohorts</a>{{end}}
{{if .CohortID}}<section><h2>Invite a learner</h2><p>For cohorts whose formation uses invitation admission. The learner must already belong to this institution. No seat is reserved until they join.</p>
<form method="post" action="/console/admissions"><input type="hidden" name="csrf_token" value="{{.CSRFToken}}"><input type="hidden" name="idempotency_key" value="{{.Key}}-invite"><input type="hidden" name="cohort_id" value="{{.CohortID}}"><input type="hidden" name="decision" value="invited"><input type="hidden" name="expected_version" value="0"><label for="email">Learner email</label><input id="email" name="email" type="email" required maxlength="254"><button type="submit">Grant invitation</button></form></section>
{{range .Admissions}}<section><h2>{{.Email}}</h2><p>Status: {{.Status}}</p><form method="post" action="/console/admissions"><input type="hidden" name="csrf_token" value="{{$.CSRFToken}}"><input type="hidden" name="idempotency_key" value="{{$.Key}}-{{.MembershipID}}"><input type="hidden" name="cohort_id" value="{{$.CohortID}}"><input type="hidden" name="membership_id" value="{{.MembershipID}}"><input type="hidden" name="expected_version" value="{{.Version}}">
{{if eq .Status "pending"}}<button name="decision" value="approved">Approve</button> <button name="decision" value="rejected">Reject</button>{{else if eq .Status "revoked"}}{{if eq .EnrollmentPolicy "invitation"}}<button name="decision" value="invited">Renew invitation</button>{{else if eq .EnrollmentPolicy "approval"}}<button name="decision" value="approved">Restore approval</button>{{end}}{{else}}<button name="decision" value="revoked">Revoke future admission</button>{{end}}</form></section>{{else}}<p>No admission requests or invitations on this page.</p>{{end}}
{{if .NextAdmission}}<a href="/console/admissions?cohort_id={{.CohortID}}&amp;after={{.NextAdmission}}">More admissions</a>{{end}}{{end}}</body></html>`))
