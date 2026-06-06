// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
)

func TestShouldPrintVersion(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-version"}, {"version"}} {
		if !shouldPrintVersion(args) {
			t.Fatalf("shouldPrintVersion(%v) = false", args)
		}
	}
	if shouldPrintVersion(nil) || shouldPrintVersion([]string{"--version", "extra"}) || shouldPrintVersion([]string{"--help"}) {
		t.Fatal("shouldPrintVersion accepted an unsupported argument shape")
	}
}

func TestVersionLine(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := version, commit, buildDate
	defer func() {
		version, commit, buildDate = oldVersion, oldCommit, oldBuildDate
	}()
	version = "v0.4.0"
	commit = "abc1234"
	buildDate = "2026-05-14T16:00:00Z"

	got := versionLine()
	for _, want := range []string{"tutor-mcp", "v0.4.0", "abc1234", "2026-05-14T16:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("versionLine() = %q, missing %q", got, want)
		}
	}
	if mcpVersion() != "0.4.0" {
		t.Fatalf("mcpVersion() = %q", mcpVersion())
	}
}
