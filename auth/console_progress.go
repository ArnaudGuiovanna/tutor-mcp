// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type progressPageData struct {
	CSRFToken           string
	Institution, MCPURL string
	Cohorts             models.TrainerCohortPage
	Insights            *models.CohortInsights
	Statistics          *models.CohortStatistics
	Learner             *models.LearnerProgress
	After, ConceptAfter string
}

func (s *OAuthServer) HandleConsoleProgress(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireConsole(w, r)
	if !ok {
		return
	}
	// This internal read capability is attached only after validating the
	// independent console cookie and MFA. It is never issued as an OAuth token.
	p := c.principal
	p.Scopes = []string{models.OAuthScopeProgressRead}
	if !p.CanReadInstitutionProgress() {
		writeProgressPageError(w, storeport.ErrInvalidPrincipal)
		return
	}
	store, ok := s.store.(storeport.ProgressStore)
	if !ok {
		writeProgressPageError(w, storeport.ErrProgressUnavailable)
		return
	}
	q := r.URL.Query()
	data := progressPageData{Institution: c.membership.TenantName, MCPURL: MCPResource(s.baseURL), After: q.Get("after"), ConceptAfter: q.Get("concept_after")}
	var err error
	switch {
	case q.Get("enrollment_id") != "":
		data.Learner, err = store.GetLearnerProgress(r.Context(), p, q.Get("enrollment_id"), data.After, 50)
	case q.Get("cohort_id") != "" && q.Get("view") == "statistics":
		stats, ok := s.store.(storeport.StatisticsStore)
		if !ok {
			writeProgressPageError(w, storeport.ErrProgressUnavailable)
			return
		}
		data.Statistics, err = stats.GetCohortStatistics(r.Context(), p, q.Get("cohort_id"), data.ConceptAfter, 50)
	case q.Get("cohort_id") != "":
		data.Insights, err = store.GetCohortInsights(r.Context(), p, q.Get("cohort_id"), data.After, data.ConceptAfter, 50)
	default:
		data.Cohorts, err = store.ListTrainerCohorts(r.Context(), p, data.After, 50)
	}
	if err != nil {
		if storeport.PublicProgressError(err).Retryable {
			s.logger.Error("console progress read failed", "error_type", "store")
		}
		writeProgressPageError(w, err)
		return
	}
	if data.Insights != nil && data.Insights.CanManageAssignments {
		var ok bool
		data.CSRFToken, ok = consoleCSRF(w, consoleCookiePath)
		if !ok {
			return
		}
	}
	progressPageHeaders(w)
	if err := progressPageTmpl.Execute(w, data); err != nil {
		s.logger.Error("console progress render failed", "error_type", "template")
	}
}

func (s *OAuthServer) HandleConsoleProgressAssignment(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	c, ok := s.requireConsole(w, r)
	if !ok {
		return
	}
	store, ok := s.store.(storeport.ProgressAssignmentStore)
	if !ok {
		writeProgressPageError(w, storeport.ErrProgressUnavailable)
		return
	}
	decision := r.FormValue("assigned")
	if decision != "true" && decision != "false" {
		writeProgressPageError(w, storeport.ErrInvalidProgressRequest)
		return
	}
	cohort := r.FormValue("cohort_id")
	if err := store.SetCohortTrainerAssignment(r.Context(), c.principal, cohort, r.FormValue("email"), decision == "true"); err != nil {
		writeProgressPageError(w, err)
		return
	}
	http.Redirect(w, r, "/console/progress?cohort_id="+url.QueryEscape(cohort), http.StatusSeeOther)
}

func progressPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
}

func writeProgressPageError(w http.ResponseWriter, err error) {
	public := storeport.PublicProgressError(err)
	status := http.StatusServiceUnavailable
	switch public.Code {
	case "invalid_request":
		status = http.StatusBadRequest
	case "forbidden":
		status = http.StatusForbidden
	case "not_found":
		status = http.StatusNotFound
	}
	progressPageHeaders(w)
	http.Error(w, public.Code+": "+public.Message, status)
}

