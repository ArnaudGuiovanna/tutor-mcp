// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import "testing"

// TestReplaceMarkdownSection exercises the hand-rolled markdown section
// replacer directly, asserting exact output for each shape. This function is
// the continuity core: it is how MEMORY.md sections are surgically rewritten.
func TestReplaceMarkdownSection(t *testing.T) {
	tests := []struct {
		name    string
		current string
		key     string
		content string
		want    string
	}{
		{
			name:    "empty document returns only the new section",
			current: "",
			key:     "Current state",
			content: "Fresh start.",
			want:    "## Current state\nFresh start.\n",
		},
		{
			name:    "whitespace-only document returns only the new section",
			current: "  \n\t\n",
			key:     "Current state",
			content: "Fresh start.",
			want:    "## Current state\nFresh start.\n",
		},
		{
			name: "replace section in the middle keeps neighbours intact",
			current: "## Alpha\n" +
				"alpha body\n" +
				"## Target\n" +
				"old target body\n" +
				"## Omega\n" +
				"omega body\n",
			key:     "Target",
			content: "new target body",
			want: "## Alpha\n" +
				"alpha body\n" +
				"## Target\n" +
				"new target body\n" +
				"## Omega\n" +
				"omega body\n",
		},
		{
			name: "replace the last section consumes to end of file",
			current: "## Alpha\n" +
				"alpha body\n" +
				"## Target\n" +
				"old line one\n" +
				"old line two\n",
			key:     "Target",
			content: "replaced",
			want: "## Alpha\n" +
				"alpha body\n" +
				"## Target\n" +
				"replaced\n",
		},
		{
			name: "replace the first section keeps following sections",
			current: "## Target\n" +
				"old\n" +
				"## Beta\n" +
				"beta body\n",
			key:     "Target",
			content: "fresh",
			want: "## Target\n" +
				"fresh\n" +
				"## Beta\n" +
				"beta body\n",
		},
		{
			name: "missing section is appended after a blank separator",
			current: "## Alpha\n" +
				"alpha body\n",
			key:     "Brand new",
			content: "appended body",
			want: "## Alpha\n" +
				"alpha body\n" +
				"\n" +
				"## Brand new\n" +
				"appended body\n",
		},
		{
			name:    "missing section on document without trailing newline still appends",
			current: "## Alpha\nalpha body",
			key:     "Brand new",
			content: "appended body",
			want: "## Alpha\n" +
				"alpha body\n" +
				"\n" +
				"## Brand new\n" +
				"appended body\n",
		},
		{
			name:    "key is trimmed before building the heading",
			current: "",
			key:     "   Spaced   ",
			content: "body",
			want:    "## Spaced\nbody\n",
		},
		{
			name:    "trailing newlines in content are normalised to one",
			current: "",
			key:     "Notes",
			content: "line\n\n\n",
			want:    "## Notes\nline\n",
		},
		{
			name: "subsection (### ) inside the section is preserved, not treated as a boundary",
			current: "## Target\n" +
				"intro\n" +
				"### Sub\n" +
				"sub body\n" +
				"## After\n" +
				"after body\n",
			key:     "Target",
			content: "replaced wholesale",
			want: "## Target\n" +
				"replaced wholesale\n" +
				"## After\n" +
				"after body\n",
		},
		{
			name: "multiple headings: only the first matching heading is replaced",
			current: "## Target\n" +
				"first body\n" +
				"## Other\n" +
				"other body\n" +
				"## Target\n" +
				"second body\n",
			key:     "Target",
			content: "replaced",
			want: "## Target\n" +
				"replaced\n" +
				"## Other\n" +
				"other body\n" +
				"## Target\n" +
				"second body\n",
		},
		{
			name: "replacement body may itself contain heading-looking lines verbatim",
			current: "## Target\n" +
				"old\n" +
				"## After\n" +
				"after body\n",
			key: "Target",
			content: "```md\n" +
				"## not a real heading\n" +
				"```",
			want: "## Target\n" +
				"```md\n" +
				"## not a real heading\n" +
				"```\n" +
				"## After\n" +
				"after body\n",
		},
		{
			name: "documents existing-behaviour: an indented heading-like line in a code fence " +
				"IS treated as a boundary because the scanner trims whitespace before matching",
			current: "## Target\n" +
				"intro\n" +
				"```md\n" +
				"  ## looks like a heading\n" +
				"```\n" +
				"tail\n",
			key:     "Target",
			content: "replaced",
			// The scanner trims leading whitespace, so the fenced "## looks like a heading"
			// line is mistaken for a section boundary and everything from it onward is kept.
			want: "## Target\n" +
				"replaced\n" +
				"  ## looks like a heading\n" +
				"```\n" +
				"tail\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := replaceMarkdownSection(tc.current, tc.key, tc.content)
			if got != tc.want {
				t.Fatalf("replaceMarkdownSection mismatch\n--- got ---\n%q\n--- want ---\n%q", got, tc.want)
			}
		})
	}
}
