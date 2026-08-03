// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	NotificationFrequencyAsScheduled = "as_scheduled"
	NotificationFrequencyDaily       = "daily"
	NotificationFrequencyWeekly      = "weekly"
)

// AccessibilityPreferences contains accommodations that can be honoured
// deterministically by the runtime and surfaced to the tutor. The scheduler
// uses ScreenReaderOptimized and AvoidEmojis when rendering webhook embeds;
// the remaining preferences guide learner-facing activity composition.
type AccessibilityPreferences struct {
	ScreenReaderOptimized bool `json:"screen_reader_optimized,omitempty"`
	AvoidEmojis           bool `json:"avoid_emojis,omitempty"`
	PreferPlainLanguage   bool `json:"prefer_plain_language,omitempty"`
	ExtraProcessingTime   bool `json:"extra_processing_time,omitempty"`
}

// Availability is the learner-owned source of truth for proactive contact.
// WindowsJSON and AccessibilityJSON are kept as canonical JSON at the storage
// boundary so SQLite and PostgreSQL expose the same schema and validation.
type Availability struct {
	LearnerID              string
	Timezone               string
	WindowsJSON            string
	AvgDuration            int
	SessionsWeek           int
	DoNotDisturb           bool
	NotificationConsent    bool
	NotificationFrequency  string
	MaxNotificationsPerDay int
	AccessibilityJSON      string
	Version                int
	UpdatedAt              time.Time
}

// NotificationBounds are UTC instants derived from the learner's local civil
// day/week. AddDate (rather than +24h) makes the boundaries correct across DST.
type NotificationBounds struct {
	DayStart       time.Time
	DayEnd         time.Time
	FrequencyStart time.Time
	FrequencyEnd   time.Time
	OnePerPeriod   bool
}

func DefaultAvailability(learnerID string) *Availability {
	return &Availability{
		LearnerID:              learnerID,
		Timezone:               "UTC",
		WindowsJSON:            "[]",
		AvgDuration:            30,
		SessionsWeek:           3,
		NotificationFrequency:  NotificationFrequencyDaily,
		MaxNotificationsPerDay: 1,
		AccessibilityJSON:      "{}",
	}
}

func (a *Availability) Windows() ([]TimeWindow, error) {
	if a == nil {
		return nil, fmt.Errorf("availability is required")
	}
	var windows []TimeWindow
	if err := decodeStrictJSON(defaultJSON(a.WindowsJSON, "[]"), &windows); err != nil {
		return nil, fmt.Errorf("preferred_windows: %w", err)
	}
	if windows == nil {
		windows = []TimeWindow{}
	}
	return windows, nil
}

func (a *Availability) Accessibility() (AccessibilityPreferences, error) {
	if a == nil {
		return AccessibilityPreferences{}, fmt.Errorf("availability is required")
	}
	var prefs AccessibilityPreferences
	if err := decodeStrictJSON(defaultJSON(a.AccessibilityJSON, "{}"), &prefs); err != nil {
		return AccessibilityPreferences{}, fmt.Errorf("accessibility_preferences: %w", err)
	}
	return prefs, nil
}

// NormalizeAndValidate canonicalizes JSON and civil-time values before they
// cross the persistence boundary. Unknown JSON fields are rejected so a typo
// cannot silently disable an accommodation or notification constraint.
func (a *Availability) NormalizeAndValidate() error {
	if a == nil {
		return fmt.Errorf("availability is required")
	}
	if strings.TrimSpace(a.LearnerID) == "" {
		return fmt.Errorf("learner_id is required")
	}
	a.Timezone = strings.TrimSpace(a.Timezone)
	if a.Timezone == "" {
		a.Timezone = "UTC"
	}
	if len(a.Timezone) > 128 || a.Timezone == "Local" || strings.Contains(a.Timezone, "..") || strings.HasPrefix(a.Timezone, "/") {
		return fmt.Errorf("timezone must be a valid IANA timezone")
	}
	if _, err := time.LoadLocation(a.Timezone); err != nil {
		return fmt.Errorf("timezone must be a valid IANA timezone: %w", err)
	}
	if a.AvgDuration < 5 || a.AvgDuration > 240 {
		return fmt.Errorf("avg_session_duration_minutes must be between 5 and 240")
	}
	if a.SessionsWeek < 1 || a.SessionsWeek > 21 {
		return fmt.Errorf("sessions_per_week must be between 1 and 21")
	}
	if a.NotificationFrequency == "" {
		a.NotificationFrequency = NotificationFrequencyDaily
	}
	switch a.NotificationFrequency {
	case NotificationFrequencyAsScheduled, NotificationFrequencyDaily, NotificationFrequencyWeekly:
	default:
		return fmt.Errorf("notification_frequency must be as_scheduled, daily, or weekly")
	}
	if a.MaxNotificationsPerDay == 0 {
		a.MaxNotificationsPerDay = 1
	}
	if a.MaxNotificationsPerDay < 1 || a.MaxNotificationsPerDay > 10 {
		return fmt.Errorf("max_notifications_per_day must be between 1 and 10")
	}

	windows, err := a.Windows()
	if err != nil {
		return err
	}
	for i := range windows {
		day, ok := canonicalWeekday(windows[i].Day)
		if !ok {
			return fmt.Errorf("preferred_windows[%d].day must be a weekday name", i)
		}
		windows[i].Day = day
		start, err := parseClock(windows[i].Start)
		if err != nil {
			return fmt.Errorf("preferred_windows[%d].start: %w", i, err)
		}
		end, err := parseClock(windows[i].End)
		if err != nil {
			return fmt.Errorf("preferred_windows[%d].end: %w", i, err)
		}
		if start >= end {
			return fmt.Errorf("preferred_windows[%d] must end after it starts; split overnight windows across two days", i)
		}
		windows[i].Start = formatClock(start)
		windows[i].End = formatClock(end)
	}
	sort.Slice(windows, func(i, j int) bool {
		di, _ := canonicalWeekdayIndex(windows[i].Day)
		dj, _ := canonicalWeekdayIndex(windows[j].Day)
		if di != dj {
			return di < dj
		}
		return windows[i].Start < windows[j].Start
	})
	for i := 1; i < len(windows); i++ {
		if windows[i-1].Day == windows[i].Day && windows[i].Start < windows[i-1].End {
			return fmt.Errorf("preferred_windows on %s must not overlap", windows[i].Day)
		}
	}
	encodedWindows, err := json.Marshal(windows)
	if err != nil {
		return fmt.Errorf("encode preferred_windows: %w", err)
	}
	a.WindowsJSON = string(encodedWindows)

	prefs, err := a.Accessibility()
	if err != nil {
		return err
	}
	encodedPrefs, err := json.Marshal(prefs)
	if err != nil {
		return fmt.Errorf("encode accessibility_preferences: %w", err)
	}
	a.AccessibilityJSON = string(encodedPrefs)
	return nil
}