var progressPageTmpl = template.Must(template.New("progress").Funcs(template.FuncMap{
	"mastery": func(value *float64) string {
		if value == nil {
			return "No estimate"
		}
		return fmt.Sprintf("%.0f%%", *value*100)
	},
	"date": func(value *time.Time) string {
		if value == nil {
			return "—"
		}
		return value.UTC().Format("2006-01-02 15:04 UTC")
	},
	"statint": func(cell models.StatInt) string {
		if cell.Value == nil {
			return "Insufficient data"
		}
		return fmt.Sprintf("%d", *cell.Value)
	},
	"statmastery": func(cell models.StatFloat) string {
		if cell.Value == nil {
			return "Insufficient data"
		}
		return fmt.Sprintf("%.0f%%", *cell.Value*100)
	},
	"weight": func(w *models.CollectiveWeight) string {
		if w == nil {
			return "None (fewer than 30 qualifying learners)"
		}
		return fmt.Sprintf("v%d · %d learners · learn %.2f, forget %.2f, slip %.2f, guess %.2f", w.Version, w.Contributors, w.PLearn, w.PForget, w.PSlip, w.PGuess)
	},
	"badgekind": func(b models.BadgeStatistics) string {
		switch {
		case b.Kind == models.BadgeMastery:
			return "Mastery challenge passed"
		case b.Kind == models.BadgeFormation:
			return "Formation completed"
		}
		return fmt.Sprintf("Memory maintained — %d days", b.RetentionDays)
	},
	"signal": func(value string) string {
		switch value {
		case "no_reviews":
			return "No concept reviews recorded yet"
		case "historical_estimates_unavailable":
			return "Some historical estimates are unavailable because their records changed after migration."
		case "no_review_in_14_days":
			return "No concept review in the last 14 days"
		case "reviewed_concepts_below_mastery_threshold":
			return "Some reviewed concepts remain below the 80% mastery estimate"
		}
		return ""
	},
}).Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Learning progress</title><style>
body{font:16px/1.6 system-ui;max-width:1100px;margin:2rem auto;padding:1rem;color:#182c40;background:#f8fafc}a{color:#164b8c}nav{display:flex;gap:1rem;flex-wrap:wrap}section,details{background:white;border:1px solid #ccd5df;padding:1rem;margin:1rem 0;border-radius:.5rem}table{width:100%;border-collapse:collapse}th,td{text-align:left;vertical-align:top;border-bottom:1px solid #ccd5df;padding:.6rem}.scroll{overflow-x:auto}.muted{color:#465c70}code{overflow-wrap:anywhere}caption{text-align:left;margin-bottom:.5rem}h1,h2{line-height:1.3}
</style></head><body><p>{{.Institution}}</p><h1>Learning progress</h1><nav><a href="/console">Institution console</a><a href="/console/progress">Cohorts</a></nav>
<p class="muted">Staff access is limited to your institution and current assignments. Mastery estimates describe recorded reviews; they are not grades or certifications. Missing reviews mean insufficient evidence.</p>
{{if .Learner}}{{with .Learner}}
<p><a href="/console/progress?cohort_id={{.Cohort.CohortID}}">{{.Cohort.FormationName}} — {{.Cohort.CohortName}}</a> · Version {{.Cohort.Version}}</p>
<section><h2>{{.Learner.Email}}</h2><p><a href="/console/progress/badges?enrollment_id={{.Learner.EnrollmentID}}">Learning badges</a></p><p>Enrollment: <code>{{.Learner.EnrollmentID}}</code> · {{.Learner.Status}}</p><p>{{.Learner.ObservedConceptCount}} / {{.Learner.ConceptCount}} concepts reviewed; {{.Learner.MasteredConceptCount}} at or above 80%. Average over reviewed concepts: {{mastery .Learner.AverageMastery}}.</p><p>Last concept review: {{date .Learner.LastReviewAt}}</p>
{{range .Learner.AttentionSignals}}<p>{{signal .}}</p>{{end}}
<p>{{.InteractionCount}} recorded interactions; {{.EvaluatedAttemptCount}} current evaluated attempts, including {{.TrustedEvaluationCount}} trusted evaluations. An interaction count is not a count of successful assessments.</p></section>
<section class="scroll"><h2>Concept progress</h2><table><caption>This page of the published program</caption><thead><tr><th>Concept</th><th>Module</th><th>Mastery estimate</th><th>Reviews</th><th>Last review</th><th>Next review</th></tr></thead><tbody>
{{range .Concepts}}<tr><th scope="row">{{.Label}}<br><small>{{.StableKey}}</small></th><td>{{.ModuleTitle}}</td><td>{{mastery .Mastery}}</td><td>{{.ReviewCount}}</td><td>{{date .LastReviewAt}}</td><td>{{date .NextReviewAt}}</td></tr>{{else}}<tr><td colspan="6">No concepts on this page.</td></tr>{{end}}</tbody></table>
{{if .NextAfter}}<a href="/console/progress?enrollment_id={{.Learner.EnrollmentID}}&amp;after={{.NextAfter}}">Next concepts</a>{{end}}</section>
<section class="scroll"><h2>Recent sessions</h2><p>Up to {{.RecentSessionLimit}} most recent sessions for this enrollment. Session status is recorded state; an open session does not prove the learner is online.</p><table><thead><tr><th>Session</th><th>Status</th><th>Started</th><th>Last activity</th></tr></thead><tbody>{{range .RecentSessions}}<tr><td><code>{{.SessionID}}</code></td><td>{{.Status}}</td><td>{{.StartedAt.Format "2006-01-02 15:04 UTC"}}</td><td>{{.LastActiveAt.Format "2006-01-02 15:04 UTC"}}</td></tr>{{else}}<tr><td colspan="4">No sessions recorded.</td></tr>{{end}}</tbody></table></section>
<p class="muted">Read at {{.GeneratedAt.Format "2006-01-02 15:04 UTC"}}.</p>
{{end}}{{else if .Statistics}}{{with .Statistics}}
<p><a href="/console/progress?cohort_id={{.Cohort.CohortID}}">{{.Cohort.FormationName}} — {{.Cohort.CohortName}}</a> · Version {{.Cohort.Version}}</p>
<h2>Collective statistics</h2>
<p class="muted">Anonymous figures computed by a background worker about once an hour; they lag recent learning activity. Figures concerning fewer than {{.MinimumContributors}} learners, and groups that would reveal them by subtraction, are withheld. Mastery is an estimate, not a grade or certification.</p>
{{if eq .Status "not_yet_computed"}}<section><p>These statistics have not been computed yet. Try again later.</p></section>
{{else}}<p>Computed at {{date .ComputedAt}}.{{if .Stale}} <strong>This snapshot is older than six hours and may be out of date.</strong>{{end}}</p>
{{if eq .Status "insufficient_data"}}<section><p>Fewer than {{.MinimumContributors}} learners with recorded reviews: no collective figure is available for this cohort.</p></section>{{else}}
<section><table><tbody><tr><th scope="row">Learners with recorded reviews</th><td>{{statint .Contributors}}</td></tr><tr><th scope="row">Active or completed learners</th><td>{{statint .Participants}}</td></tr><tr><th scope="row">Learners who completed the formation</th><td>{{statint .CompletedLearners}}</td></tr><tr><th scope="row">Mean of learner mastery estimates</th><td>{{statmastery .MeanMastery}}</td></tr><tr><th scope="row">Median of learner mastery estimates</th><td>{{statmastery .MedianMastery}}</td></tr></tbody></table></section>
<section class="scroll"><h2>Learning badges</h2><table><thead><tr><th>Badge</th><th>Learners</th></tr></thead><tbody>{{range .Badges}}<tr><th scope="row">{{badgekind .}}</th><td>{{statint .Learners}}</td></tr>{{end}}</tbody></table></section>
<section class="scroll"><h2>Concepts</h2><table><thead><tr><th>Concept</th><th>Observed learners</th><th>Mean</th><th>Median</th><th>Collective BKT weight</th></tr></thead><tbody>{{range .Concepts}}<tr><th scope="row">{{.Label}}</th><td>{{statint .ObservedLearners}}</td><td>{{statmastery .MeanMastery}}</td><td>{{statmastery .MedianMastery}}</td><td>{{weight .CollectiveWeight}}</td></tr>{{else}}<tr><td colspan="5">No concepts on this page.</td></tr>{{end}}</tbody></table>
<p class="muted">A collective weight is the median of at least thirty learners' individualized BKT parameters for the concept, published as an immutable version. It reaches each learner once per version, at their next recorded interaction, and never changes mastery estimates or review schedules.</p>
{{if .NextConceptAfter}}<a href="/console/progress?cohort_id={{.Cohort.CohortID}}&amp;view=statistics&amp;concept_after={{.NextConceptAfter}}">Next concepts</a>{{end}}</section>{{end}}{{end}}
{{end}}{{else if .Insights}}{{with .Insights}}
<h2>{{.Cohort.FormationName}} — {{.Cohort.CohortName}}</h2><p>Version {{.Cohort.Version}} · Cohort {{.Cohort.CohortStatus}} · Formation {{.Cohort.FormationStatus}}</p><p>Whole cohort: {{.Cohort.EnrollmentCount}} enrollments, {{.Cohort.ActiveCount}} active, {{.Cohort.CompletedCount}} completed.</p>
<p><a href="/console/progress?cohort_id={{.Cohort.CohortID}}&amp;view=statistics">Collective statistics</a></p>
{{if .CanManageAssignments}}<section><h2>Trainer access</h2><p>Assign an existing active trainer in this institution to this cohort. Revoking this assignment takes effect on the next read; other role-based access still applies.</p><ul>{{range .Trainers}}<li>{{.Email}}</li>{{else}}<li>No assigned trainers.</li>{{end}}</ul>{{if .TrainersTruncated}}<p>The first 100 assignments are shown. Use the email form to revoke any assignment.</p>{{end}}
<form method="post" action="/console/progress/trainers"><input type="hidden" name="csrf_token" value="{{$.CSRFToken}}"><input type="hidden" name="cohort_id" value="{{.Cohort.CohortID}}"><label for="trainer-email">Trainer email</label> <input id="trainer-email" type="email" name="email" required maxlength="254"> <button name="assigned" value="true">Grant access</button> <button name="assigned" value="false">Revoke access</button></form></section>{{end}}
<section class="scroll"><h2>Learner follow-up</h2><table><caption>This page of enrollments, including historical records</caption><thead><tr><th>Learner</th><th>Status</th><th>Reviewed concepts</th><th>Mastery estimate</th><th>Last review</th><th>Follow-up signals</th></tr></thead><tbody>
{{range .Learners}}<tr><th scope="row"><a href="/console/progress?enrollment_id={{.EnrollmentID}}">{{.Email}}</a></th><td>{{.Status}}</td><td>{{.ObservedConceptCount}} / {{.ConceptCount}}</td><td>{{mastery .AverageMastery}}</td><td>{{date .LastReviewAt}}</td><td>{{range .AttentionSignals}}<p>{{signal .}}</p>{{end}}</td></tr>{{else}}<tr><td colspan="6">No enrollments on this page.</td></tr>{{end}}</tbody></table>
{{if .NextAfter}}<a href="/console/progress?cohort_id={{.Cohort.CohortID}}&amp;after={{.NextAfter}}&amp;concept_after={{$.ConceptAfter}}">Next learners</a>{{end}}</section>
<section class="scroll"><h2>Concept observations</h2><p>Only active enrollments contribute. Averages require at least {{.MinimumContributors}} distinct learners with recorded reviews for that concept.</p><table><thead><tr><th>Concept</th><th>Observed learners</th><th>Average mastery estimate</th></tr></thead><tbody>
{{range .Concepts}}<tr><th scope="row">{{.Label}}</th><td>{{.ObservedLearners}}</td><td>{{if .SuppressionReason}}Insufficient observed learners{{else}}{{mastery .AverageMastery}}{{end}}</td></tr>{{else}}<tr><td colspan="3">No concepts on this page.</td></tr>{{end}}</tbody></table>
{{if .NextConceptAfter}}<a href="/console/progress?cohort_id={{.Cohort.CohortID}}&amp;after={{$.After}}&amp;concept_after={{.NextConceptAfter}}">Next concepts</a>{{end}}</section>
<p class="muted">Read at {{.GeneratedAt.Format "2006-01-02 15:04 UTC"}}. Follow-up signals invite a conversation; they do not explain the cause of a learner's situation.</p>
{{end}}{{else}}
<section class="scroll"><h2>Your cohorts</h2><table><thead><tr><th>Formation / cohort</th><th>Version</th><th>Status</th><th>Active / total enrollments</th></tr></thead><tbody>{{range .Cohorts.Items}}<tr><th scope="row"><a href="/console/progress?cohort_id={{.CohortID}}">{{.FormationName}} — {{.CohortName}}</a></th><td>{{.Version}}</td><td>{{.CohortStatus}}</td><td>{{.ActiveCount}} / {{.EnrollmentCount}}</td></tr>{{else}}<tr><td colspan="4">No published cohorts are available in your current assignments.</td></tr>{{end}}</tbody></table>{{if .Cohorts.NextAfter}}<a href="/console/progress?after={{.Cohorts.NextAfter}}">Next cohorts</a>{{end}}</section>
{{end}}
<details><summary>Discuss progress with your AI assistant</summary><p>Connect your AI client to <code>{{.MCPURL}}</code>, sign in with your staff account and allow progress reading when requested. Ask it to summarize a cohort or follow an individual enrollment. Your assistant receives learner identities and authorized progress, so use a client approved by your institution.</p><p>The assistant writes the synthesis from recorded observations. Ask it to cite concepts and dates and distinguish missing evidence from difficulties. No AI synthesis is generated or saved by this dashboard.</p></details>
</body></html>`))
