// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	institutionSignupOpen     = "open"
	institutionSignupOperator = "operator"
)

var planIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type institutionAccountConfig struct {
	SignupMode string
	SignupPlan string
}

// loadInstitutionAccountConfig reads the institution signup policy. The
// institution profile requires an explicit choice: "open" lets anyone create
// an institution from /signup, "operator" keeps creation to tutor-control-plane.
func loadInstitutionAccountConfig() (institutionAccountConfig, error) {
	cfg := institutionAccountConfig{
		SignupMode: strings.ToLower(strings.TrimSpace(os.Getenv("INSTITUTION_SIGNUP"))),
		SignupPlan: strings.TrimSpace(os.Getenv("SIGNUP_PLAN")),
	}
	switch cfg.SignupMode {
	case institutionSignupOpen:
		if !planIDPattern.MatchString(cfg.SignupPlan) {
			return institutionAccountConfig{}, fmt.Errorf("INSTITUTION_SIGNUP=open requires SIGNUP_PLAN, the plan id given to new institutions")
		}
	case institutionSignupOperator:
		if cfg.SignupPlan != "" {
			return institutionAccountConfig{}, fmt.Errorf("SIGNUP_PLAN is only used with INSTITUTION_SIGNUP=open")
		}
	case "":
		return institutionAccountConfig{}, fmt.Errorf("INSTITUTION_SIGNUP is required with --profile institution (want open or operator)")
	default:
		return institutionAccountConfig{}, fmt.Errorf("unknown INSTITUTION_SIGNUP %q (want open or operator)", cfg.SignupMode)
	}
	return cfg, nil
}
