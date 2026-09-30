// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"html/template"
	"net/http"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func (s *OAuthServer) HandleLearnerBadges(w http.ResponseWriter, r *http.Request) {
	s.handleBadges(w, r, false)
}
func (s *OAuthServer) HandleConsoleBadges(w http.ResponseWriter, r *http.Request) {
	s.handleBadges(w, r, true)
}

func (s *OAuthServer) handleBadges(w http.ResponseWriter, r *http.Request, staff bool) {
	var c *consoleContext
	var ok bool
	if staff {
		c, ok = s.requireConsole(w, r)
	} else {
		c, ok = s.learnerSession(w, r, true)
	}
	if !ok {
		return
	}
	p := c.principal
	base, back := "/learn/badges", "/learn?mine=1"
	if staff {
		p.Scopes = []string{models.OAuthScopeProgressRead}
		base, back = "/console/progress/badges", "/console/progress"
	}
	source, ok := s.store.(storeport.BadgeStore)
	if !ok {
		http.Error(w, "Badges unavailable", http.StatusServiceUnavailable)
		return
	}
	enrollment := r.URL.Query().Get("enrollment_id")
	page, err := source.GetEnrollmentBadges(r.Context(), p, enrollment, r.URL.Query().Get("after"), 30, staff)
	progressPageHeaders(w)
	if err != nil {
		status := http.StatusServiceUnavailable
		switch storeport.PublicProgressError(err).Code {
		case "forbidden":
			status = http.StatusForbidden
		case "not_found":
			status = http.StatusNotFound
		case "invalid_request":
			status = http.StatusBadRequest
		}
		http.Error(w, "Badges unavailable for this enrollment", status)
		return
	}
	data := struct {
		Institution, Enrollment, Base, Back string
		Page                                models.BadgePage
	}{c.membership.TenantName, enrollment, base, back, page}
	if err := badgesTmpl.Execute(w, data); err != nil {
		s.logger.Error("badges page render failed", "error_type", "template")
	}
}

var badgesTmpl = template.Must(template.New("badges").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Learning badges</title><style>
body{font:16px/1.6 system-ui;max-width:900px;margin:2rem auto;padding:1rem;color:#182c40;background:#f8fafc}a{color:#164b8c}article{background:white;border:1px solid #ccd5df;padding:1.2rem;margin:1rem 0;border-radius:.7rem}h1,h2{line-height:1.3}code{overflow-wrap:anywhere}.medal{font-size:2rem;color:#8a6400}summary{cursor:pointer}small{color:#465c70}
</style></head><body><p>{{.Institution}}</p><nav><a href="{{.Back}}">Back to formations</a></nav><h1>Learning badges</h1>
<p>These badges record dated achievements. A mastery badge recognizes a successful challenge; a formation badge recognizes every concept in its published program. Memory badges recognize successful FSRS recalls over 7, 30 or 90 days. Keep reviewing to maintain your memory.</p>
{{range .Page.Items}}<article><span class="medal" aria-hidden="true">&#9733;</span><h2>{{if eq .Kind "mastery"}}Mastery challenge passed{{else if eq .Kind "formation_completed"}}Formation completed{{else}}Memory maintained · {{.RetentionDays}} days{{end}}</h2><p>{{.Label}}</p><p>Achieved {{.AchievedAt.Format "2006-01-02"}}</p>
{{if ne .EvidenceStatus "valid"}}<p>Historical achievement: some supporting evidence has changed or is no longer available.</p>{{end}}
<details><summary>Evidence and version</summary><p>Published version: <code>{{.VersionID}}</code>. Rule: <code>{{.PolicyVersion}}</code>.</p><ul>{{range .Evidence}}<li>Response {{.RespondedAt.Format "2006-01-02 15:04 UTC"}} · assessment <code>{{.AttemptID}}</code> · {{.EvaluationMethod}}</li>{{end}}</ul></details></article>{{else}}<p>No badges earned for this enrollment yet.</p>{{end}}
{{if .Page.NextAfter}}<a href="{{.Base}}?enrollment_id={{.Enrollment}}&amp;after={{.Page.NextAfter}}">Next badges</a>{{end}}<p><small>Badges describe recorded learning evidence and do not confer an external qualification.</small></p></body></html>`))