// AllowsNotificationAt applies consent, DND and weekly local-time windows.
// An empty window list means any local time is acceptable; consent is still
// explicit and defaults to false.
func (a *Availability) AllowsNotificationAt(at time.Time) (bool, error) {
	if err := a.NormalizeAndValidate(); err != nil {
		return false, err
	}
	if !a.NotificationConsent || a.DoNotDisturb {
		return false, nil
	}
	windows, err := a.Windows()
	if err != nil {
		return false, err
	}
	if len(windows) == 0 {
		return true, nil
	}
	loc, _ := time.LoadLocation(a.Timezone)
	local := at.In(loc)
	day := strings.ToLower(local.Weekday().String())
	minute := local.Hour()*60 + local.Minute()
	for _, window := range windows {
		if window.Day != day {
			continue
		}
		start, _ := parseClock(window.Start)
		end, _ := parseClock(window.End)
		if minute >= start && minute < end {
			return true, nil
		}
	}
	return false, nil
}

func (a *Availability) NotificationBounds(at time.Time) (NotificationBounds, error) {
	if err := a.NormalizeAndValidate(); err != nil {
		return NotificationBounds{}, err
	}
	loc, _ := time.LoadLocation(a.Timezone)
	local := at.In(loc)
	dayStartLocal := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	dayEndLocal := dayStartLocal.AddDate(0, 0, 1)
	bounds := NotificationBounds{
		DayStart:       dayStartLocal.UTC(),
		DayEnd:         dayEndLocal.UTC(),
		FrequencyStart: dayStartLocal.UTC(),
		FrequencyEnd:   dayEndLocal.UTC(),
	}
	switch a.NotificationFrequency {
	case NotificationFrequencyDaily:
		bounds.OnePerPeriod = true
	case NotificationFrequencyWeekly:
		daysSinceMonday := (int(dayStartLocal.Weekday()) + 6) % 7
		weekStart := dayStartLocal.AddDate(0, 0, -daysSinceMonday)
		bounds.FrequencyStart = weekStart.UTC()
		bounds.FrequencyEnd = weekStart.AddDate(0, 0, 7).UTC()
		bounds.OnePerPeriod = true
	}
	return bounds, nil
}

func decodeStrictJSON(raw string, dst any) error {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func defaultJSON(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func canonicalWeekday(value string) (string, bool) {
	day, ok := canonicalWeekdayIndex(value)
	if !ok {
		return "", false
	}
	return [...]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}[day], true
}

func canonicalWeekdayIndex(value string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sun", "sunday":
		return int(time.Sunday), true
	case "mon", "monday":
		return int(time.Monday), true
	case "tue", "tues", "tuesday":
		return int(time.Tuesday), true
	case "wed", "wednesday":
		return int(time.Wednesday), true
	case "thu", "thur", "thurs", "thursday":
		return int(time.Thursday), true
	case "fri", "friday":
		return int(time.Friday), true
	case "sat", "saturday":
		return int(time.Saturday), true
	default:
		return 0, false
	}
}

func parseClock(value string) (int, error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, fmt.Errorf("must use HH:MM (24-hour local time)")
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, fmt.Errorf("must use HH:MM (24-hour local time)")
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func formatClock(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}
