// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import (
	"testing"
	"time"
)

func validAvailabilityForTest() *Availability {
	return &Availability{
		LearnerID:              "L1",
		Timezone:               "America/New_York",
		WindowsJSON:            `[{"day":"Sunday","start":"01:00","end":"03:30"}]`,
		AvgDuration:            30,
		SessionsWeek:           3,
		NotificationConsent:    true,
		NotificationFrequency:  NotificationFrequencyAsScheduled,
		MaxNotificationsPerDay: 2,
		AccessibilityJSON:      `{}`,
	}
}

func TestAvailabilityRejectsInvalidTimezoneAndOverlappingWindows(t *testing.T) {
	a := validAvailabilityForTest()
	a.Timezone = "../etc/passwd"
	if err := a.NormalizeAndValidate(); err == nil {
		t.Fatal("expected invalid timezone to be rejected")
	}

	a = validAvailabilityForTest()
	a.WindowsJSON = `[
		{"day":"Mon","start":"09:00","end":"11:00"},
		{"day":"monday","start":"10:30","end":"12:00"}
	]`
	if err := a.NormalizeAndValidate(); err == nil {
		t.Fatal("expected overlapping windows to be rejected")
	}

	a = validAvailabilityForTest()
	a.AccessibilityJSON = `{"avoid_emojis":true,"typo":true}`
	if err := a.NormalizeAndValidate(); err == nil {
		t.Fatal("expected unknown accessibility field to be rejected")
	}
}

func TestAvailabilityWindowsFollowDSTCivilTime(t *testing.T) {
	a := validAvailabilityForTest()
	// Spring-forward day: both real instants map inside the same civil window,
	// even though 02:xx never exists locally.
	for _, at := range []time.Time{
		time.Date(2026, time.March, 8, 6, 45, 0, 0, time.UTC), // 01:45 EST
		time.Date(2026, time.March, 8, 7, 15, 0, 0, time.UTC), // 03:15 EDT
	} {
		allowed, err := a.AllowsNotificationAt(at)
		if err != nil || !allowed {
			t.Fatalf("spring DST instant %s: allowed=%v err=%v", at, allowed, err)
		}
	}
	bounds, err := a.NotificationBounds(time.Date(2026, time.March, 8, 7, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got := bounds.DayEnd.Sub(bounds.DayStart); got != 23*time.Hour {
		t.Fatalf("spring-forward local day = %s, want 23h", got)
	}

	// Fall-back day: both occurrences of 01:30 are inside the same window and
	// the local civil day spans 25 real hours.
	a.WindowsJSON = `[{"day":"sunday","start":"01:00","end":"02:00"}]`
	for _, at := range []time.Time{
		time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC), // 01:30 EDT
		time.Date(2026, time.November, 1, 6, 30, 0, 0, time.UTC), // 01:30 EST
	} {
		allowed, err := a.AllowsNotificationAt(at)
		if err != nil || !allowed {
			t.Fatalf("fall DST instant %s: allowed=%v err=%v", at, allowed, err)
		}
	}
	bounds, err = a.NotificationBounds(time.Date(2026, time.November, 1, 6, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got := bounds.DayEnd.Sub(bounds.DayStart); got != 25*time.Hour {
		t.Fatalf("fall-back local day = %s, want 25h", got)
	}
}

func TestAvailabilityConsentAndDNDOverrideOpenWindows(t *testing.T) {
	a := validAvailabilityForTest()
	a.WindowsJSON = `[]`
	at := time.Date(2026, time.May, 4, 12, 0, 0, 0, time.UTC)
	a.NotificationConsent = false
	if allowed, err := a.AllowsNotificationAt(at); err != nil || allowed {
		t.Fatalf("without consent: allowed=%v err=%v", allowed, err)
	}
	a.NotificationConsent = true
	a.DoNotDisturb = true
	if allowed, err := a.AllowsNotificationAt(at); err != nil || allowed {
		t.Fatalf("with DND: allowed=%v err=%v", allowed, err)
	}
}
