// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import "testing"

func TestLoadInstitutionAccountConfigRequiresExplicitPolicy(t *testing.T) {
	cases := []struct {
		signup, plan string
		wantErr      bool
		wantMode     string
	}{
		{signup: "", wantErr: true},
		{signup: "public", wantErr: true},
		{signup: "open", wantErr: true},
		{signup: "open", plan: "bad plan", wantErr: true},
		{signup: "Open", plan: "plan_trial", wantMode: "open"},
		{signup: "operator", wantMode: "operator"},
		{signup: "operator", plan: "plan_trial", wantErr: true},
	}
	for _, tc := range cases {
		t.Setenv("INSTITUTION_SIGNUP", tc.signup)
		t.Setenv("SIGNUP_PLAN", tc.plan)
		cfg, err := loadInstitutionAccountConfig()
		if (err != nil) != tc.wantErr {
			t.Fatalf("signup=%q plan=%q: err=%v, wantErr=%v", tc.signup, tc.plan, err, tc.wantErr)
		}
		if err == nil && cfg.SignupMode != tc.wantMode {
			t.Fatalf("signup=%q: mode=%q, want %q", tc.signup, cfg.SignupMode, tc.wantMode)
		}
	}
}
