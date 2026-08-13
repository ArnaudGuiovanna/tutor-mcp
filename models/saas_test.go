// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package models

import "testing"

func TestControlPlanePrincipalRequiresCanonicalAuditFields(t *testing.T) {
	valid := ControlPlanePrincipal{ActorID: "operator", Roles: []string{RolePlatformAdmin},
		Reason: "approved change", RequestID: "CHG-1"}
	if !valid.Validate() {
		t.Fatal("canonical control-plane principal rejected")
	}
	for name, mutate := range map[string]func(*ControlPlanePrincipal){
		"request absent": func(p *ControlPlanePrincipal) { p.RequestID = "" },
		"reason padded":  func(p *ControlPlanePrincipal) { p.Reason = " approved change" },
		"actor padded":   func(p *ControlPlanePrincipal) { p.ActorID += " " },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if candidate.Validate() {
				t.Fatal("invalid audit authority accepted")
			}
		})
	}
}
