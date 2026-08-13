// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteArchiveExclusiveUsesPrivateCreateOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "tenant.json")
	if err := writeArchiveExclusive(path, []byte("archive")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive permissions=%#o", info.Mode().Perm())
	}
	if err := writeArchiveExclusive(path, []byte("overwrite")); err == nil {
		t.Fatal("archive was overwritten")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "archive" {
		t.Fatalf("content=%q err=%v", content, err)
	}
}

func TestArchiveDashStreamsWithoutMixingMetadata(t *testing.T) {
	var output bytes.Buffer
	if err := writeArchiveDestination("-", []byte("archive\n"), &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "archive\n" {
		t.Fatalf("stream=%q", output.String())
	}
	decoded, err := readArchiveSource("-", bytes.NewBufferString(output.String()))
	if err != nil || string(decoded) != "archive\n" {
		t.Fatalf("decoded=%q err=%v", decoded, err)
	}
}
